// Package tracebackjava implements the `traceback-java` line detector.
//
// What it does:
//
//   - Flags contiguous regions that look like a Java stack trace. Java
//     emits several opener shapes:
//
//     1. `Exception in thread "..." <FQCN>[<Exception|Error>]: <msg>` —
//     the standard uncaught-exception form from the JVM.
//
//     2. A bare fully-qualified exception/error type at column 0, e.g.
//     `java.lang.IllegalStateException: something broke`. This form is
//     common when the application prints the throwable via
//     `Throwable.printStackTrace(PrintStream)` directly (no thread
//     prefix) or when only the inner exception is logged.
//
//   - After the opener, Java appends:
//
//     `\tat some.package.Class.method(FileName.java:123)` — stack frames
//     `\t... N more` — collapsed shared-tail frames
//     `Caused by: some.pkg.Other: message` — cause chain (one per depth)
//     `\tSuppressed: some.pkg.Other: message` — suppressed exceptions
//     (may be indented with tabs/spaces)
//
//   - The region ends at the first blank line or first line that doesn't
//     match any of the continuation shapes. Multiple back-to-back stack
//     traces (different threads exiting simultaneously) produce one
//     anomaly each — a new `Exception in thread "..."` line closes the
//     current region and starts a new one.
//
//   - Severity 0.7: a Java stack trace is almost always a real signal
//     worth jumping to. Not 1.0 because tracebacks are also frequent
//     noise in some applications (e.g. per-request handler errors in a
//     web server).
//
// State machine (one detector instance, one "current stack" at a time):
//
//	closed:                       no stack in progress
//	  on a line matching either opener cue: transition to in_stack,
//	    record the opener span (line number, start offset, end offset).
//
//	in_stack:                     continuation expected
//	  - blank line: close; emit; transition to closed.
//	  - continuation line (frame / "... N more" / "Caused by:" /
//	    "Suppressed:"): extend end span; stay in in_stack.
//	  - opener line (either shape): current stack ends, emit what we
//	    have, and open a new stack starting from this line. This is the
//	    back-to-back multiple-stack case.
//	  - any other line: close; emit; transition to closed; re-process
//	    the current line through tryOpen in case it looked stack-ish
//	    but didn't parse (defensive — in practice this second pass is a
//	    no-op because we already rejected it).
//
// Finalize:
//   - If we're in_stack at EOF, emit the pending region spanning opener
//     through the last continuation line. Stacks at the very end of a
//     file are legitimate signals.
//
// Registration: this package has an init() that calls analyzer.Register
// so a blank import in cmd/rx/main.go is enough to hook it up.
package tracebackjava

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
	detectorName        = "traceback-java"
	detectorVersion     = "0.1.0"
	detectorCategory    = "log-traceback"
	detectorDescription = "Java stack traces (Exception in thread ... / Caused by: / Suppressed:)"

	// severity is the plan-mandated value for this detector.
	severity = 0.7
)

// openerThreadRe matches the JVM "Exception in thread" opener. The
// trailing content (exception type + colon + message) is not validated
// here — if a line starts with `Exception in thread "name"`, we treat
// it as the start of a stack. Anchored with `^` so we never fire on a
// substring match elsewhere in a line.
//
// Compiled once via regexp.MustCompile; RE2 so no catastrophic
// backtracking on pathological input.
var openerThreadRe = regexp.MustCompile(`^Exception in thread "[^"]+"`)

// openerBareRe matches a bare fully-qualified exception or error type
// at the start of a line, e.g. `java.lang.IllegalStateException: ...`
// or `org.springframework.BeanCreationException ...`.
//
// Shape: at least one package segment (`(?:\w+\.)+`) plus a leaf class
// name that ends in `Exception` or `Error`. The trailing group ensures
// we don't match fragments of prose; the type must be followed by `:`,
// whitespace, or end-of-line.
var openerBareRe = regexp.MustCompile(`^(?:\w+\.)+\w+(?:Exception|Error)(?::|\s|$)`)

// continuationAtRe matches a stack frame line: leading whitespace + `at`
// + `FQCN.method(Source.java:line)`. Java frames always use this shape;
// some JDK frames use `$` (inner classes) or `<init>` / `<clinit>`
// (constructors / static initializers) in the method slot, so the
// character class includes `$`, `<`, and `>`.
var continuationAtRe = regexp.MustCompile(`^\s+at [\w.$<>]+\(.*\)$`)

// continuationMoreRe matches the `... N more` line the JVM emits to
// collapse the shared tail of a Caused-by chain. Always preceded by a
// tab and always ends in `more`.
var continuationMoreRe = regexp.MustCompile(`^\s+\.\.\. \d+ more$`)

// continuationSuppressedRe matches a `Suppressed: ...` line. Java
// indents Suppressed with a tab by default but the spec allows any
// leading whitespace here so unusual formatters still match.
var continuationSuppressedRe = regexp.MustCompile(`^\s*Suppressed: `)

// continuationCausedByPrefix is the exact prefix of a `Caused by: ...`
// line. Match via bytes.HasPrefix rather than regex — it's a literal
// and the fast path matters here.
var continuationCausedByPrefix = []byte("Caused by: ")

// state represents the detector's position in the state machine
// described in the package doc.
type state int

const (
	stateClosed state = iota
	stateInStack
)

// Detector implements both analyzer.FileAnalyzer (for registry
// enumeration) and analyzer.LineDetector (for the streaming scan).
//
// Only one stack trace can be in progress at a time; back-to-back
// stacks are emitted as separate anomalies — the detector closes the
// current region on a new opener line and immediately re-opens.
type Detector struct {
	// out accumulates emitted anomalies across the scan. Finalize emits
	// any still-open stack and returns this slice.
	out []analyzer.Anomaly

	// st is the state-machine position.
	st state

	// openLine is the 1-based line number of the opener line.
	openLine int64

	// openStartOffset is the byte offset of the opener line's first byte.
	openStartOffset int64

	// endLine is the 1-based line number of the last line that belongs
	// to the currently-tracked stack. Updated on every continuation.
	endLine int64

	// endOffset is the byte offset (exclusive) of the last byte of the
	// last line that belongs to the current stack. Used as the emitted
	// anomaly's EndOffset.
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

// Supports says yes to anything. Java stack traces can appear in any
// text-shaped log (application logs, CI output, container stdout, etc.).
// Non-Java logs simply never match the opener cues so the detector is a
// no-op at near-zero cost in practice.
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
// the coordinator. We branch on the current state.
//
// The coordinator passes a *Window; we only read w.Current() here. We do
// NOT retain ev.Bytes across calls (borrowed-bytes contract).
func (d *Detector) OnLine(w *analyzer.Window) {
	ev := w.Current()

	switch d.st {
	case stateClosed:
		d.tryOpen(ev)
	case stateInStack:
		d.handleInStack(ev)
	}
}

// tryOpen handles the stateClosed branch. If the current line matches
// either opener shape, transition to stateInStack and record the span.
func (d *Detector) tryOpen(ev analyzer.LineEvent) {
	if !isOpenerLine(ev.Bytes) {
		return
	}
	d.st = stateInStack
	d.openLine = ev.Number
	d.openStartOffset = ev.StartOffset
	d.endLine = ev.Number
	d.endOffset = ev.EndOffset
}

// handleInStack handles the stateInStack branch: the current line is
// either a continuation (extend span), a blank line (close), a new
// opener (close + re-open), or something else (close).
func (d *Detector) handleInStack(ev analyzer.LineEvent) {
	if len(ev.Bytes) == 0 {
		// Blank line: stack is over.
		d.emit()
		d.reset()
		return
	}
	if isOpenerLine(ev.Bytes) {
		// Back-to-back stacks: emit the current one and start a new one
		// from this line.
		d.emit()
		d.reset()
		d.tryOpen(ev)
		return
	}
	if isContinuationLine(ev.Bytes) {
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		return
	}
	// Non-continuation, non-opener, non-blank: stack ended. Emit and
	// close, then re-process the current line through tryOpen — it
	// should fail the opener check (we just verified above that it
	// doesn't) so this second pass is a no-op, but keeping the symmetry
	// with the Python detector makes future changes safer.
	d.emit()
	d.reset()
}

// isOpenerLine returns true if the line matches either opener shape.
// Pulled into its own helper because we check it from two states
// (stateClosed and stateInStack).
func isOpenerLine(line []byte) bool {
	return openerThreadRe.Match(line) || openerBareRe.Match(line)
}

// isContinuationLine returns true if the line matches any of the four
// continuation shapes: `at ...`, `... N more`, `Caused by: ...`, or
// `Suppressed: ...`.
func isContinuationLine(line []byte) bool {
	// Fast path for "Caused by: ": literal prefix, no regex needed.
	// Checked first because it's the cheapest test.
	if bytes.HasPrefix(line, continuationCausedByPrefix) {
		return true
	}
	return continuationAtRe.Match(line) ||
		continuationMoreRe.Match(line) ||
		continuationSuppressedRe.Match(line)
}

// Finalize is called once after the last OnLine. If we're mid-stack at
// EOF, emit what we have.
//
// FlushContext is unused here; Java stack detection is purely
// structural and doesn't depend on file-global stats.
func (d *Detector) Finalize(_ *analyzer.FlushContext) []analyzer.Anomaly {
	if d.st != stateClosed {
		d.emit()
		d.reset()
	}
	return d.out
}

// emit appends one anomaly for the currently-tracked stack span and
// leaves the state fields alone (reset does that separately). Split
// from reset so callers that want to emit-then-continue can do so in
// two readable steps.
func (d *Detector) emit() {
	lineCount := d.endLine - d.openLine + 1
	d.out = append(d.out, analyzer.Anomaly{
		StartLine:   d.openLine,
		EndLine:     d.endLine,
		StartOffset: d.openStartOffset,
		EndOffset:   d.endOffset,
		Severity:    severity,
		// Semantic category — this is the stable wire-contract
		// `category` field. The coordinator stamps Anomaly.DetectorName
		// separately and leaves Category alone.
		Category:    detectorCategory,
		Description: fmt.Sprintf("Java stack trace, %d lines", lineCount),
	})
}

// reset clears the current-stack fields and returns the state machine
// to stateClosed. Does not touch the accumulated out slice.
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

// init registers a detector FACTORY with the global analyzer registry.
// Callers activate the detector by blank-importing this package in
// cmd/rx/main.go:
//
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackjava"
//
// Factory-based registration: every index build gets its own fresh
// Detector so stack state cannot leak across builds.
func init() {
	analyzer.RegisterLineDetector(func() analyzer.LineDetector { return New() })
}
