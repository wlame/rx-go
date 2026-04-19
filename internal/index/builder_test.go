package index

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Helper: write `content` to a temp file and return its path. Used by
// most tests to avoid boilerplate around tmp dir setup.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fixture.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestBuild_TinyFile verifies the basic case: 10 short lines produce a
// single initial checkpoint and no additional ones (file is < step).
func TestBuild_TinyFile(t *testing.T) {
	var buf bytes.Buffer
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&buf, "line %d\n", i)
	}
	p := writeTempFile(t, buf.String())

	idx, err := Build(p, BuildOptions{StepBytes: 1024 * 1024}) // 1 MB step — never crossed
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(idx.LineIndex) != 1 {
		t.Errorf("expected 1 checkpoint (initial), got %d", len(idx.LineIndex))
	}
	if idx.LineIndex[0].LineNumber != 1 || idx.LineIndex[0].ByteOffset != 0 {
		t.Errorf("initial checkpoint = %+v, want {1, 0}", idx.LineIndex[0])
	}
	if idx.LineCount == nil || *idx.LineCount != 10 {
		t.Errorf("LineCount: got %v, want 10", idx.LineCount)
	}
	if idx.SourceSizeBytes != int64(buf.Len()) {
		t.Errorf("SourceSizeBytes: got %d, want %d", idx.SourceSizeBytes, buf.Len())
	}
	if idx.FileType != rxtypes.FileTypeText {
		t.Errorf("FileType: got %v, want text", idx.FileType)
	}
}

// TestBuild_MultipleCheckpoints forces multiple checkpoints by using a
// small step. Every line is 100 bytes; with step=500 we should see a
// new checkpoint roughly every 5 lines.
func TestBuild_MultipleCheckpoints(t *testing.T) {
	var buf bytes.Buffer
	line := strings.Repeat("x", 99) + "\n" // 100 bytes total incl. \n
	for i := 0; i < 50; i++ {
		buf.WriteString(line)
	}
	p := writeTempFile(t, buf.String())

	idx, err := Build(p, BuildOptions{StepBytes: 500})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Expect: [1, 0] then every 5 lines: (6, 500), (11, 1000), ... up to line 51
	// Actually: after 5 lines (500 bytes), checkpoint (6, 500); after 10 lines, (11, 1000).
	if len(idx.LineIndex) < 10 {
		t.Errorf("expected >= 10 checkpoints, got %d", len(idx.LineIndex))
	}
	// Sanity: each entry's line is strictly increasing, offset is strictly increasing.
	for i := 1; i < len(idx.LineIndex); i++ {
		if idx.LineIndex[i].LineNumber <= idx.LineIndex[i-1].LineNumber {
			t.Errorf("checkpoint %d: line %d <= prev %d", i,
				idx.LineIndex[i].LineNumber, idx.LineIndex[i-1].LineNumber)
		}
		if idx.LineIndex[i].ByteOffset <= idx.LineIndex[i-1].ByteOffset {
			t.Errorf("checkpoint %d: offset %d <= prev %d", i,
				idx.LineIndex[i].ByteOffset, idx.LineIndex[i-1].ByteOffset)
		}
	}
	// First real checkpoint: line 6 at offset 500.
	if idx.LineIndex[1].LineNumber != 6 || idx.LineIndex[1].ByteOffset != 500 {
		t.Errorf("second checkpoint = %+v, want {6, 500}", idx.LineIndex[1])
	}
}

// TestBuild_RoundTrip builds an index, saves it, reloads it, and asserts
// the loaded struct is byte-for-byte equal to the built one.
func TestBuild_RoundTrip(t *testing.T) {
	content := strings.Repeat("hello world\n", 1000)
	p := writeTempFile(t, content)

	// Isolate the cache dir so this test doesn't pollute the user's.
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	built, err := Build(p, BuildOptions{StepBytes: 2000, Analyze: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cachePath, err := Save(built)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadFromPath(cachePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The on-disk form round-trips exactly except for the created_at
	// timestamp (we just verify it's non-empty on the loaded side).
	if loaded.LineCount == nil || built.LineCount == nil ||
		*loaded.LineCount != *built.LineCount {
		t.Errorf("LineCount: got %v, want %v", loaded.LineCount, built.LineCount)
	}
	if len(loaded.LineIndex) != len(built.LineIndex) {
		t.Fatalf("LineIndex length: got %d, want %d",
			len(loaded.LineIndex), len(built.LineIndex))
	}
	for i := range built.LineIndex {
		if loaded.LineIndex[i] != built.LineIndex[i] {
			t.Errorf("LineIndex[%d]: got %+v, want %+v",
				i, loaded.LineIndex[i], built.LineIndex[i])
		}
	}
	if loaded.LineLengthMax == nil || *loaded.LineLengthMax != 11 {
		t.Errorf("LineLengthMax: got %v, want 11", loaded.LineLengthMax)
	}
}

// TestBuild_AnalyzeStatistics verifies the line-length aggregates. With
// a mix of line lengths we can sanity-check avg/median/p95 derived
// from a deterministic input.
func TestBuild_AnalyzeStatistics(t *testing.T) {
	// 100 lines: lengths 1..100 bytes of content (plus \n).
	var buf bytes.Buffer
	for i := 1; i <= 100; i++ {
		buf.WriteString(strings.Repeat("a", i))
		buf.WriteByte('\n')
	}
	p := writeTempFile(t, buf.String())

	idx, err := Build(p, BuildOptions{Analyze: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if idx.LineLengthMax == nil || *idx.LineLengthMax != 100 {
		t.Errorf("LineLengthMax: got %v, want 100", idx.LineLengthMax)
	}
	// Average of 1..100 = 50.5
	if idx.LineLengthAvg == nil ||
		*idx.LineLengthAvg < 50.4 || *idx.LineLengthAvg > 50.6 {
		t.Errorf("LineLengthAvg: got %v, want ~50.5", idx.LineLengthAvg)
	}
	// Median of 1..100 = (50+51)/2 = 50.5
	if idx.LineLengthMedian == nil ||
		*idx.LineLengthMedian < 50.4 || *idx.LineLengthMedian > 50.6 {
		t.Errorf("LineLengthMedian: got %v, want ~50.5", idx.LineLengthMedian)
	}
	// P95 of 1..100 via linear interpolation: k=(100-1)*95/100=94.05
	// floor=94, so result = 95 + 0.05*(96-95) = 95.05
	if idx.LineLengthP95 == nil ||
		*idx.LineLengthP95 < 95.0 || *idx.LineLengthP95 > 95.1 {
		t.Errorf("LineLengthP95: got %v, want ~95.05", idx.LineLengthP95)
	}
	// The longest line is line 100 at some positive offset.
	if idx.LineLengthMaxLineNumber == nil || *idx.LineLengthMaxLineNumber != 100 {
		t.Errorf("LineLengthMaxLineNumber: got %v, want 100", idx.LineLengthMaxLineNumber)
	}
}

// TestBuild_LineLengthStats_ComputedWithoutAnalyze covers Stage 9
// Round 2 R1-B10: Python always populates line_length stats (max/avg/
// median/p95/p99/stddev/max_line_number/max_byte_offset) regardless of
// the --analyze flag. The --analyze flag only gates anomaly detection
// and prefix-pattern work, not the basic line-length scan.
//
// Previous Go behavior: stats were gated on Analyze=true, producing
// null fields in POST /v1/index responses for non-analyze runs. That
// broke rx-viewer dashboards showing line-length histograms.
func TestBuild_LineLengthStats_ComputedWithoutAnalyze(t *testing.T) {
	// Known deterministic mix of line lengths (1..20 bytes).
	var buf bytes.Buffer
	for i := 1; i <= 20; i++ {
		buf.WriteString(strings.Repeat("a", i))
		buf.WriteByte('\n')
	}
	p := writeTempFile(t, buf.String())

	// Build WITHOUT --analyze. Python parity: stats must still be present.
	idx, err := Build(p, BuildOptions{Analyze: false})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.LineLengthMax == nil || *idx.LineLengthMax != 20 {
		t.Errorf("LineLengthMax (no analyze): got %v, want 20", idx.LineLengthMax)
	}
	// Avg of 1..20 = 10.5
	if idx.LineLengthAvg == nil || *idx.LineLengthAvg < 10.4 || *idx.LineLengthAvg > 10.6 {
		t.Errorf("LineLengthAvg (no analyze): got %v, want ~10.5", idx.LineLengthAvg)
	}
	if idx.LineLengthMedian == nil {
		t.Errorf("LineLengthMedian (no analyze) should be populated, got nil")
	}
	if idx.LineLengthP95 == nil {
		t.Errorf("LineLengthP95 (no analyze) should be populated, got nil")
	}
	if idx.LineLengthStddev == nil {
		t.Errorf("LineLengthStddev (no analyze) should be populated, got nil")
	}
	if idx.LineLengthMaxLineNumber == nil || *idx.LineLengthMaxLineNumber != 20 {
		t.Errorf("LineLengthMaxLineNumber (no analyze): got %v, want 20",
			idx.LineLengthMaxLineNumber)
	}
	if idx.LineLengthMaxByteOffset == nil {
		t.Errorf("LineLengthMaxByteOffset (no analyze) should be populated, got nil")
	}
}

// TestBuild_EmptyFile handles the edge case where the source file has
// zero bytes. Python's builder produces line_count=0, empty_line_count=0,
// line_length_avg=0, etc. We match.
func TestBuild_EmptyFile(t *testing.T) {
	p := writeTempFile(t, "")
	idx, err := Build(p, BuildOptions{Analyze: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.LineCount == nil || *idx.LineCount != 0 {
		t.Errorf("LineCount: got %v, want 0", idx.LineCount)
	}
	if len(idx.LineIndex) != 1 {
		t.Errorf("LineIndex: expected just the initial [1,0], got %d entries", len(idx.LineIndex))
	}
	if idx.LineLengthAvg == nil || *idx.LineLengthAvg != 0 {
		t.Errorf("LineLengthAvg: got %v, want 0", idx.LineLengthAvg)
	}
}

// TestBuild_EmptyLineCount counts blank lines separately from content lines.
func TestBuild_EmptyLineCount(t *testing.T) {
	content := "hello\n\nworld\n\n\n"
	p := writeTempFile(t, content)
	idx, err := Build(p, BuildOptions{Analyze: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.LineCount == nil || *idx.LineCount != 5 {
		t.Errorf("LineCount: got %v, want 5", idx.LineCount)
	}
	if idx.EmptyLineCount == nil || *idx.EmptyLineCount != 3 {
		t.Errorf("EmptyLineCount: got %v, want 3", idx.EmptyLineCount)
	}
}

// TestBuild_LineEndingSample_OvershootsAt64K_PythonParity covers Stage 8
// Reviewer 1 High #4: Python appends whole lines to the line-ending
// sample until `len(sample) >= 64 KB`, potentially overshooting by up
// to one line. Go previously truncated the last line at byte-granularity
// to fit exactly into the 64 KB budget, which could drop trailing \r\n
// bytes that Python would have seen.
//
// Concretely: build a file whose LF-only content fills the sample to
// ~65530 bytes, then a single CRLF line of length >6. Python's sample
// overshoots to include the full CRLF line (~65539 bytes), so it sees
// the \r\n and classifies as "mixed". Go's pre-fix behavior: take=6,
// appends only "abcdef" without the \r\n, so sample is pure LF → "LF".
//
// The fix drops the truncation; the Go sample must overshoot like
// Python and reach the "mixed" classification.
func TestBuild_LineEndingSample_OvershootsAt64K_PythonParity(t *testing.T) {
	var b bytes.Buffer
	// Fill with "x\n" (2 bytes each) until we've accumulated 65530 bytes
	// (still below 65536 so the Python/Go loop goes one more iteration).
	// 65530 / 2 = 32765 lines.
	for i := 0; i < 32765; i++ {
		b.WriteString("x\n")
	}
	// Now a 9-byte CRLF line. remaining = 65536 - 65530 = 6.
	// Python: appends all 9 bytes, sample becomes 65539 bytes.
	// Go pre-fix: take=6, appends "abcdef" only, sample becomes 65536
	// bytes WITHOUT the \r\n terminator.
	b.WriteString("abcdefg\r\n")
	// More content after — Python's append loop stops once sample >= 64K,
	// and so does Go's via sampleComplete flag.
	for i := 0; i < 10; i++ {
		b.WriteString("z\n")
	}
	p := writeTempFile(t, b.String())

	idx, err := Build(p, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.LineEnding == nil {
		t.Fatal("LineEnding = nil")
	}
	// Python's behavior: sample contains LF + CRLF → "mixed".
	// Go's pre-fix behavior: sample truncated before \r\n → pure LF.
	if *idx.LineEnding != "mixed" {
		t.Errorf("LineEnding: got %q, want %q (Python samples whole lines; pre-fix Go truncated before CRLF terminator)",
			*idx.LineEnding, "mixed")
	}
}

// TestBuild_LineEndingDetection asserts CR/LF/CRLF/mixed classification.
func TestBuild_LineEndingDetection(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"lf_only", "a\nb\nc\n", "LF"},
		{"crlf_only", "a\r\nb\r\nc\r\n", "CRLF"},
		{"mixed", "a\nb\r\nc\n", "mixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeTempFile(t, tc.content)
			idx, err := Build(p, BuildOptions{})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if idx.LineEnding == nil {
				t.Fatal("LineEnding = nil")
			}
			if *idx.LineEnding != tc.want {
				t.Errorf("LineEnding: got %q, want %q", *idx.LineEnding, tc.want)
			}
		})
	}
}

// TestBuild_ReturnsDirError verifies we don't crash on directory input.
func TestBuild_ReturnsDirError(t *testing.T) {
	dir := t.TempDir()
	_, err := Build(dir, BuildOptions{})
	if err == nil {
		t.Error("expected error for directory input")
	}
}

// TestBuild_ReturnsStatError verifies a missing path yields an error.
func TestBuild_ReturnsStatError(t *testing.T) {
	_, err := Build("/definitely/does/not/exist/"+fmt.Sprintf("%d", os.Getpid()),
		BuildOptions{})
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// TestBuild_LongLineDoesNotPanic verifies we don't hit bufio's line limit
// on a single 10 MB line.
func TestBuild_LongLineDoesNotPanic(t *testing.T) {
	big := strings.Repeat("z", 10*1024*1024) + "\n"
	p := writeTempFile(t, big)
	idx, err := Build(p, BuildOptions{Analyze: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.LineCount == nil || *idx.LineCount != 1 {
		t.Errorf("LineCount: got %v, want 1", idx.LineCount)
	}
	if idx.LineLengthMax == nil || *idx.LineLengthMax != 10*1024*1024 {
		t.Errorf("LineLengthMax: got %v, want %d", idx.LineLengthMax, 10*1024*1024)
	}
}

// TestBuild_ValidForSource verifies the stamp can be validated.
func TestBuild_ValidForSource(t *testing.T) {
	content := "hello\n"
	p := writeTempFile(t, content)
	idx, err := Build(p, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !IsValidForSource(idx, p) {
		t.Error("freshly-built index should validate for its source")
	}
	// Modify the file → stamp should now fail.
	if err := os.WriteFile(p, []byte("longer content\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if IsValidForSource(idx, p) {
		t.Error("after mutation, IsValidForSource should be false")
	}
}

// TestGetIndexStepBytes verifies the default step-size is threshold/50.
func TestGetIndexStepBytes(t *testing.T) {
	t.Setenv("RX_LARGE_FILE_MB", "50")
	got := GetIndexStepBytes()
	want := int64(50) * 1024 * 1024 / 50 // 1 MB
	if got != want {
		t.Errorf("default step: got %d, want %d", got, want)
	}

	t.Setenv("RX_LARGE_FILE_MB", "100")
	got = GetIndexStepBytes()
	want = int64(100) * 1024 * 1024 / 50 // 2 MB
	if got != want {
		t.Errorf("step with 100 MB threshold: got %d, want %d", got, want)
	}
}

// TestDetectLineEnding_EmptySample verifies the default for empty input.
func TestDetectLineEnding_EmptySample(t *testing.T) {
	if got := detectLineEnding([]byte{}); got != "LF" {
		t.Errorf("empty sample → %q, want LF", got)
	}
}

// TestBuild_HugeAnalyze exercises the percentile branch on a realistic
// but not enormous sample (1000 lines of varying length).
func TestBuild_HugeAnalyze(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 1000; i++ {
		n := 10 + (i % 90) // 10..99
		buf.WriteString(strings.Repeat("q", n))
		buf.WriteByte('\n')
	}
	p := writeTempFile(t, buf.String())
	idx, err := Build(p, BuildOptions{Analyze: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.LineLengthStddev == nil || *idx.LineLengthStddev <= 0 {
		t.Errorf("LineLengthStddev: got %v, want > 0", idx.LineLengthStddev)
	}
	if idx.LineLengthP95 == nil || *idx.LineLengthP95 < 90 {
		t.Errorf("LineLengthP95: got %v, want >= 90", idx.LineLengthP95)
	}
}

// TestReservoirPercentile_EdgeCases covers the small-n branches of the
// percentile helper. Replaces the old TestPercentile_EdgeCases which
// tested a slice-based helper that was deleted when the builder switched
// to online Welford + reservoir sampling.
func TestReservoirPercentile_EdgeCases(t *testing.T) {
	if got := reservoirPercentile(nil, 95); got != 0 {
		t.Errorf("nil slice p95: got %v, want 0", got)
	}
	if got := reservoirPercentile([]int{42}, 95); got != 42 {
		t.Errorf("single-element p95: got %v, want 42", got)
	}
	// Caller must pre-sort the slice — reservoirPercentile assumes sorted
	// input (the accumulator sorts its reservoir copy before calling).
	if got := reservoirPercentile([]int{1, 2, 3, 4, 5}, 50); got < 2.9 || got > 3.1 {
		t.Errorf("p50 of 1..5: got %v, want ~3", got)
	}
	if got := reservoirPercentile([]int{1, 2, 3, 4, 5}, 0); got != 1 {
		t.Errorf("p0: got %v, want 1", got)
	}
	if got := reservoirPercentile([]int{1, 2, 3, 4, 5}, 100); got != 5 {
		t.Errorf("p100: got %v, want 5", got)
	}
}

// TestHasNonWhitespace_WhitespaceSet ensures our whitespace detection
// matches Python's default strip() characters.
func TestHasNonWhitespace_WhitespaceSet(t *testing.T) {
	for _, ws := range [][]byte{{' '}, {'\t'}, {'\v'}, {'\f'}, {'\r'}, {'\n'}} {
		if hasNonWhitespace(ws) {
			t.Errorf("%q should be whitespace-only", ws)
		}
	}
	if hasNonWhitespace([]byte(" \t \v \f \r \n ")) {
		t.Error("mixed whitespace should return false")
	}
	if !hasNonWhitespace([]byte(" a ")) {
		t.Error("content with letter should return true")
	}
}
