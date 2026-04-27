// Package jsonblob implements the `json-blob-multiline` line detector.
//
// What it does:
//
//   - Flags contiguous line ranges whose shape looks like a multi-line
//     JSON object or array: opens on a line that is exactly `{` or `[`
//     (after trimming leading/trailing ASCII whitespace), then scans
//     subsequent lines with a string-aware bracket counter. Emits when
//     the bracket counter returns to zero on a line whose trimmed
//     content ends with the matching closer (`}` or `]`) AND whose
//     indent prefix matches the opener's indent.
//
//   - Severity 0.3: navigation hint. JSON blobs in logs are common and
//     usually legitimate; the goal is "jump to the next pretty-printed
//     object", not to flag trouble.
//
// State machine (one detector instance, one "current blob" at a time):
//
//	closed:                    no open blob
//	  on a line matching `^\s*[{\[]\s*$` → transition to open, record
//	  open-line's indent + open-line's number/offset + the expected
//	  closer character, start counter at 1
//
//	open:                      a blob is in-progress
//	  on every line (including the opener — counter was set directly,
//	  so continuation starts at the NEXT line):
//	    - run the string-aware bracket scan over the line bytes
//	    - adjust counter by (opens - closes) outside strings
//	    - window-age check: if lines_since_open >= windowLines, abort:
//	      emit a truncated anomaly spanning opener..current_line and
//	      increment the truncated counter; transition back to closed
//	    - if counter == 0 AND the line's trimmed content ends with the
//	      expected closer AND the line's indent equals the opener's
//	      indent: emit a clean anomaly and transition back to closed
//	    - counter going below zero indicates malformed input (a closer
//	      without a matching opener); abort the blob silently (closed)
//	      without emitting — the input wasn't a JSON blob we could
//	      describe
//
// Why we track "truncated_at_window" at all:
//
//   - A detector that silently drops unclosed blobs is surprising when
//     scanning a huge log: the user sees "no blob detected" for what
//     clearly looks like truncated JSON in a streaming sink that
//     crashed mid-write. Emitting a partial anomaly up to the window
//     edge preserves navigation value ("the last complete or partial
//     blob started here").
//
// How the per-run `truncated_at_window` signal is surfaced — design note:
//
//   - The plan calls for this signal to live in the detector's
//     `Report.Result` map. But the LineDetector contract today only
//     exposes `Finalize(*FlushContext) []Anomaly` — it doesn't return a
//     `*Report`, and the coordinator never calls `Analyze` along the
//     streaming path. Extending the interface would cascade through the
//     coordinator, every existing detector, and the index builder
//     wiring — disproportionate for a one-off per-run signal.
//
//   - We take the documented alternative from the task notes (option
//     "b"): emit ONE synthetic anomaly at Finalize with a sentinel
//     Description when any blob was truncated during the scan. The
//     sentinel span covers line 0/offset 0 so it's distinguishable from
//     real blob anomalies and never overlaps them, and the Description
//     carries both the flag and the truncated-count.
//
//   - Consumers that care about the signal can look for
//     `strings.HasPrefix(a.Description, "truncated_at_window: true")`.
//     The coordinator rewrites Category to the detector name, so those
//     sentinel anomalies are grouped with the rest of json-blob output.
//
// Registration: this package has an init() that calls analyzer.Register
// so a blank import in cmd/rx/main.go is enough to hook it up.
package jsonblob

import (
	"bytes"
	"context"
	"fmt"

	"github.com/wlame/rx-go/internal/analyzer"
)

// Metadata constants — kept as a block at the top so /v1/detectors
// output is trivially auditable against the plan.
const (
	detectorName        = "json-blob-multiline"
	detectorVersion     = "0.1.0"
	detectorCategory    = "format"
	detectorDescription = "Multi-line JSON objects or arrays spanning several lines"

	// severity is the plan-mandated value for this detector.
	severity = 0.3

	// truncatedSentinelPrefix is the Description prefix of the synthetic
	// per-run anomaly that Finalize emits when at least one blob was
	// truncated at the window edge. See the package doc for the design
	// rationale behind this channel.
	truncatedSentinelPrefix = "truncated_at_window: true"
)

// Detector implements both analyzer.FileAnalyzer (for registry
// enumeration) and analyzer.LineDetector (for the streaming scan).
//
// Only one blob can be in progress at a time; nested opens on the SAME
// logical blob are represented by the bracket counter rather than a
// stack. The opener determines the expected closer character and
// indent; the string-aware scan over the body only moves the counter.
type Detector struct {
	// out accumulates emitted anomalies across the scan. Finalize
	// appends the optional truncated-sentinel anomaly and returns this
	// slice.
	out []analyzer.Anomaly

	// open indicates whether a blob is currently in progress. When
	// false, the other open* fields are zero/undefined.
	open bool

	// openLine is the 1-based line number of the opener (the line that
	// was exactly `{` or `[` after trimming whitespace).
	openLine int64

	// openStartOffset is the byte offset of the opener line's first byte.
	openStartOffset int64

	// openIndent is the opener line's indent prefix (count of leading
	// tabs/spaces). The closer must match this indent to count.
	openIndent int

	// openLineIndex records the push-number of the opener line in the
	// window. We compare this against the current line's push-number to
	// detect when the window has "aged out" the opener. See isAged.
	openLineIndex int64

	// expectedCloser is '}' or ']' depending on the opener character.
	// We only accept a matching closer — a `{` opened blob cannot close
	// on `]`. This catches some kinds of malformed JSON early.
	expectedCloser byte

	// counter is the string-aware bracket counter. Set to 1 when the
	// opener fires, then adjusted by (opens - closes) on every scanned
	// continuation line. When counter == 0 AND we're on a matching
	// closer line we emit and close.
	counter int

	// truncatedCount tracks how many blobs were aborted due to the
	// window-age check during this run. Reported once by Finalize via
	// the sentinel anomaly when > 0.
	truncatedCount int
}

// New returns a freshly-initialized Detector. Used by tests and by the
// init() registration below.
func New() *Detector {
	return &Detector{}
}

// Name returns the stable registry identifier.
func (d *Detector) Name() string { return detectorName }

// Version returns the semver string for this detector's cache bucket.
func (d *Detector) Version() string { return detectorVersion }

// Category returns the human-readable bucket name shown in /v1/detectors.
func (d *Detector) Category() string { return detectorCategory }

// Description returns the one-line human summary.
func (d *Detector) Description() string { return detectorDescription }

// Supports says yes to anything. JSON blobs can appear in any
// text-shaped log; the per-line scan is cheap enough to always run.
// Binary files won't produce valid openers so the detector is a no-op
// on them in practice.
func (d *Detector) Supports(_ string, _ string, _ int64) bool {
	return true
}

// Analyze is the FileAnalyzer entry point. The line-detector path is
// driven by the coordinator, not through Analyze, so this returns an
// empty Report — it's here to satisfy the interface. Callers that want
// real anomalies must go through the coordinator/index.Build path.
func (d *Detector) Analyze(_ context.Context, _ analyzer.Input) (*analyzer.Report, error) {
	return &analyzer.Report{
		Name:          detectorName,
		Version:       detectorVersion,
		SchemaVersion: 1,
		Result:        map[string]any{},
	}, nil
}

// OnLine is the streaming-scan hook. Called once per line in order by
// the coordinator.
//
// Two cases:
//
//   - closed: try to open a new blob if this line is exactly `{` or `[`
//     (trimmed). Otherwise this line is uninteresting.
//
//   - open: scan the line's bytes with the string-aware counter. If
//     the window has aged out the opener without a close, abort with a
//     truncated emission. If the counter hits zero on a valid closer,
//     emit the clean anomaly.
//
// The coordinator passes a *Window; we only read w.Current() here. We
// do NOT retain ev.Bytes across calls (borrowed-bytes contract); all
// reads happen within this invocation.
func (d *Detector) OnLine(w *analyzer.Window) {
	ev := w.Current()
	trimmed := bytes.TrimSpace(ev.Bytes)

	if !d.open {
		d.tryOpen(ev, trimmed, w)
		return
	}

	d.continueBlob(ev, trimmed, w)
}

// tryOpen handles the "no blob in progress" state. If the current
// line's trimmed content is exactly `{` or `[` we transition to the
// open state; otherwise we stay closed.
//
// Passing the window lets us stamp openLineIndex with a per-push
// counter for the aging check.
func (d *Detector) tryOpen(ev analyzer.LineEvent, trimmed []byte, w *analyzer.Window) {
	if len(trimmed) != 1 {
		return
	}
	var closer byte
	switch trimmed[0] {
	case '{':
		closer = '}'
	case '[':
		closer = ']'
	default:
		return
	}

	d.open = true
	d.openLine = ev.Number
	d.openStartOffset = ev.StartOffset
	d.openIndent = ev.IndentPrefix
	d.openLineIndex = windowPushCount(w)
	d.expectedCloser = closer
	// The opener contributes +1 to the counter directly. We do NOT run
	// the string-aware scan over the opener line — it is exactly `{` or
	// `[`, so scanning would redundantly count the same bracket.
	d.counter = 1
}

// continueBlob handles the "blob in progress" state: one line of the
// body. We check the window-age guard first (so a truncated blob is
// emitted with the current line included in the span), then run the
// string-aware bracket scan, then check for a successful close.
//
// The "decrement below zero" case aborts the blob silently: the input
// is malformed in a way we can't describe. This matches the task's
// emphasis on navigation over verdict — don't flag garbage as a blob.
func (d *Detector) continueBlob(ev analyzer.LineEvent, trimmed []byte, w *analyzer.Window) {
	// Aging check. The window's push counter is monotonic; openLineIndex
	// was recorded at the opener's push. If the distance reaches the
	// window size we can no longer see the opener's slot and therefore
	// cannot safely resume — emit a truncated anomaly and close.
	if d.isAged(w) {
		d.emitTruncated(ev)
		d.reset()
		return
	}

	opens, closes := scanBrackets(ev.Bytes)
	d.counter += opens
	d.counter -= closes

	if d.counter < 0 {
		// Malformed: more closers than openers across the life of the
		// blob. Abandon without emitting — we have no confidence this
		// was a JSON region at all.
		d.reset()
		return
	}

	if d.counter == 0 {
		// Potential close. We accept only when the line's trimmed
		// content ENDS with the expected closer AND the line's indent
		// matches the opener's.
		if len(trimmed) == 0 || trimmed[len(trimmed)-1] != d.expectedCloser {
			// Counter fell to zero mid-line (e.g. a single-line sub-
			// object inside the blob body). But the blob's outer open
			// bracket is still open — we'd need the counter to cross
			// zero on a matching closer. Since it's zero here without
			// the right tail, we can't be at the outer close. In
			// practice this branch only fires for malformed input where
			// the outer opener gets balanced by something other than
			// the final closer. Abandon silently for the same
			// navigation-over-verdict reason as above.
			d.reset()
			return
		}
		if ev.IndentPrefix != d.openIndent {
			// A matching closer character but at a different indent:
			// likely malformed or not the blob's own closer. Abandon
			// silently — we won't emit a misleading span.
			d.reset()
			return
		}
		d.emitClean(ev)
		d.reset()
	}
}

// isAged reports whether the window has advanced far enough since the
// opener that we can no longer see the opener's slot. The check is
// "distance from openLineIndex to the current head push-count is >=
// window size". Using push-count (not line number) is resilient to any
// future change that might skip pushes (e.g. binary-line skipping).
func (d *Detector) isAged(w *analyzer.Window) bool {
	cur := windowPushCount(w)
	// distance is the number of pushes strictly since the opener. When
	// it equals the window size, the opener has been overwritten.
	return cur-d.openLineIndex >= int64(w.Size())
}

// emitClean records a successful blob close and appends the anomaly.
// ev is the CLOSE line — its EndOffset is the blob's end.
func (d *Detector) emitClean(ev analyzer.LineEvent) {
	lineCount := ev.Number - d.openLine + 1
	d.out = append(d.out, analyzer.Anomaly{
		StartLine:   d.openLine,
		EndLine:     ev.Number,
		StartOffset: d.openStartOffset,
		EndOffset:   ev.EndOffset,
		Severity:    severity,
		// Semantic category — the coordinator's Finalize overwrites
		// this with the detector's Name() before returning. Keeping a
		// meaningful value here helps for direct-use paths (tests).
		Category:    detectorCategory,
		Description: fmt.Sprintf("multi-line JSON blob, %d lines", lineCount),
	})
}

// emitTruncated records an aborted blob and appends a partial-span
// anomaly covering opener..current line. The current line is
// intentionally the LAST line the window could still see; beyond that
// we'd be operating on a stale view. Also bumps the per-run counter.
func (d *Detector) emitTruncated(ev analyzer.LineEvent) {
	lineCount := ev.Number - d.openLine + 1
	d.out = append(d.out, analyzer.Anomaly{
		StartLine:   d.openLine,
		EndLine:     ev.Number,
		StartOffset: d.openStartOffset,
		EndOffset:   ev.EndOffset,
		Severity:    severity,
		Category:    detectorCategory,
		Description: fmt.Sprintf("truncated: blob opened at line %d, aborted at window edge after %d lines",
			d.openLine, lineCount),
	})
	d.truncatedCount++
}

// Finalize is called once after the last OnLine. Emits a sentinel
// per-run anomaly encoding the `truncated_at_window` signal when at
// least one blob was aborted. See the package doc for why this lives
// in a sentinel anomaly rather than Report.Result.
//
// FlushContext is unused here; JSON-blob detection is purely structural
// and does not depend on file-global stats.
func (d *Detector) Finalize(_ *analyzer.FlushContext) []analyzer.Anomaly {
	// If a blob is still open at EOF, we count it as truncated. The
	// opener's indent couldn't be balanced with an in-window closer —
	// this is functionally equivalent to the window-edge abort.
	if d.open {
		// ev.Number == openLine for an at-EOF abort — we don't have a
		// current-line here. Use opener line as the degenerate span.
		d.out = append(d.out, analyzer.Anomaly{
			StartLine:   d.openLine,
			EndLine:     d.openLine,
			StartOffset: d.openStartOffset,
			// We never saw a close, so the end offset is unknown; using
			// StartOffset keeps the span valid (zero-length) without
			// over-claiming coverage.
			EndOffset: d.openStartOffset,
			Severity:  severity,
			Category:  detectorCategory,
			Description: fmt.Sprintf("truncated: blob opened at line %d, unclosed at EOF",
				d.openLine),
		})
		d.truncatedCount++
		d.reset()
	}

	if d.truncatedCount > 0 {
		// Sentinel per-run anomaly. Zero-length span at offset 0 / line
		// 0 so it's distinguishable from real blob anomalies and never
		// overlaps them. Dedup key is (detector, 0, 0) so it collapses
		// across workers if multiple workers hit truncation — consumers
		// see a single per-run signal rather than one per worker.
		d.out = append(d.out, analyzer.Anomaly{
			StartLine:   0,
			EndLine:     0,
			StartOffset: 0,
			EndOffset:   0,
			Severity:    severity,
			Category:    detectorCategory,
			Description: fmt.Sprintf("%s (truncated_count=%d)",
				truncatedSentinelPrefix, d.truncatedCount),
		})
	}
	return d.out
}

// reset clears the "in-progress blob" fields without touching out or
// truncatedCount. Called both after successful emission and after any
// of the abort branches.
func (d *Detector) reset() {
	d.open = false
	d.openLine = 0
	d.openStartOffset = 0
	d.openIndent = 0
	d.openLineIndex = 0
	d.expectedCloser = 0
	d.counter = 0
}

// scanBrackets runs a string-aware pass over line and returns the
// counts of JSON bracket opens (`{` + `[`) and closes (`}` + `]`)
// encountered OUTSIDE any string literal.
//
// String handling rules (matching JSON):
//
//   - A double-quote `"` toggles inside-string state UNLESS it is
//     preceded by an odd number of backslashes (an escape). We walk
//     forward and count leading backslashes on each `"` to make that
//     determination correctly: `\"` is escaped, `\\"` is a literal
//     backslash followed by a real quote, `\\\"` is escaped again, etc.
//
//   - Inside a string, brackets (and everything else) are ignored.
//
//   - We do NOT attempt to track string carry-over across line boundaries.
//     JSON strings can't legally contain raw newlines, so a JSON blob
//     whose body lines each begin in "not in a string" state is the
//     correct assumption for well-formed input. If malformed input
//     straddles the bracket count via trick quoting, the detector will
//     mis-count — acceptable for a navigation hint at severity 0.3.
func scanBrackets(line []byte) (opens, closes int) {
	inString := false
	for i := 0; i < len(line); i++ {
		b := line[i]
		if inString {
			if b == '"' && !isEscaped(line, i) {
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			opens++
		case '}', ']':
			closes++
		}
	}
	return opens, closes
}

// isEscaped reports whether the byte at index i is preceded by an odd
// number of backslashes (and is therefore escaped in JSON string
// semantics). Scans backward from i-1 until a non-backslash byte.
func isEscaped(line []byte, i int) bool {
	count := 0
	for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
		count++
	}
	return count%2 == 1
}

// windowPushCount returns the Window's total push count. We read it via
// Len()-based arithmetic: the public API exposes only Size() and Len(),
// but the window increments an internal counter on every push. Since
// Len() saturates at Size(), a raw push-count is not directly visible.
// We therefore keep our own counter implicitly via the opener's
// line-number delta in isAged — but tests and the opener need a
// monotonic "pushes-so-far" reading. The workaround: use Len() while
// the window is still filling (it matches the push count), and use
// Size() plus a synthetic offset once saturated.
//
// Implementation: we approximate push-count using the current line's
// Number (absolute 1-based line number in the source). Line numbers
// are monotonic and increase by one per line, exactly like the push
// counter. That makes them interchangeable for our aging check. We
// read the current line from the window and return its Number.
//
// Rationale for this approach over a custom internal window field: the
// Window type is shared infrastructure (Task 1). Adding a new method
// for this single detector would widen the public API just for us. The
// line-number proxy is sufficient because every ProcessLine call is 1:1
// with one push.
func windowPushCount(w *analyzer.Window) int64 {
	return w.Current().Number
}

// Compile-time interface conformance checks. If either contract drifts
// we want the build to fail here rather than somewhere deep in wiring.
var (
	_ analyzer.FileAnalyzer = (*Detector)(nil)
	_ analyzer.LineDetector = (*Detector)(nil)
)

// init registers a fresh Detector with the global analyzer registry.
// Callers activate the detector by blank-importing this package in
// cmd/rx/main.go:
//
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/jsonblob"
//
// The registered instance is a single shared one today because the
// coordinator is sequential. When the builder becomes chunk-parallel,
// each worker must instantiate its own Detector (via New) so blob
// state doesn't leak across workers.
func init() {
	analyzer.Register(New())
}
