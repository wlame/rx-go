package index

// Integration tests for the analyzer coordinator wired into index.Build
// (plan task 5). These sit alongside builder_test.go so they share the
// writeTempFile helper; a separate file keeps the analyzer-specific
// wiring obviously attributable to this task.
//
// What's covered here:
//
//   - Build(Analyze=true) with a mock detector runs ProcessLine once per
//     line AND produces anomalies on matching cue lines.
//   - Build(Analyze=false) skips the coordinator entirely — no ProcessLine
//     calls, no Anomalies pointer set (null on the wire).
//   - FlushContext fields (total lines, median, P99) match the index's
//     own computed line-length stats.
//   - Offsets reported in anomalies align with what the line-index uses
//     (start_offset == line's byte position, end_offset == next line's
//     byte position = start + len(line) + len(terminator)).
//   - Multi-line anomalies (cue + continuation) survive the full round
//     trip through ProcessLine → Finalize → Deduplicate → AnomalyRangeResult.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/analyzer"
)

// cueDetector is the mock LineDetector used by the integration tests.
// It emits one anomaly per line whose content starts with a configured
// "cue" byte-slice. That's enough to verify the coordinator dispatches
// every line AND that detector-emitted anomalies reach the index.
//
// Kept in _test.go (not shared with the detectors package) because the
// detector catalog is added task-by-task later in the plan — we don't
// want premature coupling here.
type cueDetector struct {
	name    string
	cue     string
	version string

	processCalls atomic.Int64
	finalizeHit  atomic.Int32
	flushSeen    *analyzer.FlushContext

	// emitted is the list of anomalies this detector produced, captured
	// so tests can assert per-detector attribution before dedup mangles
	// the view. We record them during OnLine so a chain of asserts can
	// inspect the pre-aggregation state.
	emitted []analyzer.Anomaly
}

func (c *cueDetector) Name() string        { return c.name }
func (c *cueDetector) Version() string     { return c.version }
func (c *cueDetector) Category() string    { return "test-cue" }
func (c *cueDetector) Description() string { return "mock detector for builder tests" }
func (c *cueDetector) Supports(_, _ string, _ int64) bool {
	return true
}
func (c *cueDetector) Analyze(_ context.Context, _ analyzer.Input) (*analyzer.Report, error) {
	return &analyzer.Report{Name: c.name, Version: c.version, SchemaVersion: 1}, nil
}

func (c *cueDetector) OnLine(w *analyzer.Window) {
	c.processCalls.Add(1)
	ev := w.Current()
	if !bytes.HasPrefix(ev.Bytes, []byte(c.cue)) {
		return
	}
	// Emit immediately during OnLine so Finalize has work to return.
	// We set the semantic Category so we can verify the coordinator
	// overwrites it with Name() before storage.
	c.emitted = append(c.emitted, analyzer.Anomaly{
		StartLine:   ev.Number,
		EndLine:     ev.Number,
		StartOffset: ev.StartOffset,
		EndOffset:   ev.EndOffset,
		Severity:    0.5,
		Category:    "semantic-category", // will be overwritten with Name()
		Description: fmt.Sprintf("%s match at line %d", c.name, ev.Number),
	})
}

func (c *cueDetector) Finalize(flush *analyzer.FlushContext) []analyzer.Anomaly {
	c.finalizeHit.Add(1)
	c.flushSeen = flush
	// Return a defensive copy — prevents the coordinator's Category
	// overwrite from stomping on the test's captured `emitted` view.
	out := make([]analyzer.Anomaly, len(c.emitted))
	copy(out, c.emitted)
	return out
}

// Compile-time interface check — catches drift between the test mock
// and the real LineDetector contract.
var _ analyzer.LineDetector = (*cueDetector)(nil)

// TestBuild_Analyze_WithDetector_EmitsAnomalies is the happy-path wiring
// test. A file with three cue lines interspersed with non-cue lines
// should produce exactly three anomalies in the index, with the Detector
// field pointing at the mock detector's name.
func TestBuild_Analyze_WithDetector_EmitsAnomalies(t *testing.T) {
	// 6 lines total: lines 2, 4, 6 match the "ERROR:" cue.
	// Each non-matching line is "ok\n" (3 bytes); cue lines are "ERROR: X\n" (9 bytes).
	content := "ok\nERROR: A\nok\nERROR: B\nok\nERROR: C\n"
	p := writeTempFile(t, content)

	det := &cueDetector{name: "mock-cue", version: "0.1.0", cue: "ERROR:"}
	idx, err := Build(p, BuildOptions{
		Analyze:   true,
		Detectors: []analyzer.LineDetector{det},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Every line hit the coordinator — ProcessLine count == total lines.
	// Total lines in the fixture = 6.
	if got := det.processCalls.Load(); got != 6 {
		t.Errorf("ProcessLine calls = %d, want 6 (one per line)", got)
	}
	// Finalize runs exactly once per worker — single-worker means once.
	if got := det.finalizeHit.Load(); got != 1 {
		t.Errorf("Finalize invocations = %d, want 1", got)
	}

	// Three anomalies reach the index.
	if idx.Anomalies == nil {
		t.Fatal("idx.Anomalies should be populated for Analyze=true")
	}
	anomalies := *idx.Anomalies
	if len(anomalies) != 3 {
		t.Fatalf("got %d anomalies, want 3", len(anomalies))
	}

	// Verify each anomaly points at the detector (Detector field set) AND
	// the legacy Category field was rewritten to the detector's Name()
	// (see coordinator.Finalize rationale).
	wantLines := []int64{2, 4, 6}
	for i, a := range anomalies {
		if a.StartLine != wantLines[i] || a.EndLine != wantLines[i] {
			t.Errorf("anomaly[%d]: start=%d end=%d, want both = %d",
				i, a.StartLine, a.EndLine, wantLines[i])
		}
		if a.Detector != "mock-cue" {
			t.Errorf("anomaly[%d].Detector = %q, want %q", i, a.Detector, "mock-cue")
		}
		// Post-coordinator Category is the detector name, NOT the
		// detector's semantic Category(). This matches the dedup
		// contract (Task 4 keys on Category).
		if a.Category != "mock-cue" {
			t.Errorf("anomaly[%d].Category = %q, want %q (detector name)", i, a.Category, "mock-cue")
		}
	}

	// Offset sanity: line 2 starts at byte 3 ("ok\n" consumed); line 4 at
	// byte 3 + 9 + 3 = 15; line 6 at 15 + 9 + 3 = 27.
	wantStarts := []int64{3, 15, 27}
	for i, a := range anomalies {
		if a.StartOffset != wantStarts[i] {
			t.Errorf("anomaly[%d].StartOffset = %d, want %d", i, a.StartOffset, wantStarts[i])
		}
		// End offset = start + line length incl. terminator (9 bytes).
		if a.EndOffset != wantStarts[i]+9 {
			t.Errorf("anomaly[%d].EndOffset = %d, want %d", i, a.EndOffset, wantStarts[i]+9)
		}
	}

	// AnomalySummary counts by detector name.
	if idx.AnomalySummary["mock-cue"] != 3 {
		t.Errorf("AnomalySummary[mock-cue] = %d, want 3", idx.AnomalySummary["mock-cue"])
	}
}

// TestBuild_Analyze_DisabledSkipsCoordinator confirms the no-regression
// guarantee: with Analyze=false, the coordinator is not created, so
// detectors' ProcessLine / Finalize hooks never fire. Anomalies on the
// index stays nil (serializes to JSON null, matching the pre-task-5
// behavior).
func TestBuild_Analyze_DisabledSkipsCoordinator(t *testing.T) {
	p := writeTempFile(t, "ok\nERROR: A\nok\n")

	det := &cueDetector{name: "mock", version: "0.1.0", cue: "ERROR:"}
	idx, err := Build(p, BuildOptions{
		Analyze:   false, // <-- the important bit
		Detectors: []analyzer.LineDetector{det},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if det.processCalls.Load() != 0 {
		t.Errorf("ProcessLine was called (%d times) when Analyze=false", det.processCalls.Load())
	}
	if det.finalizeHit.Load() != 0 {
		t.Errorf("Finalize was called when Analyze=false")
	}
	if idx.Anomalies != nil {
		t.Errorf("idx.Anomalies = %v, want nil (Analyze=false should leave the field null)", idx.Anomalies)
	}
	if idx.AnomalySummary != nil {
		t.Errorf("idx.AnomalySummary = %v, want nil (Analyze=false)", idx.AnomalySummary)
	}
}

// TestBuild_Analyze_NoDetectorsIsEmpty covers the edge case of Analyze=true
// but no detectors provided. The coordinator's zero-detector fast path
// should run, emit no anomalies, and the builder should publish an empty
// (non-nil) slice — matching Python's "analysis_performed=true but nothing
// found" serialization.
func TestBuild_Analyze_NoDetectorsIsEmpty(t *testing.T) {
	p := writeTempFile(t, "line-a\nline-b\n")

	idx, err := Build(p, BuildOptions{Analyze: true, Detectors: nil})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if idx.Anomalies == nil {
		t.Fatal("idx.Anomalies should be non-nil when Analyze=true (even if empty)")
	}
	if len(*idx.Anomalies) != 0 {
		t.Errorf("got %d anomalies, want 0", len(*idx.Anomalies))
	}
	// Summary map is allocated but empty.
	if idx.AnomalySummary == nil {
		t.Fatal("AnomalySummary should be non-nil when Analyze=true")
	}
	if len(idx.AnomalySummary) != 0 {
		t.Errorf("AnomalySummary = %v, want empty", idx.AnomalySummary)
	}
}

// TestBuild_Analyze_FlushContextMatchesIndexStats verifies the coordinator
// receives a FlushContext whose fields match the index's own computed
// line-length statistics. This is the wiring contract long-line (Task 8)
// will depend on: it reads Median and P99 from the FlushContext and must
// see the same values the builder publishes.
func TestBuild_Analyze_FlushContextMatchesIndexStats(t *testing.T) {
	// Build a file with a varied line-length distribution so the
	// percentile fields are meaningful. Lines are 10..109 bytes of
	// content (plus newline) = 100 distinct lengths.
	var buf bytes.Buffer
	for i := 0; i < 100; i++ {
		buf.WriteString(strings.Repeat("x", 10+i))
		buf.WriteByte('\n')
	}
	p := writeTempFile(t, buf.String())

	det := &cueDetector{name: "flush-witness", version: "0.1.0", cue: "__never_matches__"}
	idx, err := Build(p, BuildOptions{
		Analyze:   true,
		Detectors: []analyzer.LineDetector{det},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if det.flushSeen == nil {
		t.Fatal("detector never observed a FlushContext")
	}

	// TotalLines matches the index's LineCount.
	if idx.LineCount == nil || det.flushSeen.TotalLines != *idx.LineCount {
		t.Errorf("FlushContext.TotalLines = %d, LineCount = %v",
			det.flushSeen.TotalLines, idx.LineCount)
	}
	// Median matches int-truncated index median. We truncate because the
	// FlushContext field is int64 while the index stores float64; the
	// builder documents that conversion.
	if idx.LineLengthMedian == nil || det.flushSeen.MedianLineLength != int64(*idx.LineLengthMedian) {
		t.Errorf("FlushContext.MedianLineLength = %d, LineLengthMedian = %v",
			det.flushSeen.MedianLineLength, idx.LineLengthMedian)
	}
	if idx.LineLengthP99 == nil || det.flushSeen.P99LineLength != int64(*idx.LineLengthP99) {
		t.Errorf("FlushContext.P99LineLength = %d, LineLengthP99 = %v",
			det.flushSeen.P99LineLength, idx.LineLengthP99)
	}
}

// TestBuild_Analyze_DedupCollapsesDuplicates verifies the Deduplicate
// wiring is in the code path. We simulate "same anomaly emitted twice"
// by using a detector that emits an extra duplicate of each line's
// anomaly — the duplicate must collapse to one in the final index.
//
// This is the stand-in for the plan's "anomalies straddling chunk
// boundaries are detected exactly once" assertion: today the builder is
// single-worker so there are no chunk boundaries, but the dedup path
// MUST still run so the future chunk-parallel build stays correct.
func TestBuild_Analyze_DedupCollapsesDuplicates(t *testing.T) {
	p := writeTempFile(t, "ok\nERROR: A\nok\n")

	det := &duplicateEmitter{cueDetector: cueDetector{name: "dup", version: "0.1.0", cue: "ERROR:"}}
	idx, err := Build(p, BuildOptions{
		Analyze:   true,
		Detectors: []analyzer.LineDetector{det},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.Anomalies == nil {
		t.Fatal("Anomalies pointer not populated")
	}
	// duplicateEmitter emits the same anomaly TWICE per match; Deduplicate
	// must collapse them on (Category, start_offset, end_offset).
	if len(*idx.Anomalies) != 1 {
		t.Errorf("got %d anomalies after dedup, want 1", len(*idx.Anomalies))
	}
}

// duplicateEmitter emits two identical anomalies per match — exercises
// the Deduplicate branch of the builder. Kept separate from cueDetector
// so the "normal" tests above don't have to work around double-emit.
type duplicateEmitter struct {
	cueDetector
}

func (d *duplicateEmitter) OnLine(w *analyzer.Window) {
	ev := w.Current()
	if !bytes.HasPrefix(ev.Bytes, []byte(d.cue)) {
		return
	}
	a := analyzer.Anomaly{
		StartLine:   ev.Number,
		EndLine:     ev.Number,
		StartOffset: ev.StartOffset,
		EndOffset:   ev.EndOffset,
		Severity:    0.5,
		Category:    "semantic",
		Description: "dup",
	}
	// Emit twice — second identical entry should be collapsed by
	// analyzer.Deduplicate during Build's post-walk phase.
	d.emitted = append(d.emitted, a, a)
}

func (d *duplicateEmitter) Finalize(flush *analyzer.FlushContext) []analyzer.Anomaly {
	d.flushSeen = flush
	out := make([]analyzer.Anomaly, len(d.emitted))
	copy(out, d.emitted)
	return out
}

var _ analyzer.LineDetector = (*duplicateEmitter)(nil)

// TestBuild_Analyze_MultiLineAnomalySpan covers the common detector
// shape: an opening cue plus one or more continuation lines that form
// a single anomaly whose end_offset is AFTER the closing line. We want
// to confirm nothing in the builder mangles end offsets greater than
// the opening line's own end.
func TestBuild_Analyze_MultiLineAnomalySpan(t *testing.T) {
	// 4 lines. Line 2 ("OPEN") opens a pattern that closes at line 3
	// ("CLOSE"). The detector emits one anomaly spanning lines 2-3.
	content := "ok\nOPEN\nCLOSE\nok\n"
	p := writeTempFile(t, content)

	det := &openCloseDetector{name: "oc", version: "0.1.0"}
	idx, err := Build(p, BuildOptions{
		Analyze:   true,
		Detectors: []analyzer.LineDetector{det},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if idx.Anomalies == nil || len(*idx.Anomalies) != 1 {
		t.Fatalf("want 1 anomaly, got %v", idx.Anomalies)
	}
	a := (*idx.Anomalies)[0]
	if a.StartLine != 2 || a.EndLine != 3 {
		t.Errorf("span lines: start=%d end=%d, want 2..3", a.StartLine, a.EndLine)
	}
	// Line 2 "OPEN" starts at byte 3 ("ok\n"). Line 3 "CLOSE" ends at
	// 3 (ok\n) + 5 (OPEN\n) + 6 (CLOSE\n) = 14.
	if a.StartOffset != 3 {
		t.Errorf("StartOffset = %d, want 3", a.StartOffset)
	}
	if a.EndOffset != 14 {
		t.Errorf("EndOffset = %d, want 14", a.EndOffset)
	}
}

// openCloseDetector is a tiny state-machine detector used to exercise
// multi-line anomaly emission. It opens on "OPEN" and closes on the
// next line starting with "CLOSE", emitting one anomaly per open/close
// pair with byte offsets spanning the range.
type openCloseDetector struct {
	name    string
	version string

	// State machine fields. "open" is the currently-open event, if any.
	open         *openState
	emitted      []analyzer.Anomaly
	flushedSeen  *analyzer.FlushContext
	finalizeHits atomic.Int32
}

type openState struct {
	startLine, startOffset int64
}

func (d *openCloseDetector) Name() string        { return d.name }
func (d *openCloseDetector) Version() string     { return d.version }
func (d *openCloseDetector) Category() string    { return "test" }
func (d *openCloseDetector) Description() string { return "open/close test detector" }
func (d *openCloseDetector) Supports(_, _ string, _ int64) bool {
	return true
}
func (d *openCloseDetector) Analyze(_ context.Context, _ analyzer.Input) (*analyzer.Report, error) {
	return &analyzer.Report{Name: d.name, Version: d.version, SchemaVersion: 1}, nil
}

func (d *openCloseDetector) OnLine(w *analyzer.Window) {
	ev := w.Current()
	switch {
	case bytes.HasPrefix(ev.Bytes, []byte("OPEN")):
		d.open = &openState{startLine: ev.Number, startOffset: ev.StartOffset}
	case d.open != nil && bytes.HasPrefix(ev.Bytes, []byte("CLOSE")):
		d.emitted = append(d.emitted, analyzer.Anomaly{
			StartLine:   d.open.startLine,
			EndLine:     ev.Number,
			StartOffset: d.open.startOffset,
			EndOffset:   ev.EndOffset,
			Severity:    0.5,
			Category:    "semantic",
			Description: "open/close pair",
		})
		d.open = nil
	}
}

func (d *openCloseDetector) Finalize(flush *analyzer.FlushContext) []analyzer.Anomaly {
	d.finalizeHits.Add(1)
	d.flushedSeen = flush
	out := make([]analyzer.Anomaly, len(d.emitted))
	copy(out, d.emitted)
	return out
}

var _ analyzer.LineDetector = (*openCloseDetector)(nil)

// TestBuild_Analyze_ZeroWindowLinesFallsBackToDefault confirms that the
// BuildOptions.WindowLines = 0 case does NOT panic and produces a
// usable coordinator — the resolver supplies a default. This is the
// "don't force every test or call site to specify a window size" ergonomics
// check.
func TestBuild_Analyze_ZeroWindowLinesFallsBackToDefault(t *testing.T) {
	p := writeTempFile(t, "ok\nERROR: A\nok\n")

	det := &cueDetector{name: "default-w", version: "0.1.0", cue: "ERROR:"}
	idx, err := Build(p, BuildOptions{
		Analyze:     true,
		WindowLines: 0, // <-- not set; resolver picks the default
		Detectors:   []analyzer.LineDetector{det},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.Anomalies == nil || len(*idx.Anomalies) != 1 {
		t.Errorf("expected 1 anomaly with default window size, got %v", idx.Anomalies)
	}
}

// TestBuild_Analyze_ExplicitWindowLinesIsRespected confirms that a
// caller-supplied WindowLines flows through to the coordinator. We
// can't observe the window size directly from outside the coordinator,
// but we can confirm the build still works with a tiny (1) window —
// the coordinator's NewWindow clamps to at least 1, so this is the
// lower bound for meaningful use.
func TestBuild_Analyze_ExplicitWindowLinesIsRespected(t *testing.T) {
	p := writeTempFile(t, "ok\nERROR: A\nok\nERROR: B\nok\n")

	det := &cueDetector{name: "win1", version: "0.1.0", cue: "ERROR:"}
	idx, err := Build(p, BuildOptions{
		Analyze:     true,
		WindowLines: 1,
		Detectors:   []analyzer.LineDetector{det},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The cue detector doesn't look back in the window (only Current()),
	// so window size = 1 is still enough for it to emit anomalies.
	if idx.Anomalies == nil || len(*idx.Anomalies) != 2 {
		t.Errorf("expected 2 anomalies with window=1, got %v", idx.Anomalies)
	}
}
