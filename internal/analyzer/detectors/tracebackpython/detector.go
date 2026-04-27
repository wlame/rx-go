// Package tracebackpython implements the `traceback-python` line detector.
//
// What it does:
//
//   - Flags contiguous regions that look like a Python traceback: the
//     opening cue is a line whose trimmed-of-trailing-whitespace content
//     is exactly `Traceback (most recent call last):`. The region grows
//     line-by-line through the indented frame lines (`  File "...", line
//     N, in func` / `    <source-snippet>`) and ends on the exception
//     line — the unindented `ExceptionType: message` line that follows
//     the last frame.
//
//   - Severity 0.7: a traceback is almost always a real signal worth
//     jumping to in a large log. Not 1.0 because tracebacks are also
//     frequent noise in some applications (e.g. per-request handler
//     errors in a web server).
//
// State machine (one detector instance, one "current traceback" at a time):
//
//	closed:                       no traceback in progress
//	  on a line matching the opening cue exactly:
//	    record opener span (line number, start offset), transition to
//	    in_frames. We do NOT include any "cause" / "during handling"
//	    preamble — the opener line itself is the region's start, matching
//	    the user-visible start of the traceback block.
//
//	in_frames:                    frames expected (indented lines)
//	  - indented line (tabs or spaces at start): continuation, extend
//	    end span to this line.
//	  - non-indented line:
//	      - if it matches the exception-line shape (`Word: ...` or bare
//	        `WordError` without the colon — e.g. `KeyboardInterrupt`):
//	        transition to in_exception, extend end span.
//	      - if it's a blank line: keep the in_frames state but DO NOT
//	        extend the span. Python tracebacks don't have blank lines
//	        inside the frame list; a blank line means the traceback is
//	        over without a recognizable exception-type line. Transition
//	        to closed and emit the anomaly spanning up to the previous
//	        non-blank line.
//	      - anything else: traceback ends abruptly. Emit what we have
//	        (opener..last frame) and transition to closed, then re-process
//	        the current line as if we were closed (it might be the start
//	        of a new traceback, but in practice it'll just be ordinary
//	        log content).
//
//	in_exception:                 exception line seen; waiting for close
//	  - any line closes the region. The exception line was the last line
//	    belonging to this traceback. Emit spanning opener..exception
//	    line. Transition to closed and re-process the current line.
//
// Finalize:
//   - If we're in_frames or in_exception at EOF, emit what we have so
//     tracebacks at the very end of a file aren't lost. For in_frames
//     the span ends on the last frame; for in_exception it ends on the
//     exception line.
//
// Chained tracebacks (`During handling of the above exception, another
// exception occurred:` / `The above exception was the direct cause of
// the following exception:`):
//
//   - Python emits these as SEPARATE traceback blocks separated by a
//     blank line and the "During handling" / "The above exception"
//     prose. Our detector treats them as separate anomalies — one per
//     `Traceback (most recent call last):` opener. This matches the
//     navigation intent: each traceback block is its own jump target.
//
// Registration: this package has an init() that calls analyzer.Register
// so a blank import in cmd/rx/main.go is enough to hook it up.
package tracebackpython

import (
	"bytes"
	"context"
	"fmt"
	"regexp"

	"github.com/wlame/rx-go/internal/analyzer"
)

// Metadata constants — kept as a block at the top so /v1/detectors
// output is trivially auditable against the plan.
const (
	detectorName        = "traceback-python"
	detectorVersion     = "0.1.0"
	detectorCategory    = "log-traceback"
	detectorDescription = "Python tracebacks (Traceback (most recent call last): ...)"

	// severity is the plan-mandated value for this detector.
	severity = 0.7
)

// openerCue is the exact opening-cue line (after trimming trailing
// whitespace). Python always emits this header verbatim at the start of
// a traceback, so a byte-for-byte match is safer than a regex.
var openerCue = []byte("Traceback (most recent call last):")

// exceptionLineRe matches the shape of the final line of a Python
// traceback: either `ExceptionType: message` or (rarely) just the
// exception type with no message, e.g. `KeyboardInterrupt`. The type
// itself may be a dotted path (`json.decoder.JSONDecodeError`).
//
// Compiled once at package init (via regexp.MustCompile). RE2 so no
// catastrophic backtracking on pathological input.
//
// Why this shape:
//
//   - `^[A-Za-z_][\w.]*`: an identifier (may include dots for module
//     paths). Python exception types are always valid identifiers.
//   - `(?::\s.*)?$`: optional colon+space+message tail. Some exceptions
//     (SystemExit with no arg, KeyboardInterrupt) have no message.
//
// We deliberately require the full line to match this shape so lines
// like `Exception group info:` (a colon but wrong followups) don't
// masquerade as exception lines. The pattern is anchored with `$`.
var exceptionLineRe = regexp.MustCompile(`^[A-Za-z_][\w.]*(?::\s.*)?$`)

// state represents the detector's position in the state machine
// described in the package doc.
type state int

const (
	stateClosed state = iota
	stateInFrames
	stateInException
)

// Detector implements both analyzer.FileAnalyzer (for registry
// enumeration) and analyzer.LineDetector (for the streaming scan).
//
// Only one traceback can be in progress at a time; chained tracebacks
// (separated by blank lines and "During handling..." prose) are treated
// as separate anomalies — we transition to closed between them.
type Detector struct {
	// out accumulates emitted anomalies across the scan. Finalize
	// emits any still-open traceback and returns this slice.
	out []analyzer.Anomaly

	// st is the state-machine position. stateClosed means no traceback
	// is in progress and the other span fields are zero/undefined.
	st state

	// openLine is the 1-based line number of the `Traceback (most
	// recent call last):` opener.
	openLine int64

	// openStartOffset is the byte offset of the opener line's first byte.
	openStartOffset int64

	// endLine is the 1-based line number of the last line that belongs
	// to the currently-tracked traceback. Updated on every continuation.
	endLine int64

	// endOffset is the byte offset (exclusive) of the last byte of the
	// last line that belongs to the current traceback. Used as the
	// emitted anomaly's EndOffset.
	endOffset int64
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

// Supports says yes to anything. Python tracebacks can appear in any
// text-shaped log (web server logs, batch job output, syslog, etc.).
// Non-Python logs simply never match the opener cue so the detector is
// a no-op at near-zero cost in practice.
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
// We branch on the current state and dispatch to the per-state handler
// helpers. The handlers may transition the state and, in stateInException,
// may re-process the current line after closing a traceback — but only
// by pushing it through tryOpen, which is the only path back into an
// open state.
//
// The coordinator passes a *Window; we only read w.Current() here. We do
// NOT retain ev.Bytes across calls (borrowed-bytes contract).
func (d *Detector) OnLine(w *analyzer.Window) {
	ev := w.Current()

	switch d.st {
	case stateClosed:
		d.tryOpen(ev)
	case stateInFrames:
		d.handleInFrames(ev)
	case stateInException:
		d.handleInException(ev)
	}
}

// tryOpen handles the stateClosed branch. If the current line's trimmed-
// trailing-whitespace content matches the opener cue, we transition to
// stateInFrames and record the span's start.
//
// Trimming only the trailing whitespace (not the leading) because the
// opener cue must start at column 0 for real Python tracebacks. Leading
// whitespace would indicate an indented/nested traceback-in-another-
// tool's-output, which we don't try to handle.
func (d *Detector) tryOpen(ev analyzer.LineEvent) {
	if !bytes.Equal(bytes.TrimRight(ev.Bytes, " \t"), openerCue) {
		return
	}
	d.st = stateInFrames
	d.openLine = ev.Number
	d.openStartOffset = ev.StartOffset
	d.endLine = ev.Number
	d.endOffset = ev.EndOffset
}

// handleInFrames handles the stateInFrames branch: we've seen the opener
// and are expecting indented frame lines or the final exception line.
//
// Branches:
//
//   - Indented line (starts with tab or space): continuation. Extend
//     the end span and stay in stateInFrames.
//   - Blank line: Python tracebacks don't have blank lines mid-frame.
//     A blank line means the traceback is over without a recognizable
//     exception-type line. Emit what we have and close.
//   - Non-indented, non-blank line: candidate exception-type line. If
//     it matches the exception-line shape, extend the span and
//     transition to stateInException. Otherwise, the traceback ended
//     abruptly (malformed input or truncated log); emit what we have
//     spanning opener..last frame and close, then try to re-open on
//     the current line in case it's the start of a new traceback.
func (d *Detector) handleInFrames(ev analyzer.LineEvent) {
	if len(ev.Bytes) == 0 {
		// Blank line inside frames: traceback ended without exception
		// line. Emit up to the previous non-blank (already tracked in
		// endLine/endOffset) and close.
		d.emit()
		d.reset()
		return
	}
	if ev.IndentPrefix > 0 {
		// Indented: a frame line or source-snippet line. Extend span.
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		return
	}
	// Unindented, non-blank: candidate exception line.
	if exceptionLineRe.Match(ev.Bytes) {
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		d.st = stateInException
		return
	}
	// Malformed / truncated traceback. Emit what we have and close, then
	// re-process the current line so it can possibly start a new block.
	d.emit()
	d.reset()
	d.tryOpen(ev)
}

// handleInException handles the stateInException branch: we saw the
// exception line and the next line (any line) closes the region. We
// emit the current traceback and re-process the current line via
// tryOpen in case it's the start of a new block (chained tracebacks
// come through this path — the line after an exception line is a blank
// line, which tryOpen will ignore, and then `During handling of...`
// or `Traceback (most recent call last):` follows).
func (d *Detector) handleInException(ev analyzer.LineEvent) {
	d.emit()
	d.reset()
	d.tryOpen(ev)
}

// Finalize is called once after the last OnLine. If we're mid-traceback
// at EOF, emit what we have. Either in_frames or in_exception counts:
// a traceback at the end of a file is a legitimate region to flag.
//
// FlushContext is unused here; Python traceback detection is purely
// structural and doesn't depend on file-global stats.
func (d *Detector) Finalize(_ *analyzer.FlushContext) []analyzer.Anomaly {
	if d.st != stateClosed {
		d.emit()
		d.reset()
	}
	return d.out
}

// emit appends one anomaly for the currently-tracked traceback span and
// leaves the state fields alone (reset does that separately). Split from
// reset so callers that want to emit-then-continue can do so in two
// readable steps.
func (d *Detector) emit() {
	lineCount := d.endLine - d.openLine + 1
	d.out = append(d.out, analyzer.Anomaly{
		StartLine:   d.openLine,
		EndLine:     d.endLine,
		StartOffset: d.openStartOffset,
		EndOffset:   d.endOffset,
		Severity:    severity,
		// Semantic category — the coordinator's Finalize overwrites
		// this with the detector's Name() before returning. Keeping a
		// meaningful value here helps for direct-use paths (tests).
		Category:    detectorCategory,
		Description: fmt.Sprintf("Python traceback, %d lines", lineCount),
	})
}

// reset clears the current-traceback fields and returns the state
// machine to stateClosed. Does not touch the accumulated out slice.
func (d *Detector) reset() {
	d.st = stateClosed
	d.openLine = 0
	d.openStartOffset = 0
	d.endLine = 0
	d.endOffset = 0
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
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackpython"
//
// The registered instance is a single shared one today because the
// coordinator is sequential. When the builder becomes chunk-parallel,
// each worker must instantiate its own Detector (via New) so traceback
// state doesn't leak across workers.
func init() {
	analyzer.Register(New())
}
