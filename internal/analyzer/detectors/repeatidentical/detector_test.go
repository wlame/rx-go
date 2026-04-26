package repeatidentical

// Unit and integration tests for the repeat-identical detector.
//
// Two layers:
//
//  1. Unit: drive a fresh Detector through simulated Window pushes via
//     a tiny helper, assert on the emitted anomaly list. Covers the
//     state-machine branches (zero runs, qualifying run, short run,
//     multiple disjoint runs, run at EOF).
//
//  2. End-to-end: write a temp file, call index.Build with Analyze=true
//     and the detector in BuildOptions.Detectors, assert that the final
//     UnifiedFileIndex.Anomalies contains the expected entries. This
//     verifies the init() registration wiring is compatible with the
//     coordinator's Finalize behavior (the coordinator overwrites
//     Category with detector Name()).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/index"
)

// feed is the test helper that drives a fresh Detector through the
// real Coordinator plumbing, then returns the detector's own Finalize
// output.
//
// Why route through a Coordinator: analyzer.Window.push is unexported,
// so tests outside the analyzer package can't push directly. The
// Coordinator is the only path that pushes into the Window, so using
// it here exactly mirrors what index.Build does in production.
//
// Why call d.Finalize directly instead of coord.Finalize: the
// coordinator overwrites each anomaly's Category with the detector
// Name() before returning (see analyzer.Coordinator.Finalize rationale).
// For the unit tests we want to verify the SEMANTIC Category the
// detector emits, so we bypass that rewrite here. The end-to-end test
// below asserts the coordinator's rewrite behavior separately.
//
// Offsets are laid out as consecutive lines with a single '\n'
// terminator between them, i.e. the total file content would be
// strings.Join(lines, "\n") + "\n".
func feed(t *testing.T, lines []string) []analyzer.Anomaly {
	t.Helper()

	d := New()
	coord := analyzer.NewCoordinator(16, []analyzer.LineDetector{d})

	var offset int64
	for i, line := range lines {
		start := offset
		end := start + int64(len(line)) + 1 // +1 for the '\n' terminator
		offset = end
		coord.ProcessLine(int64(i+1), start, end, []byte(line))
	}
	return d.Finalize(nil)
}

// TestDetector_NoLines covers the trivial empty-file case: no OnLine
// calls, Finalize must return zero anomalies.
func TestDetector_NoLines(t *testing.T) {
	got := feed(t, nil)
	if len(got) != 0 {
		t.Errorf("empty input: got %d anomalies, want 0", len(got))
	}
}

// TestDetector_SingleQualifyingRun exercises the happy path: a run of
// exactly minRunLength identical lines produces one anomaly spanning
// the run.
func TestDetector_SingleQualifyingRun(t *testing.T) {
	// 5 identical lines, exactly minRunLength.
	lines := []string{"same", "same", "same", "same", "same"}

	got := feed(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1", len(got))
	}
	a := got[0]
	if a.StartLine != 1 || a.EndLine != 5 {
		t.Errorf("span: start=%d end=%d, want 1..5", a.StartLine, a.EndLine)
	}
	// "same\n" is 5 bytes; 5 lines total = 25 bytes.
	if a.StartOffset != 0 || a.EndOffset != 25 {
		t.Errorf("offsets: start=%d end=%d, want 0..25", a.StartOffset, a.EndOffset)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if a.Category != detectorCategory {
		t.Errorf("category = %q, want %q", a.Category, detectorCategory)
	}
	if !strings.Contains(a.Description, "5 consecutive") {
		t.Errorf("description = %q, want it to mention '5 consecutive'", a.Description)
	}
}

// TestDetector_ShortRunNoEmit confirms that a run of exactly
// minRunLength-1 does NOT emit an anomaly. This is the threshold
// boundary test — one below the cutoff is silent.
func TestDetector_ShortRunNoEmit(t *testing.T) {
	// 4 identical lines (minRunLength is 5, so 4 is short) followed by
	// a differing line that ends the run.
	lines := []string{"x", "x", "x", "x", "y"}

	got := feed(t, lines)
	if len(got) != 0 {
		t.Errorf("short run: got %d anomalies, want 0; anomalies=%+v", len(got), got)
	}
}

// TestDetector_MultipleDisjointRuns verifies each qualifying run
// produces its own anomaly and the spans don't bleed into each other.
func TestDetector_MultipleDisjointRuns(t *testing.T) {
	// Run A: 6 "aa" lines (lines 1..6, qualifying).
	// Separator: 1 "break" line (line 7).
	// Run B: 5 "bb" lines (lines 8..12, qualifying exactly at threshold).
	lines := []string{
		"aa", "aa", "aa", "aa", "aa", "aa",
		"break",
		"bb", "bb", "bb", "bb", "bb",
	}

	got := feed(t, lines)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; anomalies=%+v", len(got), got)
	}

	a, b := got[0], got[1]
	if a.StartLine != 1 || a.EndLine != 6 {
		t.Errorf("run A span: start=%d end=%d, want 1..6", a.StartLine, a.EndLine)
	}
	if b.StartLine != 8 || b.EndLine != 12 {
		t.Errorf("run B span: start=%d end=%d, want 8..12", b.StartLine, b.EndLine)
	}
}

// TestDetector_RunEndingAtEOF verifies Finalize flushes an open run
// that never had a breaker line after it. Without Finalize this run
// would silently disappear.
func TestDetector_RunEndingAtEOF(t *testing.T) {
	// 5 identical lines and nothing after them.
	lines := []string{"eof", "eof", "eof", "eof", "eof"}

	got := feed(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1", len(got))
	}
	if got[0].EndLine != 5 {
		t.Errorf("EOF run EndLine = %d, want 5", got[0].EndLine)
	}
}

// TestDetector_MixedNoQualifyingRuns covers the "lots of churn but no
// run reaches the threshold" case — classic log file with unique
// messages. Should emit nothing.
func TestDetector_MixedNoQualifyingRuns(t *testing.T) {
	lines := []string{
		"a", "b", "c", // all unique
		"d", "d", // run of 2
		"e",
		"f", "f", "f", "f", // run of 4 (one short)
	}

	got := feed(t, lines)
	if len(got) != 0 {
		t.Errorf("mixed input: got %d anomalies, want 0; %+v", len(got), got)
	}
}

// TestDetector_SingleLineFile is the smallest positive input: a file
// with one line. The run is length 1, which can't qualify.
func TestDetector_SingleLineFile(t *testing.T) {
	got := feed(t, []string{"only"})
	if len(got) != 0 {
		t.Errorf("single line: got %d anomalies, want 0", len(got))
	}
}

// TestDetector_LongRun verifies the length counter isn't capped early
// and the description includes the full run length.
func TestDetector_LongRun(t *testing.T) {
	const n = 100
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "spam"
	}

	got := feed(t, lines)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1", len(got))
	}
	if got[0].EndLine != n {
		t.Errorf("long run EndLine = %d, want %d", got[0].EndLine, n)
	}
	if !strings.Contains(got[0].Description, "100 consecutive") {
		t.Errorf("description = %q, want mention of '100 consecutive'", got[0].Description)
	}
}

// TestDetector_EndToEnd_ViaIndexBuild confirms the detector plugs into
// the real index.Build pipeline and its anomalies surface in the
// UnifiedFileIndex.Anomalies list with Detector == detectorName.
//
// This is the test mandated by the plan's Task 7: "end-to-end test via
// index.Build(opts{Analyze: true}) asserting the detector appears in
// UnifiedFileIndex.Anomalies".
func TestDetector_EndToEnd_ViaIndexBuild(t *testing.T) {
	// Six identical lines surrounded by singleton lines so the run is
	// unambiguous.
	content := "intro\n" +
		"repeat\nrepeat\nrepeat\nrepeat\nrepeat\nrepeat\n" +
		"outro\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "repeat.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Pass an explicit Detector instance (rather than relying on the
	// init()-registered one) so the test is independent of global
	// registry state. The coordinator will still attach detectorName to
	// each anomaly via its Finalize.
	idx, err := index.Build(path, index.BuildOptions{
		Analyze:   true,
		Detectors: []analyzer.LineDetector{New()},
	})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if idx.Anomalies == nil {
		t.Fatal("idx.Anomalies is nil; expected populated slice under Analyze=true")
	}

	anomalies := *idx.Anomalies
	if len(anomalies) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(anomalies), anomalies)
	}
	a := anomalies[0]
	if a.Detector != detectorName {
		t.Errorf("Detector = %q, want %q", a.Detector, detectorName)
	}
	// The coordinator rewrites Category to the detector name — see
	// analyzer.Coordinator.Finalize for rationale. That's the contract
	// Deduplicate relies on.
	if a.Category != detectorName {
		t.Errorf("Category = %q, want %q (coordinator overwrites to Name())", a.Category, detectorName)
	}
	if a.StartLine != 2 || a.EndLine != 7 {
		t.Errorf("span: start=%d end=%d, want 2..7", a.StartLine, a.EndLine)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}

// Make sure the detector metadata is what the plan specifies. If
// someone tweaks a constant here, this test fails loudly so the change
// is deliberate.
func TestDetector_Metadata(t *testing.T) {
	d := New()
	cases := []struct {
		got, want string
		field     string
	}{
		{d.Name(), "repeat-identical", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "repetition", "Category"},
		{d.Description(), "Consecutive identical lines", "Description"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
}
