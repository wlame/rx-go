package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildIndex_LineStats_KnownContent is the integration regression guard
// for the Welford + reservoir-sampling refactor. It builds an index on a
// fixture with a known line-length distribution and asserts the populated
// IndexAnalysis fields are correct.
//
// The fixture is 100 identical lines of 50 bytes content + '\n' (total
// 5 100 bytes). Because every line has the same length, mean/median/p95/p99
// all collapse to 50.0 regardless of whether percentiles come from a full
// sort or from a reservoir — so this test is STABLE across the refactor and
// catches any field that gets dropped or corrupted by the integration.
func TestBuildIndex_LineStats_KnownContent(t *testing.T) {
	line := strings.Repeat("a", 50) + "\n"
	content := strings.Repeat(line, 100)

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	idx, err := Build(path, BuildOptions{Analyze: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Line count: 100 lines.
	if idx.LineCount == nil || *idx.LineCount != 100 {
		t.Errorf("LineCount: got %v want 100", idx.LineCount)
	}
	// No empty lines in this fixture.
	if idx.EmptyLineCount == nil || *idx.EmptyLineCount != 0 {
		t.Errorf("EmptyLineCount: got %v want 0", idx.EmptyLineCount)
	}
	// Every line is 50 bytes content.
	if idx.LineLengthMax == nil || *idx.LineLengthMax != 50 {
		t.Errorf("LineLengthMax: got %v want 50", idx.LineLengthMax)
	}
	// Mean/median/p95/p99 of a constant distribution are all 50 exactly.
	if idx.LineLengthAvg == nil || *idx.LineLengthAvg != 50.0 {
		t.Errorf("LineLengthAvg: got %v want 50.0", idx.LineLengthAvg)
	}
	if idx.LineLengthMedian == nil || *idx.LineLengthMedian != 50.0 {
		t.Errorf("LineLengthMedian: got %v want 50.0", idx.LineLengthMedian)
	}
	if idx.LineLengthP95 == nil || *idx.LineLengthP95 != 50.0 {
		t.Errorf("LineLengthP95: got %v want 50.0", idx.LineLengthP95)
	}
	if idx.LineLengthP99 == nil || *idx.LineLengthP99 != 50.0 {
		t.Errorf("LineLengthP99: got %v want 50.0", idx.LineLengthP99)
	}
	// All identical → variance is 0 → stddev is 0.
	if idx.LineLengthStddev == nil || *idx.LineLengthStddev != 0.0 {
		t.Errorf("LineLengthStddev: got %v want 0.0", idx.LineLengthStddev)
	}
	// First line carries the max (all tied, and the current implementation
	// keeps the FIRST observed max — line 1 at offset 0).
	if idx.LineLengthMaxLineNumber == nil || *idx.LineLengthMaxLineNumber != 1 {
		t.Errorf("LineLengthMaxLineNumber: got %v want 1", idx.LineLengthMaxLineNumber)
	}
	if idx.LineLengthMaxByteOffset == nil || *idx.LineLengthMaxByteOffset != 0 {
		t.Errorf("LineLengthMaxByteOffset: got %v want 0", idx.LineLengthMaxByteOffset)
	}
}
