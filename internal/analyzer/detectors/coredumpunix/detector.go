// Package coredumpunix implements the `coredump-unix` line detector.
//
// What it does:
//
//   - Flags contiguous regions that look like one of four distinct Unix
//     crash/abort output shapes. All four share a single detector because
//     they all mean "the process just died hard" and all fit the same
//     anomaly contract (start line, end line, severity 0.9). Keeping
//     them together means one init, one cache bucket, one registry
//     entry — but the close-rule logic is per-variant.
//
//     The four shapes are, in order of typical terseness:
//
//     1. SEGFAULT — the terse default. A line containing the literal
//     `Segmentation fault` (with optional `(core dumped)` tail) is
//     enough. The kernel writes this to the shell when a process dies
//     of SIGSEGV and the parent doesn't catch it. Usually 1 line,
//     sometimes 2 (the bare `Segmentation fault` followed by a
//     `(core dumped)` variant on the next line).
//
//     2. ASAN — AddressSanitizer output. Opens with
//     `==PID==ERROR: AddressSanitizer: ...`, emits indented `#0`,
//     `#1`, ... frames and a SUMMARY line, then a final
//     `==PID==ABORTING` tail. Can run dozens of lines.
//
//     3. KERNEL — kernel oops / panic output from dmesg/syslog. Opens
//     with `[   123.456789] Call Trace:` (the bracketed timestamp +
//     literal "Call Trace:"). Continuation lines are the same bracketed
//     timestamp format with kernel-frame content. Closes at the first
//     bracketed-timestamp line that ends the trace (typically
//     `---[ end trace ... ]---`) or the first non-bracketed line.
//
//     4. STACKSMASH — glibc's stack-protector abort. Opens with
//     `*** stack smashing detected ***`, emits a Backtrace block and a
//     Memory map dump, closes at `Aborted (core dumped)` or blank.
//
//   - One combined opener regex matches any of the four shapes. Once
//     an opener is seen, the detector records which variant it was
//     and switches its close-rule logic accordingly. The per-variant
//     continuation/close tests are small and readable, and the state
//     machine stays flat.
//
//   - Severity 0.9: a hard crash is almost always a real signal worth
//     jumping to. Not 1.0 because stack-smashing / segfault output can
//     occasionally appear as test fixtures or intentional fuzzing
//     output; secrets get the 1.0 "loud" slot.
//
// State machine (one detector instance, one "current region" at a time):
//
//	closed:                           no region in progress
//	  on a line matching the combined opener regex:
//	    - SEGFAULT: record variant, open span over just this line,
//	      transition to inSegfaultTail (the next line may or may not
//	      be a follow-up `(core dumped)` — most of the time the
//	      opener line already contains it, in which case the very
//	      next non-matching line closes).
//	    - ASAN: record variant, open span, transition to inAsan.
//	    - KERNEL: record variant, open span, transition to inKernel.
//	    - STACKSMASH: record variant, open span, transition to
//	      inStackSmash.
//
//	inSegfaultTail:                   just saw a Segfault opener
//	  - another segfault-shape line (the `(core dumped)` tail that
//	    sometimes follows): extend span one line, then close on the
//	    next OnLine. (Handled by reusing the same segfault opener
//	    test for the "one more line" extension.)
//	  - anything else: close immediately. The segfault region is
//	    one or two lines max.
//
//	inAsan:                           inside AddressSanitizer output
//	  - continuation (indented `#\d+` frame, SUMMARY line, READ/WRITE
//	    header, Shadow memory line, etc.): extend span.
//	  - `==PID==ABORTING` tail line: extend span, emit, close.
//	  - blank line: emit, close. ASAN doesn't use internal blanks.
//	  - anything else that isn't a new opener: extend span (ASAN is
//	    verbose and emits many shapes of continuation — we treat
//	    "anything until ABORTING or blank" as still-inside).
//
//	inKernel:                         inside a kernel Call Trace
//	  - bracketed-timestamp line `[\s*\d+\.\d+]`: extend span. The
//	    `---[ end trace ... ]---` tail is itself a bracketed-timestamp
//	    line, so it's naturally included.
//	  - blank line: emit, close.
//	  - anything else (no bracketed timestamp): emit, close.
//
//	inStackSmash:                     inside stack-smashing dump
//	  - continuation (Backtrace/Memory map section headers, indented
//	    hex addresses, paths): extend span.
//	  - `Aborted (core dumped)` line: extend span, emit, close.
//	  - blank line: emit, close.
//	  - new opener line: emit, close, re-open.
//
// Finalize:
//   - If we're in any in-region state at EOF, emit the pending region.
//     A crash dump at the very tail of a truncated log is still a
//     legitimate signal — arguably the strongest one.
//
// Registration: this package has an init() that calls analyzer.Register
// so a blank import in cmd/rx/main.go is enough to hook it up.
package coredumpunix

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
	detectorName        = "coredump-unix"
	detectorVersion     = "0.1.0"
	detectorCategory    = "log-crash"
	detectorDescription = "Unix crash dumps (segfault / ASAN / kernel oops / stack smashing)"

	// severity is the plan-mandated value for this detector.
	severity = 0.9
)

// combinedOpenerRe matches ANY of the four opener shapes with a single
// alternation. We inspect the match to figure out which variant we
// matched — see classifyOpener.
//
// Anchored with `^` on each alternative so a mid-line substring like
// "got Segmentation fault while running" can't fire the detector.
//
// Shape per alternative:
//
//   - Segfault: the literal string `Segmentation fault` at column 0.
//     The `(core dumped)` tail, when present, is on the same line most
//     of the time so we don't require it.
//
//   - Stack-smashing: the exact glibc banner. The `***` delimiters make
//     it unambiguous.
//
//   - ASAN: `==NNN==ERROR: AddressSanitizer:` where NNN is the PID.
//
//   - Kernel: `[   123.456789] Call Trace:` — bracketed kernel timestamp
//     (leading spaces inside the brackets are typical) followed by
//     literal `Call Trace:`. The `\s*` inside the brackets tolerates
//     any amount of leading whitespace kernels add for time alignment.
//
// Compiled once via regexp.MustCompile; RE2 — no catastrophic backtracking.
var combinedOpenerRe = regexp.MustCompile(
	`^(?:` +
		`Segmentation fault` +
		`|\*\*\* stack smashing detected \*\*\*` +
		`|==\d+==ERROR: AddressSanitizer:` +
		`|\[\s*\d+\.\d+\] Call Trace:` +
		`)`,
)

// asanAbortingRe matches the ASAN abort tail, e.g. `==12345==ABORTING`.
// This is the canonical ASAN close signal. The rest of ASAN's output
// (indented `#\d+` frames, `READ of size N` / `WRITE of size N` size
// headers, `SUMMARY:` line, `Shadow bytes ...` memory-dump sections)
// is intentionally NOT matched explicitly — ASAN formatting is
// version-dependent and we over-include rather than truncate mid-dump.
// The close rule is purely "ABORTING tail or blank line".
var asanAbortingRe = regexp.MustCompile(`^==\d+==ABORTING`)

// kernelTimestampRe matches any line starting with a bracketed kernel
// timestamp `[\s*\d+\.\d+]`. Every continuation line of a kernel Call
// Trace starts with this prefix; the first non-matching line closes.
//
// Anchored `^` so we never match mid-line substrings.
var kernelTimestampRe = regexp.MustCompile(`^\[\s*\d+\.\d+\]`)

// segfaultCoreDumpedPrefix is the optional second-line tail some shells
// emit after a Segmentation fault ("(core dumped)"). It is rare — most
// of the time the tail is on the opener line itself. We only use this
// prefix check in inSegfaultTail to decide whether to extend by one
// more line.
var segfaultCoreDumpedPrefix = []byte("(core dumped)")

// abortedCoreDumpedPrefix matches the stack-smashing close tail that
// glibc emits after the Memory map dump: `Aborted (core dumped)`. We
// include this line in the emitted region so the span covers the full
// user-visible crash block. The intermediate section headers
// (`======= Backtrace: =========`, `======= Memory map: ========`)
// and their content are intentionally NOT matched — same rationale as
// ASAN, the output format is glibc-version-dependent and the
// "Aborted (core dumped) or blank" close rule is strict enough.
var abortedCoreDumpedPrefix = []byte("Aborted (core dumped)")

// variant identifies which of the four crash-output shapes we matched.
// Recorded on open; used to pick the right close rule on every
// subsequent OnLine until reset.
type variant int

const (
	variantNone variant = iota
	variantSegfault
	variantAsan
	variantKernel
	variantStackSmash
)

// state represents the detector's position in the per-variant state
// machine. The variant field selects WHICH close rule applies; the
// state field tracks position WITHIN that rule.
type state int

const (
	stateClosed state = iota
	// stateInSegfaultTail is the immediate post-opener state for the
	// segfault variant. The opener line is recorded; the next line
	// may extend the span (a `(core dumped)` follow-up) or close.
	stateInSegfaultTail
	// stateInAsan is the inside-ASAN state.
	stateInAsan
	// stateInKernel is the inside-kernel-Call-Trace state.
	stateInKernel
	// stateInStackSmash is the inside-stack-smashing-dump state.
	stateInStackSmash
)

// Detector implements both analyzer.FileAnalyzer (for registry
// enumeration) and analyzer.LineDetector (for the streaming scan).
//
// Only one region can be in progress at a time. Back-to-back crashes
// (rare but possible — think a test runner that keeps going after one
// child segfaults) emit separate anomalies: the current region closes
// on a new opener line and the new region opens immediately.
type Detector struct {
	// out accumulates emitted anomalies across the scan. Finalize
	// emits any still-open region and returns this slice.
	out []analyzer.Anomaly

	// v is the variant of the currently-open region. variantNone when
	// st == stateClosed. Used by emit() to pick a meaningful
	// Description and by the per-state handlers to know they're in
	// the right branch.
	v variant

	// st is the state-machine position.
	st state

	// openLine is the 1-based line number of the opener line.
	openLine int64

	// openStartOffset is the byte offset of the opener line's first byte.
	openStartOffset int64

	// endLine is the 1-based line number of the last line that belongs
	// to the currently-tracked region. Updated on every continuation.
	endLine int64

	// endOffset is the byte offset (exclusive) of the last byte of the
	// last line that belongs to the current region. Used as the emitted
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

// Supports says yes to anything. Unix crash output can appear in any
// text-shaped log (stdout/stderr, dmesg capture, systemd journals, CI
// output, container logs). Non-matching logs simply never trigger the
// opener so the detector is a no-op at near-zero cost in practice.
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
// the coordinator. We branch on the current state and then, within
// each state, on the current variant's continuation/close rules.
//
// The coordinator passes a *Window; we only read w.Current() here. We
// do NOT retain ev.Bytes across calls (borrowed-bytes contract).
func (d *Detector) OnLine(w *analyzer.Window) {
	ev := w.Current()

	switch d.st {
	case stateClosed:
		d.tryOpen(ev)
	case stateInSegfaultTail:
		d.handleInSegfaultTail(ev)
	case stateInAsan:
		d.handleInAsan(ev)
	case stateInKernel:
		d.handleInKernel(ev)
	case stateInStackSmash:
		d.handleInStackSmash(ev)
	}
}

// tryOpen handles the stateClosed branch. If the current line matches
// the combined opener regex, classify which variant we saw and
// transition to the matching per-variant state.
func (d *Detector) tryOpen(ev analyzer.LineEvent) {
	v := classifyOpener(ev.Bytes)
	if v == variantNone {
		return
	}
	d.v = v
	d.openLine = ev.Number
	d.openStartOffset = ev.StartOffset
	d.endLine = ev.Number
	d.endOffset = ev.EndOffset

	// Transition into the state matching the detected variant. Each
	// variant has its own close rule so they can't share a single state.
	switch v {
	case variantSegfault:
		d.st = stateInSegfaultTail
	case variantAsan:
		d.st = stateInAsan
	case variantKernel:
		d.st = stateInKernel
	case variantStackSmash:
		d.st = stateInStackSmash
	case variantNone:
		// unreachable — guarded by the early return above.
	}
}

// handleInSegfaultTail handles the stateInSegfaultTail branch: we just
// saw a `Segmentation fault ...` opener and need to decide whether the
// NEXT line is a standalone `(core dumped)` follow-up.
//
// If this line starts with `(core dumped)`, extend the span by one and
// close — the segfault region is at most two lines. Otherwise close
// immediately (the terse one-line case) and re-process the current
// line as if stateClosed so a back-to-back crash still opens.
func (d *Detector) handleInSegfaultTail(ev analyzer.LineEvent) {
	if bytes.HasPrefix(ev.Bytes, segfaultCoreDumpedPrefix) {
		// Extend to include the `(core dumped)` tail, then close.
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		d.emit()
		d.reset()
		return
	}
	// Segfault regions are 1 line by default; close immediately and
	// reprocess this line through tryOpen in case it's the start of
	// a new region (e.g. a second segfault back-to-back).
	d.emit()
	d.reset()
	d.tryOpen(ev)
}

// handleInAsan handles the stateInAsan branch. ASAN regions are closed
// by the `==PID==ABORTING` tail or a blank line. Any line that doesn't
// open a new (different-variant) region is treated as continuation —
// ASAN output is verbose and contains many shapes (frames, SUMMARY,
// READ/WRITE headers, Shadow bytes dumps, allocation traces).
func (d *Detector) handleInAsan(ev analyzer.LineEvent) {
	// Blank line: ASAN sections are newline-separated. The first blank
	// after the opener is typically AFTER the ABORTING tail, but if we
	// hit one before, close defensively.
	if len(ev.Bytes) == 0 {
		d.emit()
		d.reset()
		return
	}
	// ABORTING tail: include this line then close.
	if asanAbortingRe.Match(ev.Bytes) {
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		d.emit()
		d.reset()
		return
	}
	// Different-variant opener: close current and reopen. Rare but
	// possible (e.g. ASAN stack followed by a kernel oops in a noisy
	// CI log).
	if v := classifyOpener(ev.Bytes); v != variantNone && v != variantAsan {
		d.emit()
		d.reset()
		d.tryOpen(ev)
		return
	}
	// Treat anything else as continuation. We could be stricter and
	// require a recognized continuation shape (indented frame,
	// SUMMARY, READ/WRITE, Shadow bytes), but ASAN formatting is
	// version-dependent and over-specific rules would miss legitimate
	// sections. The ABORTING-or-blank close rule is conservative
	// enough — see the asanAbortingRe comment for the full rationale.
	d.endLine = ev.Number
	d.endOffset = ev.EndOffset
}

// handleInKernel handles the stateInKernel branch. Kernel Call Trace
// regions use bracketed timestamps `[   123.456]` on every
// continuation line. The first non-bracketed-timestamp line closes
// the region.
func (d *Detector) handleInKernel(ev analyzer.LineEvent) {
	// Blank line: close.
	if len(ev.Bytes) == 0 {
		d.emit()
		d.reset()
		return
	}
	// Bracketed-timestamp continuation. The `---[ end trace ... ]---`
	// tail IS a bracketed-timestamp line so it's naturally included.
	if kernelTimestampRe.Match(ev.Bytes) {
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		return
	}
	// Different-variant opener: close current and reopen.
	if v := classifyOpener(ev.Bytes); v != variantNone {
		d.emit()
		d.reset()
		d.tryOpen(ev)
		return
	}
	// Non-bracketed, non-opener line: region is done.
	d.emit()
	d.reset()
}

// handleInStackSmash handles the stateInStackSmash branch. The region
// runs from the banner through the `Aborted (core dumped)` line; the
// Backtrace and Memory map sections in between are all continuation.
func (d *Detector) handleInStackSmash(ev analyzer.LineEvent) {
	// Blank line: close. Stack-smashing output typically has no
	// internal blanks; a blank signals the dump is over.
	if len(ev.Bytes) == 0 {
		d.emit()
		d.reset()
		return
	}
	// `Aborted (core dumped)` tail: include and close. This is the
	// canonical terminator glibc emits after the dump.
	if bytes.HasPrefix(ev.Bytes, abortedCoreDumpedPrefix) {
		d.endLine = ev.Number
		d.endOffset = ev.EndOffset
		d.emit()
		d.reset()
		return
	}
	// Different-variant opener: close current and reopen.
	if v := classifyOpener(ev.Bytes); v != variantNone && v != variantStackSmash {
		d.emit()
		d.reset()
		d.tryOpen(ev)
		return
	}
	// Continuation. Stack-smashing dumps contain several recognizable
	// section headers (Backtrace, Memory map) plus free-form hex
	// lines; we accept everything until `Aborted (core dumped)` or
	// blank. Keeping the continuation check permissive mirrors
	// handleInAsan — these crash-dump formats are version-dependent
	// and we'd rather over-include than truncate mid-dump.
	d.endLine = ev.Number
	d.endOffset = ev.EndOffset
}

// classifyOpener returns the variant matched by the combined opener
// regex, or variantNone if no alternative matched. The combined regex
// runs once; we then re-test cheap literal prefixes on the match to
// pick the variant. Only runs on lines that already matched the
// combined regex, so the per-variant tests are pure dispatch — they
// cannot mis-classify a non-opener.
func classifyOpener(line []byte) variant {
	if !combinedOpenerRe.Match(line) {
		return variantNone
	}
	// Prefix checks in descending specificity. The combined regex
	// already anchored at `^` so these are all column-0 comparisons.
	if bytes.HasPrefix(line, []byte("Segmentation fault")) {
		return variantSegfault
	}
	if bytes.HasPrefix(line, []byte("*** stack smashing detected ***")) {
		return variantStackSmash
	}
	// ASAN and kernel start with `=` and `[` respectively — test the
	// cheap byte first before falling through.
	if len(line) > 0 && line[0] == '=' {
		return variantAsan
	}
	if len(line) > 0 && line[0] == '[' {
		return variantKernel
	}
	// Combined regex matched but none of the above classifications
	// fired. In practice this can't happen because every alternative
	// pins a unique leading sequence. Return variantNone to be safe.
	return variantNone
}

// Finalize is called once after the last OnLine. If we're in any
// in-region state at EOF, emit the pending region. A crash dump at the
// very tail of a truncated log is a legitimate signal.
//
// FlushContext is unused here; crash-dump detection is purely
// structural and doesn't depend on file-global stats.
func (d *Detector) Finalize(_ *analyzer.FlushContext) []analyzer.Anomaly {
	if d.st != stateClosed {
		d.emit()
		d.reset()
	}
	return d.out
}

// emit appends one anomaly for the currently-tracked region and leaves
// the state fields alone (reset does that separately). Description
// includes the variant name so humans (and the UI) can tell the four
// shapes apart in a mixed anomaly list.
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
		Description: fmt.Sprintf("%s crash dump, %d lines", variantName(d.v), lineCount),
	})
}

// variantName returns a short human name for the variant, used in the
// emitted Description. Keeps the four shapes distinguishable in UI
// lists without bloating the Anomaly struct with an extra field.
func variantName(v variant) string {
	switch v {
	case variantSegfault:
		return "Segmentation fault"
	case variantAsan:
		return "AddressSanitizer"
	case variantKernel:
		return "Kernel oops"
	case variantStackSmash:
		return "Stack smashing"
	case variantNone:
		return "unknown"
	}
	return "unknown"
}

// reset clears the current-region fields and returns the state machine
// to stateClosed. Does not touch the accumulated out slice.
func (d *Detector) reset() {
	d.v = variantNone
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
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/coredumpunix"
//
// Factory-based registration: every index build gets its own fresh
// Detector so region state cannot leak across builds.
func init() {
	analyzer.RegisterLineDetector(func() analyzer.LineDetector { return New() })
}
