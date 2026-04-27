// Package longline implements the `long-line` line detector.
//
// What it does:
//
//   - Flags individual lines whose length in bytes is unusually large
//     compared to the file's own distribution of line lengths.
//
//   - The "unusually large" threshold is dynamic and file-relative: it's
//     the largest of three candidates:
//
//     threshold = max(P99 + 512, 4 * median, 1024)
//
//     P99 and median are taken from FlushContext (populated by the index
//     builder from the already-computed line-stats accumulator).
//
//   - Severity 0.3: this is a navigation hint, not a verdict. Long lines
//     are common in logs (JSON blobs, stack traces serialized onto one
//     line) but still useful to jump to when scrolling a huge file.
//
// Strategy: BUFFER-THEN-FINALIZE, not streaming.
//
//   - A naive streaming design would need P99/median at OnLine time, but
//     those are only known AFTER the full scan. We can't emit as we go.
//
//   - A naive "buffer every line" design would cost (line_count *
//     tuple_size) memory — on a 10 GB file with 100 M lines that's a few
//     gigabytes of overhead just for the detector.
//
//   - Observation: the final threshold is `max(P99 + 512, 4 * median,
//     1024)`. All three candidates are AT LEAST 1024. So any line
//     shorter than 1024 bytes can NEVER qualify, regardless of what P99
//     and median turn out to be. We can safely ignore those lines at
//     OnLine time and only buffer the rare ≥1024 byte candidates.
//
//   - In Finalize, we compute the final threshold from FlushContext and
//     emit anomalies for buffered lines whose length is at or above that
//     final threshold. The other buffered lines (≥1024 but below the
//     dynamic threshold on this particular file) are simply discarded.
//
// Memory footprint in the worst case is bounded by
// `(count of 1KB+ lines) * sizeof(candidate)` which is itself bounded
// above by `filesize / 1024 * ~32` bytes. For realistic log files with
// very few very-long lines, the buffer stays tiny.
//
// Registration: this package has an init() that calls analyzer.Register.
// Blank-importing the package in cmd/rx/main.go is enough to hook it up.
package longline

import (
	"context"
	"fmt"

	"github.com/wlame/rx-go/internal/analyzer"
)

// Metadata constants — top of file so /v1/detectors output is trivial to
// audit against the plan.
const (
	detectorName        = "long-line"
	detectorVersion     = "0.1.0"
	detectorCategory    = "format"
	detectorDescription = "Lines unusually long relative to the file's length distribution"

	// staticFloor is the lower bound of the dynamic threshold. Lines
	// strictly shorter than this cannot qualify under ANY of the three
	// threshold branches:
	//
	//   - `1024` branch: a line below 1024 is obviously below 1024.
	//   - `P99 + 512` branch: P99 is non-negative, so `P99 + 512 >= 512`.
	//     But we ALSO combine it with the 1024 floor below, so the
	//     effective threshold is always `>= 1024`.
	//   - `4 * median` branch: median is non-negative, so this is `>= 0`.
	//     Combined with the 1024 floor, the effective threshold is
	//     always `>= 1024`.
	//
	// Therefore `staticFloor == 1024` is the correct, sound cutoff for
	// the "line is worth buffering" test.
	staticFloor = 1024

	// p99Bonus is how far above P99 a line must sit under the "P99 + x"
	// branch of the threshold. Picked so that a line just slightly
	// larger than the file's 99th percentile doesn't become noise — we
	// want the detector to surface outliers, not the tail of the normal
	// distribution.
	p99Bonus = 512

	// medianMultiplier is the factor applied to the median under the
	// "k * median" branch. For files where the median is small but the
	// P99 happens to be close to the median (very flat distribution),
	// this branch still produces a sensible ceiling.
	medianMultiplier = 4

	// severity is the plan-mandated value for this detector. Navigation
	// hint, not a verdict.
	severity = 0.3
)

// candidate is one buffered long-line candidate. We store only the
// information we need to emit an Anomaly later; we intentionally do NOT
// copy the line bytes (saving memory). Line content can be recovered via
// the file's line index + offsets if a consumer ever needs it.
//
// Layout is kept compact — five int64s = 40 bytes per candidate — so
// the buffer stays small even on files with thousands of long lines.
type candidate struct {
	number      int64
	startOffset int64
	endOffset   int64
	length      int64
}

// Detector implements both analyzer.FileAnalyzer and
// analyzer.LineDetector. A single type carries all the metadata and
// both per-line and finalize behavior.
//
// State is only the buffered candidate list — no streaming state to
// track since emissions happen exclusively in Finalize.
type Detector struct {
	// buf holds candidate tuples for every line seen so far that is at
	// or above staticFloor bytes. Finalize filters this list against
	// the dynamic threshold computed from FlushContext.
	buf []candidate
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

// Category returns the human-readable bucket name.
func (d *Detector) Category() string { return detectorCategory }

// Description returns the one-line human summary.
func (d *Detector) Description() string { return detectorDescription }

// Supports says yes to anything — line length is meaningful regardless
// of mime type. Binary files are rarely indexed via this path anyway.
func (d *Detector) Supports(_ string, _ string, _ int64) bool {
	return true
}

// Analyze is the FileAnalyzer entry point. Real work happens through
// the coordinator's OnLine/Finalize path; Analyze exists to satisfy the
// FileAnalyzer interface for registry enumeration.
func (d *Detector) Analyze(_ context.Context, _ analyzer.Input) (*analyzer.Report, error) {
	return &analyzer.Report{
		Name:          detectorName,
		Version:       detectorVersion,
		SchemaVersion: 1,
		Result:        map[string]any{},
	}, nil
}

// OnLine is the streaming-scan hook. We do NOT emit here; we only buffer
// candidates whose byte length is at or above staticFloor.
//
// LineEvent.Bytes is borrowed storage (see LineEvent docs), so we never
// store a reference to it. Copying `len(ev.Bytes)` into an int64 is
// safe because len is a plain integer, not a slice header.
func (d *Detector) OnLine(w *analyzer.Window) {
	ev := w.Current()
	length := int64(len(ev.Bytes))
	if length < staticFloor {
		// Under ANY possible final threshold this line can't qualify.
		// Skip without buffering to keep memory bounded.
		return
	}
	d.buf = append(d.buf, candidate{
		number:      ev.Number,
		startOffset: ev.StartOffset,
		endOffset:   ev.EndOffset,
		length:      length,
	})
}

// Finalize computes the final dynamic threshold from the file-global
// stats in FlushContext, then emits one anomaly per buffered line at or
// above that threshold.
//
// A nil FlushContext is tolerated (treated as P99=0, median=0): the
// threshold collapses to the static floor (1024 bytes), so every
// buffered candidate becomes an anomaly. This matters for unit tests
// that drive the detector without a real coordinator/index build.
func (d *Detector) Finalize(flush *analyzer.FlushContext) []analyzer.Anomaly {
	threshold := computeThreshold(flush)

	// Pre-size the output slice to the worst case (every buffered
	// candidate qualifies) to avoid repeated grow-and-copy in the
	// common "most candidates do qualify" case. Over-allocating by a
	// few entries is fine; the slice is short-lived.
	out := make([]analyzer.Anomaly, 0, len(d.buf))
	for _, c := range d.buf {
		if c.length < threshold {
			continue
		}
		out = append(out, analyzer.Anomaly{
			StartLine:   c.number,
			EndLine:     c.number,
			StartOffset: c.startOffset,
			EndOffset:   c.endOffset,
			Severity:    severity,
			// Semantic category — this is the stable wire-contract
			// `category` field. The coordinator stamps Anomaly.DetectorName
			// separately and leaves Category alone.
			Category:    detectorCategory,
			Description: fmt.Sprintf("line is %d bytes (threshold %d)", c.length, threshold),
		})
	}
	return out
}

// computeThreshold implements the `max(P99 + 512, 4 * median, 1024)`
// formula. Extracted into a standalone function so unit tests can pin
// down its behavior directly without constructing a whole Detector.
//
// If flush is nil (no file-global stats available), both percentiles
// are treated as zero and the formula collapses to the 1024 floor.
func computeThreshold(flush *analyzer.FlushContext) int64 {
	var p99, median int64
	if flush != nil {
		p99 = flush.P99LineLength
		median = flush.MedianLineLength
	}

	threshold := int64(staticFloor)
	if cand := p99 + p99Bonus; cand > threshold {
		threshold = cand
	}
	if cand := medianMultiplier * median; cand > threshold {
		threshold = cand
	}
	return threshold
}

// Compile-time interface conformance checks. If either contract drifts
// we want the build to fail here rather than somewhere in wiring.
var (
	_ analyzer.FileAnalyzer = (*Detector)(nil)
	_ analyzer.LineDetector = (*Detector)(nil)
)

// init registers a detector FACTORY with the global analyzer registry.
// Callers activate the detector by blank-importing this package in
// cmd/rx/main.go:
//
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/longline"
//
// Why factory-based registration (not a shared instance): every index
// build gets a fresh Detector so the buffered candidate slice cannot
// leak across files in a multi-file `rx index` invocation or across
// sequential HTTP builds.
func init() {
	analyzer.RegisterLineDetector(func() analyzer.LineDetector { return New() })
}
