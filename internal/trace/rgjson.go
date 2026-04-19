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
//   - identify.go  — 2-phase "which pattern matched which line" logic
//   - reconstruct.go — cache-hit material (line text, submatches)
//   - cache.go     — on-disk cache (Python-compatible JSON)
package trace

import (
	"bufio"
	"context"
	"encoding/json"
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
// ripgrep wraps strings this way to leave room for future encoding
// variants (e.g. `{"bytes": "..."}` for non-UTF-8 payloads).
//
// When a line contains bytes that are not valid UTF-8, ripgrep emits
// `{"bytes": "<base64>"}` instead of `{"text": ...}`. We capture both
// via custom UnmarshalJSON so the caller gets a string either way.
type RgText struct {
	Text string
}

// UnmarshalJSON handles both `{"text": "..."}` (UTF-8 happy path) and
// `{"bytes": "<base64>"}` (non-UTF-8 fallback). The base64 payload is
// decoded and stored in Text verbatim; downstream code does lossy
// UTF-8 interpretation if needed, which is what Python also does.
func (t *RgText) UnmarshalJSON(data []byte) error {
	// Fast path: null.
	if string(data) == "null" {
		t.Text = ""
		return nil
	}
	var raw struct {
		Text  *string `json:"text"`
		Bytes *string `json:"bytes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Text != nil {
		t.Text = *raw.Text
		return nil
	}
	if raw.Bytes != nil {
		// Decoding base64 is deferred to the caller if they need the
		// actual bytes; we keep the raw string so the event is still
		// routable by type. Python's rg_json.py does the same.
		t.Text = *raw.Bytes
		return nil
	}
	return nil
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
type RgMatchData struct {
	Path           RgPath       `json:"path"`
	Lines          RgText       `json:"lines"`
	LineNumber     int          `json:"line_number"`
	AbsoluteOffset int64        `json:"absolute_offset"`
	Submatches     []RgSubmatch `json:"submatches"`
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

// ParseEvent parses a single JSON line from `rg --json` stdout.
//
// Empty lines and `null` produce (nil, nil) so a straight-line reader
// loop doesn't have to special-case whitespace. Malformed JSON returns
// an error; unknown event types return ErrUnknownEvent so the caller
// can tell "skip" from "fatal".
//
// Parity note: Python's parse_rg_json_event returns None for BOTH
// malformed JSON and unknown events, and logs a warning. rx-go is
// stricter — we surface malformed JSON as an error so test failures
// point at the real problem instead of getting swallowed.
func ParseEvent(line []byte) (*RgEvent, error) {
	// Trim leading/trailing whitespace — rg only uses '\n', but be
	// defensive against test stubs that inject '\r\n'.
	start, end := 0, len(line)
	for start < end && (line[start] == ' ' || line[start] == '\t' || line[start] == '\r' || line[start] == '\n') {
		start++
	}
	for end > start && (line[end-1] == ' ' || line[end-1] == '\t' || line[end-1] == '\r' || line[end-1] == '\n') {
		end--
	}
	if start == end {
		return nil, nil
	}
	trimmed := line[start:end]
	if string(trimmed) == "null" {
		return nil, nil
	}

	// Peek at the `type` field first so we only unmarshal the correct
	// data shape. This avoids overhead of repeated reflection in the
	// encoding/json code paths for the large stats payloads.
	var envelope struct {
		Type RgEventType     `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, err
	}
	ev := &RgEvent{Type: envelope.Type}

	switch envelope.Type {
	case RgEventBegin:
		var d RgBeginData
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, err
		}
		ev.Begin = &d
	case RgEventMatch:
		var d RgMatchData
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, err
		}
		ev.Match = &d
	case RgEventContext:
		var d RgContextData
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, err
		}
		ev.Context = &d
	case RgEventEnd:
		var d RgEndData
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, err
		}
		ev.End = &d
	case RgEventSummary:
		var d RgSummaryData
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, err
		}
		ev.Summary = &d
	default:
		// Surface unknown types so callers can choose to ignore them
		// without paying the cost of pretending it was a normal parse.
		return ev, ErrUnknownEvent
	}
	return ev, nil
}

// StreamEvents reads r line-by-line and returns events one at a time
// via a callback. The callback receives each successfully-parsed event
// and an error for parsing failures. Return a non-nil error from the
// callback to stop iteration early (it will propagate up).
//
// Uses bufio.Scanner with a custom buffer size so lines longer than
// 64 KB (the default limit) don't truncate on very long matched lines.
// Python's file-backed reader has no fixed line limit, so we bump ours
// to 16 MB, which is way above anything rg emits for real log lines.
func StreamEvents(ctx context.Context, r io.Reader, cb func(*RgEvent, error) error) error {
	scanner := bufio.NewScanner(r)
	// Python's default reader has no line limit; we match this in
	// spirit with a generous 16 MB cap. A log line bigger than this is
	// pathological anyway — ripgrep caps single-line search at 10 MB
	// by default, so we stay just above that.
	const maxLine = 16 * 1024 * 1024
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, maxLine)

	for scanner.Scan() {
		// Check cancellation between lines. The caller can bail out at
		// any time without waiting for the subprocess to close stdout.
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, err := ParseEvent(scanner.Bytes())
		// Swallow ErrUnknownEvent unless the callback cares — the
		// default sentinel behavior is "skip unknowns", so we
		// pass (ev, nil) in that case, matching Python's semantics.
		if errors.Is(err, ErrUnknownEvent) {
			err = nil
		}
		if ev == nil && err == nil {
			// Empty/blank line — nothing to do.
			continue
		}
		if cbErr := cb(ev, err); cbErr != nil {
			return cbErr
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}
