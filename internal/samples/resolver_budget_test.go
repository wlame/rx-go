// — byte-budget tests for the samples resolver.
//
// These tests exist to pin down the BOUNDED-READ CONTRACT: every
// resolver code path must read no more of the source file than the
// result requires (plus small overshoot for I/O buffering).
//
// They complement the correctness tests in resolver_test.go. The bug
// they exist for — an unreachable loop break on a range request —
// passed every correctness test cleanly: the output was right, it just
// read the ENTIRE file to produce a 1000-line slice. That kind of
// regression is only caught by asserting on byte traffic.
//
// The tests use internal/testutil/counting to intercept the file
// opens via the openFileForSamples seam. Production code is unchanged;
// the seam defaults to os.Open.

package samples

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// NOTE: we roll a small countingOSFile below rather than reusing
// internal/testutil/counting.File. The resolver opens files through the
// readSeekCloser interface and calls sequential Read via bufio; the
// shared helper's File wraps io.ReaderAt which this path doesn't use.
// Keeping the wrapper local keeps the test explicit about WHAT bytes
// are being counted (sequential Read bytes, not positional ReadAt).

// withCountingOpen wires the package-level openFileForSamples seam to
// a counting wrapper for the duration of the test. Returns a pointer to
// the running byte counter; the caller asserts on counter.Load() after
// exercising the code under test. The original seam is restored in
// t.Cleanup so tests in the same package don't leak state.
//
// We keep ALL opens going through a single shared counter rather than
// per-file counters. Every test here opens exactly ONE file so this is
// unambiguous.
func withCountingOpen(t *testing.T) *atomic.Int64 {
	t.Helper()
	counter := new(atomic.Int64)
	orig := openFileForSamples
	openFileForSamples = func(src paths.Pinned) (readSeekCloser, error) {
		raw, err := src.Open()
		if err != nil {
			return nil, err
		}
		return &countingOSFile{File: raw, counter: counter}, nil
	}
	t.Cleanup(func() { openFileForSamples = orig })
	return counter
}

// countingOSFile is a thin wrapper matching readSeekCloser. We can't
// reuse counting.File because it wraps io.ReaderAt; the resolver uses
// sequential Read via bufio.NewReader.
type countingOSFile struct {
	*os.File
	counter *atomic.Int64
}

// Read increments the counter by the number of bytes actually returned.
func (f *countingOSFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		f.counter.Add(int64(n))
	}
	return n, err
}

// ReadAt counts reads by position too: the timestamp format is detected
// from the head of the text read that way, and the read back from the
// end for a file's last timestamp is one too.
func (f *countingOSFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	if n > 0 {
		f.counter.Add(int64(n))
	}
	return n, err
}

// makeLargeFixture writes a text file with a fixed number of ~150-byte
// lines. Mimics the user's real-world fixture (posts_1m.txt: 1.3 GB
// = 10M lines, avg ~130 bytes/line) on a smaller scale. Returns path
// and total bytes.
func makeLargeFixture(t *testing.T, numLines int) (path string, totalBytes int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "fixture.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer func() {
		_ = f.Close()
		st, _ := os.Stat(path)
		totalBytes = st.Size()
	}()
	// Each line is deterministic so assertions can pin content.
	// Pattern: "L<N>: <padding to 150 bytes>\n"
	for i := 1; i <= numLines; i++ {
		prefix := fmt.Sprintf("L%d: ", i)
		padding := make([]byte, 150-len(prefix)-1) // -1 for newline
		for j := range padding {
			padding[j] = 'X'
		}
		if _, err := f.WriteString(prefix + string(padding) + "\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// We need totalBytes AFTER close+stat, so re-stat here inline.
	_ = f.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	return path, st.Size()
}

// TestBudget_LinesRangeRequest_StopsAtEndLine is the headline
// regression guard. A range request for lines 100..200 on a 10,000-line
// fixture must read ONLY the bytes needed for those lines plus small
// buffering overshoot — NOT the rest of the file.
//
// Before the loop break was reachable, it read the whole file: ~1.5 MB
// for this fixture, ~1.3 GB for the log that surfaced it. The loop now
// breaks once currentLine > endLine.
func TestBudget_LinesRangeRequest_StopsAtEndLine(t *testing.T) {
	// NOT t.Parallel — these tests share the package-level
	// openFileForSamples seam. Making them parallel would race on the
	// reassignment in withCountingOpen.
	const lineCount = 10_000
	path, totalBytes := makeLargeFixture(t, lineCount)
	counter := withCountingOpen(t)

	// Request lines 100-200 from a 10k-line file.
	const startLine = int64(100)
	const endLine = int64(200)
	lines, err := readLineRangeWithIndex(pinForTest(t, path), startLine, endLine, nil)
	if err != nil {
		t.Fatalf("readLineRangeWithIndex: %v", err)
	}

	// --- Correctness: we got the right 101 lines.
	wantCount := int(endLine - startLine + 1)
	if len(lines) != wantCount {
		t.Fatalf("got %d lines, want %d", len(lines), wantCount)
	}
	// Spot-check first and last.
	wantFirstPrefix := fmt.Sprintf("L%d: ", startLine)
	if len(lines[0]) < len(wantFirstPrefix) || lines[0][:len(wantFirstPrefix)] != wantFirstPrefix {
		t.Fatalf("lines[0]=%q, want prefix %q", lines[0], wantFirstPrefix)
	}
	wantLastPrefix := fmt.Sprintf("L%d: ", endLine)
	if lines[wantCount-1][:len(wantLastPrefix)] != wantLastPrefix {
		t.Fatalf("lines[last]=%q, want prefix %q", lines[wantCount-1], wantLastPrefix)
	}

	// --- Budget: read no more than endLine * ~150 bytes + bufio buffer
	// overshoot. Budget is generous to account for bufio's 4 KB default
	// reads — we just need to verify we didn't read the whole file.
	avgLineBytes := int64(150)
	bufferOvershoot := int64(8 * 1024) // 2x bufio default
	budget := endLine*avgLineBytes + bufferOvershoot
	got := counter.Load()

	if got > budget {
		t.Fatalf("read %d bytes, budget %d (file is %d bytes, %.1f%% read)",
			got, budget, totalBytes, 100.0*float64(got)/float64(totalBytes))
	}
	// Sanity: we shouldn't read drastically less than expected either
	// (that would mean we skipped content). Floor = endLine * avgLine / 2.
	floor := (endLine * avgLineBytes) / 2
	if got < floor {
		t.Fatalf("read %d bytes, expected at least %d (are we actually reading line data?)",
			got, floor)
	}
	t.Logf("read %d bytes to produce %d lines from %d-byte file (%.3f%%)",
		got, len(lines), totalBytes, 100.0*float64(got)/float64(totalBytes))
}

// TestBudget_LinesRangeMidFile_SeekSkipsPrefix verifies index-aware
// seek works: with a checkpoint at line 5000, a request for lines
// 5100-5200 must NOT read lines 1..4999. This is the whole point of
// the line index.
func TestBudget_LinesRangeMidFile_SeekSkipsPrefix(t *testing.T) {
	// NOT t.Parallel — these tests share the package-level
	// openFileForSamples seam. Making them parallel would race on the
	// reassignment in withCountingOpen.
	const lineCount = 10_000
	path, _ := makeLargeFixture(t, lineCount)
	counter := withCountingOpen(t)

	// Build a minimal index with a checkpoint every 1000 lines.
	// Each of our fixture lines is exactly 150 bytes, so line N starts
	// at byte (N-1)*150.
	idx := &rxtypes.UnifiedFileIndex{
		LineIndex: []rxtypes.LineIndexEntry{
			{LineNumber: 1, ByteOffset: 0},
			{LineNumber: 1001, ByteOffset: 1000 * 150},
			{LineNumber: 2001, ByteOffset: 2000 * 150},
			{LineNumber: 3001, ByteOffset: 3000 * 150},
			{LineNumber: 4001, ByteOffset: 4000 * 150},
			{LineNumber: 5001, ByteOffset: 5000 * 150},
			{LineNumber: 6001, ByteOffset: 6000 * 150},
			{LineNumber: 7001, ByteOffset: 7000 * 150},
			{LineNumber: 8001, ByteOffset: 8000 * 150},
			{LineNumber: 9001, ByteOffset: 9000 * 150},
		},
	}

	// Request lines 5100-5200. With the index the resolver should seek
	// to checkpoint 5001 and read from there — about 200 lines * 150 =
	// 30 KB, not 5200 * 150 = 780 KB.
	lines, err := readLineRangeWithIndex(pinForTest(t, path), 5100, 5200, idx)
	if err != nil {
		t.Fatalf("readLineRangeWithIndex: %v", err)
	}
	if len(lines) != 101 {
		t.Fatalf("got %d lines, want 101", len(lines))
	}

	// Budget: (5200 - 5001) lines of 150 bytes + bufio overshoot =
	// ~30 KB + 8 KB = 38 KB. We assert WELL UNDER the no-index path.
	budget := int64(50 * 1024)
	got := counter.Load()
	if got > budget {
		t.Fatalf("mid-file seek: read %d bytes, budget %d (index seek not working?)",
			got, budget)
	}
	t.Logf("mid-file range read %d bytes (~%.1f KB) with checkpoint seek",
		got, float64(got)/1024.0)
}

// TestBudget_LinesSingleWithContext_StopsAfterContext asserts that a
// single-line request with small context reads only the window plus
// bufio overshoot. The target-line offset must still be captured.
func TestBudget_LinesSingleWithContext_StopsAfterContext(t *testing.T) {
	// NOT t.Parallel — these tests share the package-level
	// openFileForSamples seam. Making them parallel would race on the
	// reassignment in withCountingOpen.
	const lineCount = 10_000
	path, _ := makeLargeFixture(t, lineCount)
	counter := withCountingOpen(t)

	// Single line 150 with ±5 context → read lines 145..155, capture
	// offset of line 150.
	lines, targetOffset, err := readLinesWithTarget(pinForTest(t, path), 145, 155, 150, nil)
	if err != nil {
		t.Fatalf("readLinesWithTarget: %v", err)
	}
	if len(lines) != 11 {
		t.Fatalf("got %d lines, want 11", len(lines))
	}
	// Line 150 starts at byte 149 * 150 = 22350.
	wantOffset := int64(149 * 150)
	if targetOffset != wantOffset {
		t.Fatalf("targetOffset=%d, want %d", targetOffset, wantOffset)
	}

	// Budget: 155 lines * 150 bytes + bufio overshoot.
	budget := int64(155*150 + 8*1024)
	got := counter.Load()
	if got > budget {
		t.Fatalf("single-with-context: read %d bytes, budget %d",
			got, budget)
	}
	t.Logf("single+context read %d bytes (target at offset %d)",
		got, targetOffset)
}

// TestBudget_ByteOffsetRange_StopsAtEndOffset covers the
// lineNumberForOffset → readLineRange path used in OffsetsMode. A
// byte-offset range converts both ends to line numbers (linear scan)
// then calls readLineRange which delegates to readLinesWithTarget
// with targetLine = startLine.
//
// NOTE: lineNumberForOffset currently performs a linear scan from
// byte 0; byte-offset mode is SLOWER than line mode because of this.
// This test verifies the CURRENT contract (linear scan acceptable for
// byte-offset queries — they're rare) — the pertinent budget here is
// that we don't read PAST the range.
func TestBudget_ByteOffsetRange_StopsAtEndOffset(t *testing.T) {
	// NOT t.Parallel — these tests share the package-level
	// openFileForSamples seam. Making them parallel would race on the
	// reassignment in withCountingOpen.
	const lineCount = 10_000
	path, totalBytes := makeLargeFixture(t, lineCount)
	counter := withCountingOpen(t)

	// Pick a byte offset range that covers roughly lines 100-200.
	// Line 100 starts at offset 99 * 150 = 14850. Line 200 starts at
	// offset 199 * 150 = 29850. Range: 15000..30000.
	startOff := int64(15000)
	endOff := int64(30000)

	startLine, err := lineNumberForOffset(pinForTest(t, path), startOff)
	if err != nil {
		t.Fatalf("lineNumberForOffset(start): %v", err)
	}
	endLine, err := lineNumberForOffset(pinForTest(t, path), endOff)
	if err != nil {
		t.Fatalf("lineNumberForOffset(end): %v", err)
	}
	lines, _, err := readLineRange(pinForTest(t, path), startLine, endLine)
	if err != nil {
		t.Fatalf("readLineRange: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("got 0 lines")
	}

	// Two linear scans + range read. Each linear scan reads up to the
	// requested offset (not past it). Budget:
	//   - lineNumberForOffset(start) reads ~startOff bytes
	//   - lineNumberForOffset(end)   reads ~endOff bytes
	//   - readLineRange reads ~endLine * 150 bytes
	// Total worst case: 3 * endLine * 150 + bufio overshoot per open.
	budget := int64(3*endLine*150 + 3*8*1024)
	got := counter.Load()
	if got > budget {
		t.Fatalf("byte-offset range: read %d bytes, budget %d (file=%d)",
			got, budget, totalBytes)
	}
	t.Logf("byte-offset range read %d bytes for %d lines",
		got, len(lines))
}

// TestBudget_LinesRangeNoIndex_FullScanExpected documents the WORST
// CASE: a range request without an index must still be bounded by
// endLine * avgLineBytes (it scans from the top). This confirms the
// loop break is reachable on the no-index path too: even without an
// index, we STOP at endLine.
func TestBudget_LinesRangeNoIndex_FullScanExpected(t *testing.T) {
	// NOT t.Parallel — these tests share the package-level
	// openFileForSamples seam. Making them parallel would race on the
	// reassignment in withCountingOpen.
	const lineCount = 10_000
	path, totalBytes := makeLargeFixture(t, lineCount)
	counter := withCountingOpen(t)

	// Range that extends to line 500 without an index. The resolver
	// MUST scan from the top (no seek), but MUST STILL stop at line 500.
	lines, err := readLineRangeWithIndex(pinForTest(t, path), 1, 500, nil)
	if err != nil {
		t.Fatalf("readLineRangeWithIndex: %v", err)
	}
	if len(lines) != 500 {
		t.Fatalf("got %d lines, want 500", len(lines))
	}

	// Budget: endLine * 150 + bufio overshoot.
	budget := int64(500*150 + 8*1024)
	got := counter.Load()
	if got > budget {
		t.Fatalf("no-index range: read %d bytes, budget %d, file=%d",
			got, budget, totalBytes)
	}
	// And critically: much less than totalBytes.
	if got > totalBytes/5 {
		t.Fatalf("no-index range read %d bytes — more than 20%% of file (%d) — did the break fire?",
			got, totalBytes)
	}
	t.Logf("no-index range (1..500) read %d bytes from %d-byte file",
		got, totalBytes)
}

// TestBudget_FullResolverFlow_LinesRange exercises the TOP-LEVEL
// Resolve entry point, not just the internal helper. This ensures the
// budget guarantee survives the full Request → Resolve → response
// pipeline including the index-loading lazy logic.
func TestBudget_FullResolverFlow_LinesRange(t *testing.T) {
	// NOT t.Parallel — these tests share the package-level
	// openFileForSamples seam. Making them parallel would race on the
	// reassignment in withCountingOpen.
	const lineCount = 10_000
	path, totalBytes := makeLargeFixture(t, lineCount)
	counter := withCountingOpen(t)

	endVal := int64(250)
	req := Request{
		Path: path,
		Lines: []OffsetOrRange{
			{Start: 150, End: &endVal},
		},
		IndexLoader: NoIndex,
	}
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	key := "150-" + strconv.FormatInt(endVal, 10)
	got := resp.Samples[key]
	if len(got) != int(endVal-150+1) {
		t.Fatalf("resp.Samples[%s] has %d lines, want %d",
			key, len(got), endVal-150+1)
	}

	// Budget: endVal * 150 + bufio overshoot, plus the head of the
	// text the timestamp format is detected from (time_format): a
	// mebibyte, or the whole file when it is smaller.
	budget := int64(endVal*150+8*1024) + min(totalBytes, timestamps.SampleBytes)
	bytesRead := counter.Load()
	if bytesRead > budget {
		t.Fatalf("Resolve(range 150-%d): read %d bytes, budget %d, file=%d",
			endVal, bytesRead, budget, totalBytes)
	}
	t.Logf("full Resolve read %d bytes for range 150-%d",
		bytesRead, endVal)
}

// TestBudget_ManyOffsets_OnePassOverTheFile pins the cost of asking
// about several byte offsets at once.
//
// The viewer asks exactly this way: a capped search leaves it holding a
// batch of match offsets whose line numbers it wants. Resolving them
// one at a time read the whole file once per offset, so twenty matches
// in a multi-gigabyte log meant twenty passes.
func TestBudget_ManyOffsets_OnePassOverTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.log")
	var content []byte
	for i := 1; i <= 5000; i++ {
		content = append(content, []byte(fmt.Sprintf("line %d of the log\n", i))...)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Twenty offsets spread over the file, the last one near the end.
	var spec []OffsetOrRange
	var want []int64
	for i := 1; i <= 20; i++ {
		line := i * 250
		off := int64(0)
		for n := 1; n < line; n++ {
			off += int64(len(fmt.Sprintf("line %d of the log\n", n)))
		}
		spec = append(spec, OffsetOrRange{Start: off})
		want = append(want, int64(line))
	}

	counter := withCountingOpen(t)
	resp, err := Resolve(Request{Path: path, Offsets: spec, IndexLoader: NoIndex})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	read := counter.Load()

	for i, v := range spec {
		key := strconv.FormatInt(v.Start, 10)
		if resp.Offsets[key] != want[i] {
			t.Fatalf("offset %d resolved to line %d, want %d", v.Start, resp.Offsets[key], want[i])
		}
	}

	// One pass plus the windows read around each offset. Twenty passes
	// would be twenty times the file.
	size := int64(len(content))
	if read > 3*size {
		t.Fatalf("read %d bytes for a %d byte file: that is %.1f passes, not one",
			read, size, float64(read)/float64(size))
	}
	t.Logf("read %d bytes for a %d byte file resolving %d offsets", read, size, len(spec))
}
