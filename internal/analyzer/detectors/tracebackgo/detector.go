// Package tracebackgo implements the `traceback-go` line detector.
//
// What it does:
//
//   - Flags contiguous regions that look like a Go runtime traceback.
//     Go emits two opener shapes:
//
//     1. `panic: <message>` — an unrecovered panic, either from the
//     runtime (e.g. `panic: runtime error: index out of range ...`)
//     or from user code (`panic("something")`).
//
//     2. `fatal error: <message>` — the Go runtime's "cannot recover"
//     path, emitted for deadlocks (`all goroutines are asleep -
//     deadlock!`), concurrent map writes, stack overflow, etc.
//
//   - After the opener, Go's runtime appends a dump that typically
//     contains:
//
//     `[signal SIGSEGV: segmentation violation ...]` — a single
//     bracketed line with signal info (only when the panic was a
//     signal-delivered runtime error).
//
//     A blank line separating opener prose from the goroutine dump.
//
//     One or more `goroutine N [state]:` blocks, each followed by
//     indented stack frames (`main.foo(...)`, `\t/path/file.go:NN +0xNN`).
//     Goroutine blocks are separated by blank lines.
//
//     Unindented `created by ...` lines that name the goroutine's
//     parent frame — these are part of the dump, not separators.
//
//     Optionally: `exit status N` at the very end, emitted by the
//     `go run` driver or by shell tooling.
//
//   - Severity 0.8 — higher than Java/Python tracebacks (0.7) because a
//     Go panic/fatal is almost always a crash (not a per-request
//     exception) and therefore a stronger navigation target.
//
// State machine (one detector instance, one "current stack" at a time):
//
//	closed:                       no stack in progress
//	  on a line starting with `panic: ` or `fatal error: `:
//	    transition to in_stack, record span start.
//
//	in_stack:                     stack in progress
//	  - continuation line (goroutine header, indented frame/source ref,
//	    `created by ...`, `[signal ...]`): extend span, stay in in_stack.
//	  - `exit status N` line: extend span and close (this line is part
//	    of the region; `go run` appends it after the runtime dump).
//	  - blank line: transition to in_stack_blank (tentative close —
//	    inside a multi-goroutine dump, blank lines separate goroutine
//	    blocks and are NOT region terminators; we must look at the next
//	    line to decide).
//	  - opener line (either shape): close current stack, start a new
//	    one from this line (back-to-back panics — rare but possible in
//	    runtime-escaped multi-goroutine crashes).
//
//	in_stack_blank:               just saw a blank line inside a stack
//	  - continuation line: extend span through the blank AND this line;
//	    return to in_stack.
//	  - opener line: close current stack (without including the blank),
//	    start a new one from this line.
//	  - anything else (including another blank line): close (without
//	    including the blank or the current line); transition to closed.
//
// Finalize:
//   - If we're in_stack or in_stack_blank at EOF, emit the pending
//     region spanning opener through the last extended line.
//
// Registration: this package has an init() that calls analyzer.Register
// so a blank import in cmd/rx/main.go is enough to hook it up.
package tracebackgo

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
	detectorName        = "traceback-go"
	detectorVersion     = "0.1.0"
	detectorCategory    = "log-traceback"
	detectorDescription = "Go runtime tracebacks (panic: / fatal error: / goroutine N [state]:)"

	// severity is the plan-mandated value for this detector.
	severity = 0.8
)

// openerPanicPrefix matches the `panic: ` opener. Literal prefix check
// via bytes.HasPrefix is faster than a regex and perfectly specific —
// Go always emits this exact sequence at column 0 to start a panic
// dump.
var openerPanicPrefix = []byte("panic: ")

// openerFatalPrefix matches the `fatal error: ` opener. Same rationale
// as openerPanicPrefix — it's a literal column-0 prefix emitted by the
// Go runtime for unrecoverable fatal errors (deadlocks, concurrent map
// writes, stack overflow, out-of-memory, etc.).
var openerFatalPrefix = []byte("fatal error: ")

// goroutineHeaderRe matches a goroutine block header:
//
//	goroutine 42 [chan receive]:
//
// The state inside the square brackets is arbitrary (`running`,
// `chan receive`, `sleep`, `IO wait`, `semacquire`, `runnable`, ...)
// so we accept any non-`]` content. Anchored `^...:$` so only a full
// line match counts.
//
// Compiled once via regexp.MustCompile. RE2 — no catastrophic
// backtracking on pathological input.
var goroutineHeaderRe = regexp.MustCompile(`^goroutine \d+ \[[^\]]+\]:$`)

// createdByPrefix matches the `created by ...` continuation line that
// Go emits to name the parent frame of a goroutine spawned with `go`.
// Literal prefix check — always at column 0, always exactly this
// sequence.
var createdByPrefix = []byte("created by ")

// signalInfoPrefix matches the `[signal ...]` line emitted for panics
// triggered by OS signals (SIGSEGV, SIGBUS, SIGFPE, SIGILL, ...). Always
// appears at column 0 right after the opener line.
var signalInfoPrefix = []byte("[signal ")

// exitStatusRe matches the trailing `exit status N` line emitted by
// the `go run` driver or shell tooling after a crashed Go process. We
// include this line in the emitted region when present so the anomaly
// span covers the full user-visible crash block.
//
// Anchored `^...$` so a stray substring match elsewhere doesn't trigger.
var exitStatusRe = regexp.MustCompile(`^exit status \d+$`)

// goFuncCallRe matches the column-0 function-call lines Go emits inside
// a goroutine dump, e.g.:
//
//	main.doSomething()
//	main.process(0xc0000140a0, 0x1, 0x2)
//	net/http.serverHandler.ServeHTTP(0xc000010200, 0xc000012080, 0xc000012100)
//	main.(*Handler).Serve(0x0, 0xc000012080)
//	time.Sleep(0x3b9aca00)
//
// The shape: start with a letter/underscore, then any mix of word
// chars plus `.`, `/`, `*`, `(`, `)`, `$` (for method receivers, pkg
// paths, inner types), then an opening paren somewhere. Anchored `^`
// so mid-line matches don't trigger.
//
// Why not require `$` at the end: Go sometimes appends `, 0x...` args
// that span the rest of the line, and the runtime formatter uses
// specific argument rendering that may end mid-paren in unusual cases.
// We just need to recognize the prefix as "this is a Go frame line",
// so requiring a `(` somewhere early and a dot-separated identifier
// before it is enough.
var goFuncCallRe = regexp.MustCompile(`^[A-Za-z_][\w./*()$]*\.[\w*()$]+\(`)

// state represents the detector's position in the state machine
// described in the package doc.
type state int

const (
	stateClosed state = iota
	stateInStack
	stateInStackBlank
)

// Detector implements both analyzer.FileAnalyzer (for registry
// enumeration) and analyzer.LineDetector (for the streaming scan).
//
// Only one stack can be in progress at a time; multi-goroutine dumps
// are a single region (the blank lines between goroutine blocks are
// internal separators, handled by the stateInStackBlank branch).
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

// Supports says yes to anything. Go panic dumps can appear in any
// text-shaped log (application stdout, CI output, container logs,
// systemd journals). Non-Go logs simply never match the opener cues
// so the detector is a no-op at near-zero cost in practice.
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
	case stateInStackBlank:
		d.handleInStackBlank(ev)
	}
}

// tryOpen handles the stateClosed branch. If the current line starts
// with `panic: ` or `fatal error: `, transition to stateInStack and
// record the span start.
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
// either a continuation (extend span), an exit-status terminator (extend
// and close), a blank (tentative close, requires lookahead), a new
// opener (close + re-open), or anything else (close).
func (d *Detector) handleInStack(ev analyzer.LineEvent) {
	if len(ev.Bytes) == 0 {
		// Blank line: might be an internal separator between goroutine
		// blocks, or the end of the region. Defer the decision to the
		// next line (see handleInStackBlank).
		d.st = stateInStackBlank
		return
	}
	if isOpenerLine(ev.Bytes) {
		// Back-to-back panics: close the current region and start a new
		// one from this line. Rare, but observed when a panic in one
		// goroutine triggers a cascade.
		d.emit()
		d.reset()
		d.tryOpen(ev)
		return
	}
	if exitStatusRe.Match(ev.Bytes) {
		// `exit status N` is part of the region (the crash block ends
		// here). Extend the span to include it, emit, and close.
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		d.emit()
		d.reset()
		return
	}
	if isContinuationLine(ev.Bytes) {
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		return
	}
	// Non-continuation, non-opener, non-blank, non-exit-status: the
	// stack ended abruptly. Emit what we have and close.
	d.emit()
	d.reset()
}

// handleInStackBlank handles the stateInStackBlank branch: the previous
// line was blank and we need to decide whether the region continues.
//
// Per the plan's close rule ("blank line followed by non-continuation"):
//
//   - Continuation line: the blank was an internal separator between
//     goroutine blocks. Extend through both the blank (already accounted
//     for via the previous endOffset, which covers up to the blank) and
//     the current line, then return to stateInStack.
//
//     Note: the blank line itself is NOT explicitly added to the span;
//     we rely on endOffset already covering up to the blank's start.
//     The blank bytes (just the '\n') naturally fall inside the emitted
//     [StartOffset, EndOffset) range because we update endOffset to the
//     continuation line's EndOffset, and all byte offsets are
//     monotonic.
//
//   - Opener line: close the current region (without the blank),
//     restart from this line.
//
//   - Anything else (including another blank): close.
func (d *Detector) handleInStackBlank(ev analyzer.LineEvent) {
	if len(ev.Bytes) == 0 {
		// Two blank lines in a row: definitely the end. Close.
		d.emit()
		d.reset()
		return
	}
	if isOpenerLine(ev.Bytes) {
		// Close current, reopen on this line.
		d.emit()
		d.reset()
		d.tryOpen(ev)
		return
	}
	if exitStatusRe.Match(ev.Bytes) {
		// Blank + exit status: include the exit status line, emit, close.
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		d.emit()
		d.reset()
		return
	}
	if isContinuationLine(ev.Bytes) {
		// The blank was internal; extend through this continuation line
		// and go back to the normal in-stack state.
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		d.st = stateInStack
		return
	}
	// Non-continuation after a blank: the region is over.
	d.emit()
	d.reset()
}

// isOpenerLine returns true if the line starts with either opener
// prefix. Both must be anchored at column 0 — a mid-line `panic:` in
// a log message must not fire the detector.
func isOpenerLine(line []byte) bool {
	return bytes.HasPrefix(line, openerPanicPrefix) ||
		bytes.HasPrefix(line, openerFatalPrefix)
}

// isContinuationLine returns true if the line matches any of the
// continuation shapes:
//
//   - indented stack frames or source refs (line starts with a tab
//     or space; Go indents source-path lines with a tab but some
//     formatters use spaces)
//   - `created by pkg.Name` lines (unindented, literal prefix)
//   - `[signal ...]` signal-info lines (unindented, literal prefix)
//   - column-0 function-call frames (`main.foo(...)`,
//     `net/http.serverHandler.ServeHTTP(...)`, etc.)
//   - `goroutine N [state]:` headers
//
// The order mirrors frequency in typical Go panic output (indented
// source refs dominate) to keep the fast path fast.
func isContinuationLine(line []byte) bool {
	// Indented frame / source ref: the cheapest and most common test.
	// Any line starting with a tab or space is part of the dump when
	// we're in_stack — Go never emits mid-stack log prose like that.
	if len(line) > 0 && (line[0] == '\t' || line[0] == ' ') {
		return true
	}
	// `created by ...` — unindented, literal prefix.
	if bytes.HasPrefix(line, createdByPrefix) {
		return true
	}
	// `[signal ...]` — unindented, literal prefix.
	if bytes.HasPrefix(line, signalInfoPrefix) {
		return true
	}
	// Column-0 function-call frame. Go emits these unindented between
	// a goroutine header and its indented source-ref line. Match via
	// regex because the shape is varied (package paths, method
	// receivers, type assertions) but all include a dot-separated
	// identifier and an opening paren.
	if goFuncCallRe.Match(line) {
		return true
	}
	// `goroutine N [state]:` header — regex because of the numeric /
	// state-string slots. Checked last because the header appears
	// once per goroutine block while frames dominate.
	return goroutineHeaderRe.Match(line)
}

// Finalize is called once after the last OnLine. If we're mid-stack at
// EOF, emit what we have.
//
// FlushContext is unused here; Go traceback detection is purely
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
		// Semantic category — the coordinator's Finalize overwrites this
		// with the detector's Name() before returning. Keeping a
		// meaningful value here helps direct-use paths (tests).
		Category:    detectorCategory,
		Description: fmt.Sprintf("Go traceback, %d lines", lineCount),
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
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackgo"
//
// When the builder runs chunk-parallel, each worker must instantiate
// its own Detector (via New) so stack state doesn't leak across
// workers.
func init() {
	analyzer.Register(New())
}
