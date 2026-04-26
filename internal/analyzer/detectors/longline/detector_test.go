package longline

// Unit and integration tests for the long-line detector.
//
// Two layers:
//
//  1. Unit: drive a fresh Detector through a real Coordinator (the only
//     path that exposes Window.push into the buffer). Cover the
//     threshold dominance branches (1024 floor, P99+512, 4*median),
//     the "high P99 but no qualifiers" case, short-only files, and a
//     single-extra-long-line fixture.
//
//  2. End-to-end: drive the detector through index.Build with
//     Analyze=true so the full coordinator + flush-context path is
//     exercised. Asserts the anomaly surfaces on UnifiedFileIndex.
//
// Reasoning helpers like computeThreshold have a dedicated table test so
// the three-branch max() math is documented in one place.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/index"
)

// feed drives a fresh Detector through a real Coordinator, then calls
// d.Finalize(flush) directly so the test can see the detector's
// semantic Category unchanged (the coordinator would rewrite it to
// Name()).
//
// lines is the ordered list of line contents (no trailing newline).
// Each line is laid out as `content + '\n'` at consecutive offsets, so
// StartOffset/EndOffset math is easy to predict in assertions.
func feed(t *testing.T, lines []string, flush *analyzer.FlushContext) []analyzer.Anomaly {
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
	return d.Finalize(flush)
}

// makeLine returns a string of exactly n ASCII 'a' bytes. Used by
// several tests to construct inputs at specific byte lengths.
func makeLine(n int) string {
	return strings.Repeat("a", n)
}

// TestDetector_NoLinesExceedThreshold covers the common case: every
// line is well below any plausible threshold. No anomalies should be
// emitted.
//
// With flush == nil the detector uses the static floor (1024 bytes).
// We deliberately pick 200-byte lines so they wouldn't even be
// buffered, let alone emitted.
func TestDetector_NoLinesExceedThreshold(t *testing.T) {
	lines := []string{
		makeLine(200),
		makeLine(100),
		makeLine(500),
	}

	got := feed(t, lines, nil)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0; %+v", len(got), got)
	}
}

// TestDetector_SingleExtraLongLine exercises the headline scenario: a
// file where most lines are short and one is a big outlier. The
// single long line should produce exactly one anomaly with correct
// spans.
//
// With flush == nil, the static-floor branch (1024) dominates. The
// 4000-byte line is well above that threshold.
func TestDetector_SingleExtraLongLine(t *testing.T) {
	// Lines of lengths: 10, 10, 4000, 10.
	lines := []string{
		makeLine(10),
		makeLine(10),
		makeLine(4000),
		makeLine(10),
	}

	got := feed(t, lines, nil)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1; %+v", len(got), got)
	}
	a := got[0]
	if a.StartLine != 3 || a.EndLine != 3 {
		t.Errorf("line span: start=%d end=%d, want 3..3", a.StartLine, a.EndLine)
	}

	// Offsets: lines 1..2 are 10 bytes each + '\n' = 11 bytes, so line 3
	// starts at offset 22. Line 3 is 4000 bytes + '\n' = 4001 bytes,
	// ending at 22 + 4001 = 4023.
	const wantStart int64 = 22
	const wantEnd int64 = 22 + 4001
	if a.StartOffset != wantStart || a.EndOffset != wantEnd {
		t.Errorf("offsets: start=%d end=%d, want %d..%d",
			a.StartOffset, a.EndOffset, wantStart, wantEnd)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if a.Category != detectorCategory {
		t.Errorf("category = %q, want %q", a.Category, detectorCategory)
	}
	if !strings.Contains(a.Description, "4000 bytes") {
		t.Errorf("description = %q, want it to mention '4000 bytes'", a.Description)
	}
}

// TestDetector_ThresholdDominatedByStaticFloor verifies that when both
// P99 and median are tiny, the 1024 floor is what decides whether a
// line qualifies. A 2048-byte line is above 1024; a 1000-byte line is
// below it.
//
// FlushContext here has P99=300, median=100 — both branches
// (P99+512=812, 4*median=400) are BELOW 1024, so the floor wins.
func TestDetector_ThresholdDominatedByStaticFloor(t *testing.T) {
	lines := []string{
		makeLine(2048), // qualifies
		makeLine(1000), // below staticFloor, not even buffered
		makeLine(1500), // above staticFloor and above threshold, qualifies
	}

	flush := &analyzer.FlushContext{
		TotalLines:       3,
		MedianLineLength: 100,
		P99LineLength:    300,
	}

	got := feed(t, lines, flush)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	if got[0].StartLine != 1 {
		t.Errorf("first anomaly StartLine = %d, want 1", got[0].StartLine)
	}
	if got[1].StartLine != 3 {
		t.Errorf("second anomaly StartLine = %d, want 3", got[1].StartLine)
	}
}

// TestDetector_ThresholdDominatedByP99Bonus builds a file where P99+512
// is the largest of the three candidates, then asserts only lines at
// or above that threshold are flagged.
//
// P99=5000 → P99+512 = 5512. median=100 → 4*median = 400. Static
// floor = 1024. max = 5512.
func TestDetector_ThresholdDominatedByP99Bonus(t *testing.T) {
	lines := []string{
		makeLine(6000), // 6000 >= 5512 → qualifies
		makeLine(5511), // 5511 < 5512 → does NOT qualify (but is buffered)
		makeLine(5600), // 5600 >= 5512 → qualifies
	}

	flush := &analyzer.FlushContext{
		TotalLines:       3,
		MedianLineLength: 100,
		P99LineLength:    5000,
	}

	got := feed(t, lines, flush)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	if got[0].StartLine != 1 || got[1].StartLine != 3 {
		t.Errorf("start lines = %d, %d; want 1, 3",
			got[0].StartLine, got[1].StartLine)
	}
}

// TestDetector_ThresholdDominatedByMedianMultiplier builds a file where
// 4*median is the largest candidate, then asserts the threshold is
// applied accordingly.
//
// median=2000 → 4*median = 8000. P99=3000 → P99+512 = 3512. Static
// floor = 1024. max = 8000.
func TestDetector_ThresholdDominatedByMedianMultiplier(t *testing.T) {
	lines := []string{
		makeLine(9000), // 9000 >= 8000 → qualifies
		makeLine(7999), // 7999 < 8000 → does NOT qualify
		makeLine(8000), // 8000 >= 8000 → qualifies (equality is in)
	}

	flush := &analyzer.FlushContext{
		TotalLines:       3,
		MedianLineLength: 2000,
		P99LineLength:    3000,
	}

	got := feed(t, lines, flush)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2; %+v", len(got), got)
	}
	if got[0].StartLine != 1 || got[1].StartLine != 3 {
		t.Errorf("start lines = %d, %d; want 1, 3",
			got[0].StartLine, got[1].StartLine)
	}
}

// TestDetector_HugeP99NoQualifiers covers the "the file is huge-lined
// throughout" case. Even though some lines are thousands of bytes
// long, they're UNDER the P99+512 threshold, so the detector stays
// silent.
//
// P99=10000 → P99+512 = 10512 threshold. Every line under 10512
// bytes should be silent. The 2000-byte lines ARE buffered (above the
// 1024 floor) but rejected in Finalize.
func TestDetector_HugeP99NoQualifiers(t *testing.T) {
	lines := []string{
		makeLine(2000),
		makeLine(3000),
		makeLine(5000),
		makeLine(9000),
	}

	flush := &analyzer.FlushContext{
		TotalLines:       4,
		MedianLineLength: 2000,
		P99LineLength:    10000,
	}

	got := feed(t, lines, flush)
	if len(got) != 0 {
		t.Errorf("got %d anomalies, want 0 (all lines under P99+512); %+v",
			len(got), got)
	}
}

// TestDetector_BelowFloorNeverBuffered is a regression guard: lines
// under staticFloor must be skipped at OnLine time and NEVER appear in
// the internal buffer, regardless of subsequent Finalize threshold.
//
// We look at d.buf directly after feeding, since the point is
// specifically about the OnLine gate, not the Finalize filter.
func TestDetector_BelowFloorNeverBuffered(t *testing.T) {
	d := New()
	coord := analyzer.NewCoordinator(16, []analyzer.LineDetector{d})

	// All lines below staticFloor.
	lines := []string{
		makeLine(100),
		makeLine(500),
		makeLine(1023),
	}

	var offset int64
	for i, line := range lines {
		start := offset
		end := start + int64(len(line)) + 1
		offset = end
		coord.ProcessLine(int64(i+1), start, end, []byte(line))
	}

	if len(d.buf) != 0 {
		t.Errorf("d.buf has %d entries after sub-floor input; want 0",
			len(d.buf))
	}
}

// TestComputeThreshold table-tests the three-branch max() formula plus
// the nil-flush fallback. This is the math inside Finalize extracted
// out so its behavior is pinned down independent of the Coordinator
// path.
func TestComputeThreshold(t *testing.T) {
	cases := []struct {
		name   string
		flush  *analyzer.FlushContext
		expect int64
	}{
		{
			name:   "nil flush falls back to static floor",
			flush:  nil,
			expect: 1024,
		},
		{
			name: "zero stats fall back to static floor",
			flush: &analyzer.FlushContext{
				MedianLineLength: 0,
				P99LineLength:    0,
			},
			expect: 1024,
		},
		{
			name: "static floor dominates small stats",
			flush: &analyzer.FlushContext{
				MedianLineLength: 100,
				P99LineLength:    300,
			},
			expect: 1024, // P99+512=812, 4*median=400
		},
		{
			name: "P99+512 dominates",
			flush: &analyzer.FlushContext{
				MedianLineLength: 100,
				P99LineLength:    5000,
			},
			expect: 5512, // P99+512=5512, 4*median=400, floor=1024
		},
		{
			name: "4*median dominates",
			flush: &analyzer.FlushContext{
				MedianLineLength: 2000,
				P99LineLength:    3000,
			},
			expect: 8000, // 4*median=8000, P99+512=3512, floor=1024
		},
		{
			name: "two candidates tie at the top, max still picks one",
			flush: &analyzer.FlushContext{
				MedianLineLength: 2000, // 4*2000=8000
				P99LineLength:    7488, // 7488+512=8000
			},
			expect: 8000,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeThreshold(c.flush)
			if got != c.expect {
				t.Errorf("computeThreshold = %d, want %d", got, c.expect)
			}
		})
	}
}

// TestDetector_Metadata nails the plan-mandated metadata. If someone
// tweaks a constant here, this test fails loudly so the change is
// deliberate.
func TestDetector_Metadata(t *testing.T) {
	d := New()
	cases := []struct {
		got, want string
		field     string
	}{
		{d.Name(), "long-line", "Name"},
		{d.Version(), "0.1.0", "Version"},
		{d.Category(), "format", "Category"},
		{d.Description(), "Lines unusually long relative to the file's length distribution", "Description"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
}

// TestDetector_EndToEnd_ViaIndexBuild confirms the detector plugs into
// the real index.Build pipeline and its anomalies surface in the
// UnifiedFileIndex.Anomalies list with Detector == detectorName, and
// that the coordinator overwrites Category with Name() (the
// Deduplicate contract).
//
// Construct a file where one line is a ~4KB outlier and the rest are
// short. The real index pipeline computes P99 and median — we don't
// pin those values here (they're computed from the real file shape)
// but we do assert the big line ends up flagged.
func TestDetector_EndToEnd_ViaIndexBuild(t *testing.T) {
	shortLine := makeLine(10)
	longLine := makeLine(5000)

	// 50 short lines, one long line in the middle, 50 more short lines.
	// The short lines dominate the distribution so P99 stays small.
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintln(&sb, shortLine)
	}
	longLineNumber := 51
	fmt.Fprintln(&sb, longLine)
	for i := 0; i < 50; i++ {
		fmt.Fprintln(&sb, shortLine)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "longline.log")
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

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
	if a.Category != detectorName {
		t.Errorf("Category = %q, want %q (coordinator overwrites to Name())",
			a.Category, detectorName)
	}
	if a.StartLine != int64(longLineNumber) || a.EndLine != int64(longLineNumber) {
		t.Errorf("span: start=%d end=%d, want %d..%d",
			a.StartLine, a.EndLine, longLineNumber, longLineNumber)
	}
	if a.Severity != severity {
		t.Errorf("severity = %v, want %v", a.Severity, severity)
	}
	if idx.AnomalySummary[detectorName] != 1 {
		t.Errorf("AnomalySummary[%q] = %d, want 1", detectorName,
			idx.AnomalySummary[detectorName])
	}
}
