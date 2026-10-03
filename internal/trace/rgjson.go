// Package trace implements the multi-pattern, multi-file search engine
// for rx-go. It orchestrates ripgrep subprocesses, manages chunked
// parallel scans, and merges results into the single response shape
// consumed by both the CLI and the HTTP layer.
//
// The trace package is split across several files by concern:
//
//   - rgjson.go    — types and parser for ripgrep's --json event stream
//   - chunker.go   — computes byte offsets + byte counts per chunk
//   - worker.go    — per-chunk subprocess execution and match filtering
//   - engine.go    — top-level orchestrator (parity: trace.parse_paths)
//   - compressed.go — non-seekable compressed path (gzip/xz/bz2)
//   - seekable.go  — seekable-zstd parallel frame path
//   - credit.go    — which patterns match each matched line, by ripgrep
//   - reconstruct.go — cache-hit material (line text, submatches)
//   - cache.go     — on-disk cache (Python-compatible JSON)
package trace

import (
	"bytes"
	"context"
	"errors"
	"io"
)

// ripgrep emits newline-delimited JSON on stdout when invoked with
// --json. Each line is one "event" describing a stage of the search.
//
// We only model the subset of events rx-go cares about. Unknown event
// types are ignored silently, matching Python's rg_json.py behavior
// (it returns nil for unrecognized `type`).

// ============================================================================
// Enumerations
// ============================================================================

// RgEventType is the discriminator in ripgrep's JSON stream.
type RgEventType string

// Known ripgrep event types. Strings are from ripgrep 13+ / 14+ docs.
const (
	RgEventBegin   RgEventType = "begin"
	RgEventMatch   RgEventType = "match"
	RgEventContext RgEventType = "context"
	RgEventEnd     RgEventType = "end"
	RgEventSummary RgEventType = "summary"
)

// ============================================================================
// Primitive wrappers — mirror Python's {"text": "..."} shape
// ============================================================================

// RgText models ripgrep's `{"text": "..."}` object that wraps strings.
//
// When a line contains bytes that are not valid UTF-8, ripgrep emits
// `{"bytes": "<base64>"}` instead of `{"text": ...}`. The parser decodes
// the base64, so Text holds the line's own bytes in both cases: of the
// bytes kept, which are all of them unless Truncated. A Go string may
// hold bytes that are not valid UTF-8; encoding/json writes each such
// byte as U+FFFD, which is how samples and a trace-cache hit, reading
// the same bytes from the file, show the line too.
type RgText struct {
	Text string
	// Size is the payload's length in bytes as ripgrep read it: the
	// length of the text for a UTF-8 payload, and of the decoded bytes for
	// a base64 one. For a line it counts the line break too, so the line's
	// first byte plus Size is where the next line starts. It is the whole
	// payload's length even when Text holds only part of it.
	Size int
	// Truncated is true when Text holds only the first bytes of the
	// payload: a line longer than RX_MAX_LINE_TEXT_BYTES, or a submatch
	// that runs past the end of such a cut line. A line's text that is
	// cut never ends with its line break.
	Truncated bool
}

// RgPath models `{"text": "/path/to/file"}` or null for stdin.
// Reuse RgText for the same {text|bytes} handling.
type RgPath = RgText

// ============================================================================
// Submatch
// ============================================================================

// RgSubmatch is one regex capture inside a matching line. Byte offsets
// are into the line text, not the file.
type RgSubmatch struct {
	Match RgText `json:"match"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Text is a convenience accessor mirroring Python's RgSubmatch.text
// property. Lets the rest of the codebase call sm.Text() without
// caring about the {text|bytes} wrapper.
func (sm RgSubmatch) Text() string { return sm.Match.Text }

// ============================================================================
// Event data payloads
// ============================================================================

// RgBeginData is the payload of a begin event (file-scan started).
type RgBeginData struct {
	Path RgPath `json:"path"`
}

// RgMatchData is the payload of a match event.
//
// AbsoluteOffset is the byte offset into ripgrep's INPUT stream — which
// is NOT necessarily the same as the byte offset in the source file.
// When the caller uses ReadAt to feed a chunk via stdin, the worker
// must translate AbsoluteOffset back into file coordinates before
// deduplication. See worker.go.
//
// Submatches holds at most RX_MAX_SUBMATCHES_PER_LINE entries and, when
// Lines is truncated, only those that start inside Lines.Text;
// SubmatchesTruncated is true when any were left out.
type RgMatchData struct {
	Path                RgPath       `json:"path"`
	Lines               RgText       `json:"lines"`
	LineNumber          int          `json:"line_number"`
	AbsoluteOffset      int64        `json:"absolute_offset"`
	Submatches          []RgSubmatch `json:"submatches"`
	SubmatchesTruncated bool         `json:"-"`
}

// RgContextData is the payload of a context event (-A/-B/-C flags).
// Same shape as match events except Submatches is always empty.
type RgContextData struct {
	Path           RgPath       `json:"path"`
	Lines          RgText       `json:"lines"`
	LineNumber     int          `json:"line_number"`
	AbsoluteOffset int64        `json:"absolute_offset"`
	Submatches     []RgSubmatch `json:"submatches"`
}

// RgElapsed mirrors rg's {"secs": int, "nanos": int, "human": string}.
// Not parsed into time.Duration because Python doesn't either.
type RgElapsed struct {
	Secs  int64  `json:"secs"`
	Nanos int64  `json:"nanos"`
	Human string `json:"human"`
}

// RgStats is the statistics payload attached to end/summary events.
// We parse it for completeness (tests pin field presence) but rx-go's
// engine doesn't currently consume the numbers.
type RgStats struct {
	Elapsed           RgElapsed `json:"elapsed"`
	Searches          int       `json:"searches"`
	SearchesWithMatch int       `json:"searches_with_match"`
	BytesSearched     int64     `json:"bytes_searched"`
	BytesPrinted      int64     `json:"bytes_printed"`
	MatchedLines      int64     `json:"matched_lines"`
	Matches           int64     `json:"matches"`
}

// RgEndData is the payload of an end event — emitted after a file scan
// completes. BinaryOffset is non-nil when ripgrep bailed early because
// it detected binary content.
type RgEndData struct {
	Path         RgPath  `json:"path"`
	BinaryOffset *int64  `json:"binary_offset"`
	Stats        RgStats `json:"stats"`
}

// RgSummaryData is the payload of a summary event — emitted last,
// aggregating stats across all searched files in the invocation.
type RgSummaryData struct {
	ElapsedTotal RgElapsed `json:"elapsed_total"`
	Stats        RgStats   `json:"stats"`
}

// ============================================================================
// Event envelope
// ============================================================================

// RgEvent is a single parsed rg --json event. At most one of the
// `Begin`, `Match`, `Context`, `End`, `Summary` fields is non-nil; the
// selected field matches the Type.
//
// The design choice of using pointers (rather than a sum-type trick
// or `any`) mirrors how Go packages like encoding/json/decode handle
// variant records — type-switch in caller, zero-pointer comparison
// to detect irrelevant events.
type RgEvent struct {
	Type    RgEventType
	Begin   *RgBeginData
	Match   *RgMatchData
	Context *RgContextData
	End     *RgEndData
	Summary *RgSummaryData
}

// ============================================================================
// Parser
// ============================================================================

// ErrUnknownEvent is returned by ParseEvent when the JSON is valid but
// the `type` field is something we don't model. Callers that want to
// skip unknowns silently can check errors.Is and continue.
var ErrUnknownEvent = errors.New("unknown ripgrep event type")

// ParseEvent parses a single event from `rg --json` stdout, bounded the
// way StreamEvents bounds it (see eventScanner).
//
// Empty input and `null` produce (nil, nil). Output that is not an
// event returns an error wrapping ErrMalformedEvent; an event of a type
// rx does not model returns ErrUnknownEvent beside it, so the caller can
// tell "skip" from "fatal".
//
// Parity note: Python's parse_rg_json_event returns None for BOTH
// malformed JSON and unknown events, and logs a warning. rx-go is
// stricter — we surface malformed JSON as an error so test failures
// point at the real problem instead of getting swallowed.
func ParseEvent(line []byte) (*RgEvent, error) {
	s := newEventScanner(bytes.NewReader(line), currentEventLimits())
	if _, err := s.peekNonSpace(); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	ev, err := s.readEvent()
	if err != nil {
		return ev, err
	}
	if _, err := s.peekNonSpace(); !errors.Is(err, io.EOF) {
		return nil, malformed("more than one value in one event")
	}
	return ev, nil
}

// eventReadBufferSize is the size of the event reader's buffer. It is
// not a limit on an event: strings stream through it and the parts of
// an event rx does not keep are skipped as they pass.
const eventReadBufferSize = 64 * 1024

// StreamEvents reads rg's --json output and returns events one at a
// time via a callback. The callback receives each successfully-parsed
// event, or an error for output that is not an event; return a non-nil
// error from it to stop iteration early (it will propagate up). A
// callback that passes over an error gets the events from the next line
// on.
//
// ripgrep puts no limit on an event: a matched line of any length is
// one event, and every submatch on the line adds an object of about 50
// bytes to it, so a 1 MB line on which the pattern matches each
// character makes an event of about 50 MB. StreamEvents never holds an
// event whole. It keeps at most RX_MAX_LINE_TEXT_BYTES of a line's text
// and RX_MAX_SUBMATCHES_PER_LINE of its submatches, and marks what it
// left out (RgText.Truncated, RgMatchData.SubmatchesTruncated); a line's
// offset, number and size are always exact. The limits are read once,
// when the stream starts.
//
// A caller that stops reading before r ends must also stop the process
// writing r, or that process blocks on a full pipe for ever. ProcessChunk
// and ProcessCompressed kill rg when StreamEvents returns an error.
func StreamEvents(ctx context.Context, r io.Reader, cb func(*RgEvent, error) error) error {
	s := newEventScanner(r, currentEventLimits())
	for {
		if _, err := s.peekNonSpace(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		// Check cancellation between events. The caller can bail out at
		// any time without waiting for the subprocess to close stdout.
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, err := s.readEvent()
		switch {
		case errors.Is(err, ErrUnknownEvent), err == nil && ev == nil:
			// Unknown event types and `null` are skipped, as rx-python
			// skips them.
			continue
		case errors.Is(err, ErrMalformedEvent):
			// Resume at the next line in case the callback goes on.
			s.skipLine()
			if cbErr := cb(nil, err); cbErr != nil {
				return cbErr
			}
			continue
		case err != nil:
			// The reader itself failed.
			return err
		}
		if cbErr := cb(ev, nil); cbErr != nil {
			return cbErr
		}
	}
}
