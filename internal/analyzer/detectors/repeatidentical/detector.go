// Package repeatidentical implements the `repeat-identical` line detector.
//
// What it does:
//   - Flags runs of ≥ 5 consecutive identical lines.
//   - A "run" is broken the moment a line's content differs from the
//     previous line's content (byte-for-byte, including the indent bytes
//     but excluding the trailing newline, which the coordinator already
//     strips).
//   - On every run break (and again at Finalize for any still-open run)
//     the detector emits a single anomaly covering the span of the run.
//
// Why hash instead of keeping the previous line's bytes:
//   - The Window passed to OnLine exposes a BORROWED byte slice whose
//     underlying storage is reused between pushes. Comparing today's
//     line to yesterday's slice header would therefore compare the line
//     to itself once the window reused the slot.
//   - We could copy the previous line's bytes into detector state every
//     call, but that defeats the zero-alloc hot path of Window.push.
//     FNV-1a over the bytes gives us a cheap 64-bit fingerprint that we
//     can stash in a plain uint64 field — no allocation, no retained
//     slice. Hash collisions are theoretically possible but the payoff
//     (false positive: flagging two different-looking lines as part of a
//     run) is benign for a navigation-hint detector; severity is 0.4.
//
// State machine:
//
//	initial         runLen == 0
//	line 1 pushes   runLen = 1, hash = H(line1)
//	line 2 matches  runLen = 2
//	...
//	line N differs  if runLen >= minRunLength: emit anomaly covering
//	                lines [startLine..prevLine]. Then reopen the run with
//	                runLen = 1 anchored at line N (which becomes the new
//	                candidate for the next run).
//
// Registration: this package has an init() that calls analyzer.Register
// so a blank import in cmd/rx/main.go is enough to hook it up.
package repeatidentical

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/wlame/rx-go/internal/analyzer"
)

// Metadata constants — kept as a block at the top so /v1/detectors
// output is trivially auditable.
const (
	detectorName        = "repeat-identical"
	detectorVersion     = "0.1.0"
	detectorCategory    = "repetition"
	detectorDescription = "Consecutive identical lines"

	// minRunLength is the shortest run (in lines) that we report.
	// Anything shorter is common in log files ("   ", "---", etc.) and
	// would produce far more noise than signal.
	minRunLength = 5

	// severity is the plan-mandated value for this detector. Stored here
	// (not inline at the emit site) so it's obvious at a glance.
	severity = 0.4
)

// Detector implements both analyzer.FileAnalyzer (for registry
// enumeration) and analyzer.LineDetector (for the streaming scan).
//
// The two interfaces share metadata methods (Name/Version/...), so a
// single struct carries everything.
//
// State fields (runStartLine, runStartOffset, runEndOffset, runLen,
// runHash) describe the CURRENT open run. runLen == 0 means "no run
// currently open" and is the state we start in and return to after
// emitting an anomaly.
type Detector struct {
	// out accumulates emitted anomalies across the scan. Finalize
	// returns this slice; OnLine appends to it whenever a run breaks.
	out []analyzer.Anomaly

	// runLen is the length of the currently-open run in lines.
	// Zero = no run open.
	runLen int

	// runStartLine / runStartOffset identify the first line of the
	// currently-open run. We use these as the anomaly's start.
	runStartLine   int64
	runStartOffset int64

	// runEndOffset is the byte offset AFTER the last matching line of
	// the run — updated on every continuation so that when the run
	// finally breaks we have the correct end without re-reading.
	runEndOffset int64

	// runEndLine is the line number of the last matching line. Kept
	// separate from runEndOffset because the two come from different
	// LineEvent fields and carrying both avoids a trailing-line recompute
	// at emit time.
	runEndLine int64

	// runHash is the FNV-1a 64-bit fingerprint of the run's content
	// (identical by construction for every line in the run). Compared
	// against the next line's hash on every OnLine.
	runHash uint64
}

// New returns a freshly-initialized Detector. Callers that want a
// specific detector instance (tests, manual wiring) can use this
// directly; the registry uses the init() registration below.
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

// Supports says yes to anything text-shaped. The coordinator passes each
// line's IsBinary flag separately, so a binary-heavy file just becomes a
// no-op for this detector at the per-line level.
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
// The hash-compare logic:
//   - If we have no open run (runLen == 0), start one anchored at this
//     line and remember its hash.
//   - If the current line's hash matches the open run's hash, extend
//     the run: bump the length, update the end offset / end line.
//   - Otherwise the run has broken. If it was long enough (≥ minRunLength)
//     emit an anomaly covering the span. Either way, reset the state to
//     a new one-line run anchored at the current line — this line is
//     the first candidate member of the NEXT potential run, not the last
//     member of the old one.
func (d *Detector) OnLine(w *analyzer.Window) {
	ev := w.Current()

	// Compute the hash once per OnLine. fnv.New64a is a cheap stack-
	// frame-only allocation (a 2-field struct); Write on a []byte does
	// not allocate.
	h := fnv.New64a()
	// hash.Hash's Write never returns a non-nil error; ignore it.
	_, _ = h.Write(ev.Bytes)
	lineHash := h.Sum64()

	if d.runLen == 0 {
		// No run open — start one anchored at this line.
		d.openRun(ev, lineHash)
		return
	}

	if lineHash == d.runHash {
		// Continuation: extend the run.
		d.runLen++
		d.runEndOffset = ev.EndOffset
		d.runEndLine = ev.Number
		return
	}

	// Run breaks. Emit if it was long enough, then start a new run
	// at the CURRENT line (this line is never part of the previous run).
	d.flushRunIfLongEnough()
	d.openRun(ev, lineHash)
}

// Finalize is called once after the last OnLine. If a run is still open
// and qualifies, emit it now — otherwise we'd miss runs that end at EOF.
//
// The FlushContext is unused here (repeat-identical doesn't care about
// file-global stats) but we accept it to satisfy the interface.
func (d *Detector) Finalize(_ *analyzer.FlushContext) []analyzer.Anomaly {
	d.flushRunIfLongEnough()
	return d.out
}

// openRun resets state to a new single-line run anchored at ev with
// the given hash. Shared between the "start of scan" branch and the
// "run broke, start next one" branch of OnLine.
func (d *Detector) openRun(ev analyzer.LineEvent, hash uint64) {
	d.runLen = 1
	d.runStartLine = ev.Number
	d.runStartOffset = ev.StartOffset
	d.runEndOffset = ev.EndOffset
	d.runEndLine = ev.Number
	d.runHash = hash
}

// flushRunIfLongEnough appends one anomaly to d.out IF the currently-
// tracked run is long enough, then resets the run state.
//
// Callers should reopen a new run (if appropriate) AFTER calling this;
// it does NOT open a new run itself — the two-step split keeps OnLine
// readable.
func (d *Detector) flushRunIfLongEnough() {
	if d.runLen >= minRunLength {
		d.out = append(d.out, analyzer.Anomaly{
			StartLine:   d.runStartLine,
			EndLine:     d.runEndLine,
			StartOffset: d.runStartOffset,
			EndOffset:   d.runEndOffset,
			Severity:    severity,
			// Semantic category — overwritten with detector name by the
			// coordinator's Finalize. Keeping a meaningful value here
			// helps when a detector emits anomalies outside the
			// coordinator path (tests, future direct callers).
			Category:    detectorCategory,
			Description: fmt.Sprintf("%d consecutive identical lines", d.runLen),
		})
	}
	// Reset even when we didn't emit so the next openRun starts clean.
	d.runLen = 0
	d.runHash = 0
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
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/repeatidentical"
//
// The registered instance is intentionally a single shared one — the
// coordinator is currently sequential, so sharing the instance across
// builds is safe today. When the builder becomes chunk-parallel, each
// worker must instantiate its own Detector (via New) so per-run state
// doesn't leak across workers; the registry entry then becomes a
// "factory prototype" rather than a live instance. That refactor lives
// with the chunk-parallel task, not here.
func init() {
	analyzer.Register(New())
}
