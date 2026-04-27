// Package tracebackjs implements the `traceback-js` line detector.
//
// What it does:
//
//   - Flags contiguous regions that look like a JavaScript / Node.js
//     stack trace. JS throwables serialize in a fairly consistent shape:
//
//     1. An opener line whose prose begins with an Error-type name and
//     a colon-space, e.g. `Error: something broke`, `TypeError: x is
//     not a function`, `UnhandledPromiseRejectionWarning: ...`.
//
//     2. One or more indented `at ...` frames — either parenthesized
//     (`    at Foo.bar (/path/file.js:12:34)`) or bare
//     (`    at /path/file.js:12:34`). Node and V8 use the same format;
//     browsers (Chrome, Edge, modern Firefox, Safari) emit the same
//     shape too.
//
//   - Opener DISAMBIGUATION: a line like `TypeError: x is undefined` is
//     valid plain English prose, and a naive `^\w*Error: ` match would
//     fire constantly on normal log messages. To avoid that, we require
//     at least one `\s+at ` line IMMEDIATELY FOLLOWING the opener. Only
//     then do we commit to "yes, this is a stack trace". This gives us
//     one-line lookahead semantics without actually reading ahead — we
//     just defer the "open" decision one step.
//
//   - The region ends at the first line that is not an `at ` frame. The
//     terminating line is NOT included in the emitted anomaly.
//
//   - Severity 0.6 — lower than Java/Python tracebacks (0.7) because JS
//     stack traces are often surfaced as informational warnings
//     (`UnhandledPromiseRejectionWarning`, `(node:12345) DeprecationWarning`)
//     rather than hard failures. Still a useful navigation target.
//
// State machine (one detector instance, one "current stack" at a time):
//
//	closed:                 no stack in progress
//	  on a line matching the opener cue `^\w*Error: `:
//	    transition to pending, record the span start; do NOT emit.
//
//	pending:                opener seen; waiting for confirmation
//	  - continuation line (matches the `\s+at ...` shapes):
//	    transition to in_stack, extend the span to include this frame.
//	    This is the commit point — the opener is now "real".
//	  - anything else (including another opener):
//	    abandon the pending opener; treat the current line as if we
//	    were in stateClosed (re-process through tryOpen so a new opener
//	    immediately after a rejected one still opens correctly).
//
//	in_stack:               at least one frame committed
//	  - continuation line: extend span; stay in_stack.
//	  - opener line: current region ends, emit, and open a new pending
//	    opener from this line.
//	  - anything else: close (emit); return to stateClosed.
//
// Finalize:
//   - If we're in_stack at EOF, emit the pending region. A `pending`
//     opener at EOF is NOT emitted — it never got confirmed, so by the
//     lookahead rule it doesn't count as a stack trace.
//
// Registration: this package has an init() that calls analyzer.Register
// so a blank import in cmd/rx/main.go is enough to hook it up.
package tracebackjs

import (
	"context"
	"fmt"
	"regexp"

	"github.com/wlame/rx-go/internal/analyzer"
)

// Metadata constants — kept as a block at the top so /v1/detectors
// output is trivially auditable against the plan.
const (
	detectorName        = "traceback-js"
	detectorVersion     = "0.1.0"
	detectorCategory    = "log-traceback"
	detectorDescription = "JavaScript / Node.js stack traces (Error: ... / at ...)"

	// severity is the plan-mandated value for this detector.
	severity = 0.6
)

// openerRe matches the opener cue: zero or more word-chars followed by
// `Error: ` at column 0. Matches bare `Error:` (the base Error type) as
// well as subclasses like `TypeError:`, `RangeError:`, `SyntaxError:`,
// `ReferenceError:`, and the Node-specific
// `UnhandledPromiseRejectionWarning:`. Wait — the last one ends with
// `Warning:`, not `Error:`, so it's handled separately below.
//
// Anchored with `^` so a mid-line "TypeError: x" in a log message can't
// fire.
//
// Compiled once; RE2, no catastrophic backtracking.
var openerRe = regexp.MustCompile(`^\w*Error: `)

// openerUnhandledRejectionRe matches Node's
// `UnhandledPromiseRejectionWarning: ...` opener. Node emits this header
// followed by an indented `at ...` stack, which looks exactly like a
// regular Error stack from the continuation side. Treat the warning
// header as a second opener shape so the rest of the state machine just
// works.
//
// Node has been deprecating and reshaping these warnings across
// versions; other common Node warning shapes (DeprecationWarning,
// ExperimentalWarning) generally DON'T include `at ...` frames, so they
// won't be confirmed by the lookahead — intentional.
var openerUnhandledRejectionRe = regexp.MustCompile(`^UnhandledPromiseRejectionWarning: `)

// continuationAtParenRe matches the parenthesized frame shape:
//
//	at Foo.bar (/path/to/file.js:12:34)
//	at Object.<anonymous> (/path/file.js:1:1)
//	at Module._compile (internal/modules/cjs/loader.js:999:30)
//	at Array.forEach (<anonymous>)
//
// The identifier may contain dots (namespacing), brackets (computed
// property names like `[Symbol.iterator]`), and angle brackets
// (`<anonymous>`, `<computed>`). Anchored `^...$` so trailing prose
// after the closing paren doesn't cause a false match.
var continuationAtParenRe = regexp.MustCompile(`^\s+at [\w.\[\]<>]+ \(.*\)$`)

// continuationAtBareRe matches the bare-path frame shape emitted when
// the runtime doesn't know a function name:
//
//	at /path/to/file.js:12:34
//	at file:///Users/x/app.js:5:1
//	at internal/bootstrap/node.js:623:3
//
// The line-column suffix `:\d+:\d+` is mandatory — without it we'd
// match arbitrary `    at something` prose, which would cause false
// positives on anything English-looking.
var continuationAtBareRe = regexp.MustCompile(`^\s+at .*:\d+:\d+$`)

// state represents the detector's position in the state machine
// described in the package doc.
type state int

const (
	stateClosed  state = iota
	statePending       // opener seen; waiting for at-line confirmation
	stateInStack       // at least one at-line committed
)

// Detector implements both analyzer.FileAnalyzer (for registry
// enumeration) and analyzer.LineDetector (for the streaming scan).
//
// Only one stack trace can be in progress at a time; back-to-back
// stacks are emitted as separate anomalies — the detector closes the
// current region on a new opener line and immediately re-opens (as
// pending).
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

// Supports says yes to anything. JS stack traces can appear in any
// text-shaped log (Node stdout/stderr, browser devtools exports, CI
// output, container logs). Non-JS logs simply never confirm the
// lookahead so the detector is a no-op at near-zero cost.
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
	case statePending:
		d.handlePending(ev)
	case stateInStack:
		d.handleInStack(ev)
	}
}

// tryOpen handles the stateClosed branch. If the current line matches
// the opener cue, transition to statePending and record the span start.
// We don't commit to "this is a stack" until the next line confirms
// with an `at ...` frame.
func (d *Detector) tryOpen(ev analyzer.LineEvent) {
	if !isOpenerLine(ev.Bytes) {
		return
	}
	d.st = statePending
	d.openLine = ev.Number
	d.openStartOffset = ev.StartOffset
	d.endLine = ev.Number
	d.endOffset = ev.EndOffset
}

// handlePending handles the statePending branch: we saw an opener but
// haven't confirmed it with an at-line yet. This is the "lookahead by
// one" mechanism: we delay the commit to the next OnLine call.
//
// If this line is a continuation, commit (transition to stateInStack).
// Otherwise, abandon — the opener was just prose (e.g. a log message
// mentioning "TypeError: foo" followed by unrelated content), so drop
// it and re-process the current line as if we'd started in stateClosed.
func (d *Detector) handlePending(ev analyzer.LineEvent) {
	if isContinuationLine(ev.Bytes) {
		// Confirmed: the opener + at-line pair is a real stack.
		d.st = stateInStack
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		return
	}
	// Not confirmed. Drop the pending opener and reprocess this line as
	// if we were in stateClosed — this handles the edge case of two
	// opener-shaped lines in a row where only the second one is real.
	d.reset()
	d.tryOpen(ev)
}

// handleInStack handles the stateInStack branch: the current line is
// either a continuation (extend span), a new opener (close + re-open as
// pending), or something else (close; the plan's rule is "first non-at
// line closes").
func (d *Detector) handleInStack(ev analyzer.LineEvent) {
	if isContinuationLine(ev.Bytes) {
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		return
	}
	if isOpenerLine(ev.Bytes) {
		// Back-to-back stacks: emit the current one and transition to
		// pending for the new opener. The new opener still needs its own
		// at-line confirmation.
		d.emit()
		d.reset()
		d.tryOpen(ev)
		return
	}
	// Non-continuation, non-opener: the stack is done. Emit and close.
	d.emit()
	d.reset()
}

// isOpenerLine returns true if the line matches either opener shape:
// the base `\w*Error: ` form or the Node-specific
// `UnhandledPromiseRejectionWarning: ` form.
func isOpenerLine(line []byte) bool {
	return openerRe.Match(line) || openerUnhandledRejectionRe.Match(line)
}

// isContinuationLine returns true if the line matches either of the
// two `at ...` frame shapes. Both are anchored and require specific
// structure (either parenthesized callsite or a file:line:col bare
// path), so non-stack prose that happens to start with `    at` cannot
// sneak in.
func isContinuationLine(line []byte) bool {
	return continuationAtParenRe.Match(line) || continuationAtBareRe.Match(line)
}

// Finalize is called once after the last OnLine. If we're in_stack at
// EOF, emit the pending region. A `statePending` opener at EOF is NOT
// emitted — the one-line lookahead rule requires at least one `at`
// frame to confirm, and we never saw one.
//
// FlushContext is unused here; JS stack detection is purely structural
// and doesn't depend on file-global stats.
func (d *Detector) Finalize(_ *analyzer.FlushContext) []analyzer.Anomaly {
	if d.st == stateInStack {
		d.emit()
		d.reset()
	}
	// statePending at EOF: opener was never confirmed — drop it silently.
	if d.st == statePending {
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
		// Semantic category — the coordinator's Finalize overwrites this
		// with the detector's Name() before returning. Keeping a
		// meaningful value here helps direct-use paths (tests).
		Category:    detectorCategory,
		Description: fmt.Sprintf("JavaScript stack trace, %d lines", lineCount),
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

// init registers a fresh Detector with the global analyzer registry.
// Callers activate the detector by blank-importing this package in
// cmd/rx/main.go:
//
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackjs"
//
// When the builder runs chunk-parallel, each worker must instantiate
// its own Detector (via New) so stack state doesn't leak across
// workers.
func init() {
	analyzer.Register(New())
}
