package trace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A NUL byte after the first 8 KiB does not make a file binary: rx has
// already decided it is text, and a NUL is one more byte of a line. These
// tests put NUL bytes into lines of a numbered log and check every answer
// against the text itself: each match sits at the start of its line,
// carries the line's number and its whole text, and every line holding
// the needle is found. ripgrep left to its own binary detection counts a
// NUL as a line break, which numbers the later lines too high and cuts
// the NUL line in two.

// nulContext is the window width the NUL tests ask for, so the lines
// around a NUL line are checked as well.
const nulContext = 2

// putNULs overwrites the bytes at the given positions of a line (1-based
// line number, 0-based positions inside the line) with NUL bytes. No
// byte moves, so the offsets of every line stay where they were.
func putNULs(text []byte, table textLineTable, line int, positions ...int) {
	start := table.starts[line-1]
	for _, at := range positions {
		text[start+int64(at)] = 0
	}
}

// putNULsAround puts NUL bytes into the lines given: a needle line gets
// one before the needle and one after it, any other line three. The
// positions avoid the line's number, so the text still says which line
// it is.
func putNULsAround(text []byte, table textLineTable, lines ...int) {
	for _, n := range lines {
		lineText := table.texts[n-1]
		end := len(lineText) - 2
		if strings.Contains(lineText, "NEEDLE") {
			putNULs(text, table, n, 4, end) // "line\x00<n> NEEDLE …\x00…"
			continue
		}
		putNULs(text, table, n, 4, end-10, end)
	}
}

// nulNeedleText returns a log of about size bytes with a needle on every
// 97th line. Its NUL bytes go in afterwards, with putNULsAround.
func nulNeedleText(size int) []byte {
	return writeNeedleText(size, func(line int) bool { return line%97 == 0 })
}

// checkAnswerAgainstText checks a trace answer against the text it
// searched: every match is a line holding NEEDLE, at that line's first
// byte, numbered as that line (or -1 where a capped scan left it
// unknown), with the line's whole text; its window holds the lines
// around it. When complete is set, every line holding NEEDLE must be in
// the answer.
func checkAnswerAgainstText(t *testing.T, resp *rxtypes.TraceResponse, table textLineTable, complete bool) {
	t.Helper()
	checkWindowsByOffset(t, resp, table, nulContext, nulContext)
	found := map[int64]bool{}
	for _, m := range resp.Matches {
		i := table.byStart[m.Offset]
		found[m.Offset] = true
		if m.LineText == nil || *m.LineText != table.texts[i] {
			got := "<nil>"
			if m.LineText != nil {
				got = *m.LineText
			}
			t.Errorf("match on line %d: line_text %q, want %q", i+1, got, table.texts[i])
		}
	}
	if !complete {
		return
	}
	for i, lineText := range table.texts {
		if strings.Contains(lineText, "NEEDLE") && !found[table.starts[i]] {
			t.Errorf("line %d holds NEEDLE and is not in the answer", i+1)
		}
	}
}

// traceFromCacheWith runs a trace with opts that must be answered from
// the trace cache, and fails the test when the file was scanned instead.
func traceFromCacheWith(t *testing.T, path string, opts Options) *rxtypes.TraceResponse {
	t.Helper()
	scanned := false
	opts.afterScan = func(string) { scanned = true }
	resp := traceOnce(t, path, []string{"NEEDLE"}, opts)
	if scanned {
		t.Fatal("the trace scanned the file instead of reading the cache")
	}
	return resp
}

// nulFixture is a plain log with NUL bytes in some of its lines.
type nulFixture struct {
	path  string
	text  []byte
	table textLineTable
}

// writeNULFixture writes a log of about size bytes and puts NUL bytes
// into the lines pick chooses from the file's chunks. pick runs once
// the chunks are known, so a test can aim at a middle chunk.
func writeNULFixture(t *testing.T, size int, pick func(chunkFirstLines []int, lineCount int) []int) nulFixture {
	t.Helper()
	text := nulNeedleText(size)
	path := writeTextFile(t, "nul.log", text)
	tasks, err := CreateFileTasks(path)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	table := newTextLineTable(text)
	firstLines := make([]int, len(tasks))
	for i, task := range tasks {
		firstLines[i] = table.byStart[task.Offset] + 1
	}
	putNULsAround(text, table, pick(firstLines, len(table.starts))...)
	if bytes.IndexByte(text[:8192], 0) >= 0 {
		t.Fatal("fixture has a NUL in its first 8 KiB, which makes it binary")
	}
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	return nulFixture{path: path, text: text, table: newTextLineTable(text)}
}

// nulLayouts are the two shapes a plain file is scanned in: one chunk,
// and several chunks scanned in parallel with NUL bytes in a middle one
// and in the last one. Both make the file large enough to be cached.
var nulLayouts = []struct {
	name       string
	size       int
	env        func(t *testing.T)
	wantChunks func(n int) bool
	pick       func(chunkFirstLines []int, lineCount int) []int
}{
	{
		name: "one chunk",
		size: 2 << 20,
		env: func(t *testing.T) {
			requireRipgrep(t)
			t.Setenv("RX_CACHE_DIR", t.TempDir())
		},
		wantChunks: func(n int) bool { return n == 1 },
		pick: func(_ []int, lineCount int) []int {
			// Lines 485 (a needle) and 486 lie past the first 8 KiB.
			return []int{485, 486, lineCount / 2, lineCount/2 + 3*97}
		},
	},
	{
		name:       "several chunks",
		size:       8 << 20,
		env:        chunkedTraceEnv,
		wantChunks: func(n int) bool { return n >= 3 },
		pick: func(first []int, lineCount int) []int {
			middle := first[1] + 1000
			last := first[len(first)-1]
			// Needle lines are the multiples of 97.
			needle := func(n int) int { return n - n%97 + 97 }
			return []int{middle, needle(middle), needle(middle) + 1, needle(middle + 5000), last + 40, needle(last + 40)}
		},
	},
}

// Scenarios: a NUL after the first 8 KiB in a file of one chunk, and
// several NULs in one line and in several lines of a middle chunk. The
// cold scan, the cache hit, a scan with a line index and a scan with
// --no-index each answer as the text says, and all four are equal.
func TestNULBytesInAPlainFileKeepEveryLineWhole(t *testing.T) {
	for _, layout := range nulLayouts {
		t.Run(layout.name, func(t *testing.T) {
			layout.env(t)
			t.Setenv("RX_LARGE_FILE_MB", "1")
			fx := writeNULFixture(t, layout.size, layout.pick)
			tasks, err := CreateFileTasks(fx.path)
			if err != nil || !layout.wantChunks(len(tasks)) {
				t.Fatalf("fixture scans in %d chunk(s) (%v); not the layout under test", len(tasks), err)
			}
			opts := Options{ContextBefore: nulContext, ContextAfter: nulContext}

			cold := traceOnce(t, fx.path, []string{"NEEDLE"}, opts)
			checkAnswerAgainstText(t, cold, fx.table, true)

			requireTraceCache(t, fx.path)
			warm := traceFromCacheWith(t, fx.path, opts)
			traceanswer.RequireSame(t, "cache hit", warm, cold)

			uncached := opts
			uncached.NoCache = true
			withoutIndex := uncached
			withoutIndex.NoIndex = true
			noIndex := traceOnce(t, fx.path, []string{"NEEDLE"}, withoutIndex)
			traceanswer.RequireSame(t, "--no-index", noIndex, cold)

			idx, err := index.Build(fx.path, index.BuildOptions{StepBytes: 64 << 10})
			if err != nil {
				t.Fatalf("build index: %v", err)
			}
			if _, err := index.Save(idx); err != nil {
				t.Fatalf("save index: %v", err)
			}
			indexed := traceOnce(t, fx.path, []string{"NEEDLE"}, uncached)
			traceanswer.RequireSame(t, "line index", indexed, cold)
		})
	}
}

// writeCappedNULFixture writes a plain file of several chunks whose
// needles all lie in the last chunk, every tenth line from its sixth,
// with NUL bytes in that chunk ahead of the needles and on one needle
// line. A cap below the needle count keeps the first matches of that
// chunk on every run, so two capped answers hold the same matches, and
// each kept match lies after a NUL line. It returns the offset of the
// first NUL line.
func writeCappedNULFixture(t *testing.T) (fx nulFixture, firstNULLine int64) {
	t.Helper()
	chunkedTraceEnv(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	text := writeNeedleText(8<<20, func(int) bool { return false })
	path := writeTextFile(t, "capped-nul.log", text)
	tasks, err := CreateFileTasks(path)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) < 2 {
		t.Fatalf("%d chunk(s); the test needs at least 2", len(tasks))
	}
	table := newTextLineTable(text)
	first := table.byStart[tasks[len(tasks)-1].Offset] + 1 // 1-based number of the last chunk's first line
	for n := first + 5; n < first+2000; n += 10 {
		markNeedles(text, table, n)
	}
	putNULsAround(text, table, first+2, first+15, first+101)
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	return nulFixture{path: path, text: text, table: newTextLineTable(text)}, table.starts[first+1]
}

// Scenario: a capped trace of a chunked file with NUL bytes in the chunk
// the cap stops in. Without an index the scan may leave lines at -1;
// --no-index counts them, and a line index numbers them. Every number
// filled in is the line's, and the three answers agree under the
// accelerator rule.
func TestCappedTraceOfAFileWithNULBytesNumbersLinesAsTheText(t *testing.T) {
	fx, firstNULLine := writeCappedNULFixture(t)
	limit := 40
	opts := Options{MaxResults: &limit, ContextBefore: nulContext, ContextAfter: nulContext, NoCache: true}

	cold := traceOnce(t, fx.path, []string{"NEEDLE"}, opts)
	if len(cold.Matches) != limit {
		t.Fatalf("the capped trace returned %d matches, want %d", len(cold.Matches), limit)
	}
	for _, m := range cold.Matches {
		if m.Offset <= firstNULLine {
			t.Fatalf("match at offset %d lies before the first NUL line at %d; the fixture tests nothing", m.Offset, firstNULLine)
		}
	}
	checkAnswerAgainstText(t, cold, fx.table, false)

	withoutIndex := opts
	withoutIndex.NoIndex = true
	noIndex := traceOnce(t, fx.path, []string{"NEEDLE"}, withoutIndex)
	if n := traceanswer.UnnumberedLines(noIndex); n != 0 {
		t.Fatalf("--no-index left %d line numbers at -1", n)
	}
	checkAnswerAgainstText(t, noIndex, fx.table, false)

	idx, err := index.Build(fx.path, index.BuildOptions{StepBytes: 64 << 10})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("save index: %v", err)
	}
	indexed := traceOnce(t, fx.path, []string{"NEEDLE"}, opts)
	checkAnswerAgainstText(t, indexed, fx.table, false)

	traceanswer.RequireAgree(t, "--no-index against cold", noIndex, cold, fx.text)
	traceanswer.RequireAgree(t, "line index against cold", indexed, cold, fx.text)
}

// lastVersionThatSplitNULLines is the last trace-cache format version
// written by scans that let ripgrep split lines at NUL bytes.
const lastVersionThatSplitNULLines = 5

// A trace cache of that version may hold a match at an offset inside its
// line. Such a cache is not read; the trace scans the file again and
// answers as the text says.
func TestTraceCacheFromBeforeNULLinesWereWholeIsNotRead(t *testing.T) {
	layout := nulLayouts[0]
	layout.env(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	fx := writeNULFixture(t, layout.size, layout.pick)
	opts := Options{ContextBefore: nulContext, ContextAfter: nulContext}
	fresh := traceOnce(t, fx.path, []string{"NEEDLE"}, opts)

	// Rewrite the cache the way the older scan left it: the match on
	// the NUL needle line (line 485) stored at the byte after the NUL.
	cachePath := CachePath(fx.path, []string{"NEEDLE"}, nil)
	data, err := LoadCache(cachePath)
	if err != nil {
		t.Fatalf("load cache: %v", err)
	}
	nulLineStart := fx.table.starts[484]
	split := false
	for i := range data.Matches {
		if data.Matches[i].Offset == nulLineStart {
			data.Matches[i].Offset = nulLineStart + 5
			split = true
		}
	}
	if !split {
		t.Fatalf("the cache holds no match at the NUL line's offset %d", nulLineStart)
	}
	data.Version = lastVersionThatSplitNULLines
	if err := SaveCache(cachePath, data); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	again := traceOnce(t, fx.path, []string{"NEEDLE"}, opts)
	checkAnswerAgainstText(t, again, fx.table, true)
	traceanswer.RequireSame(t, "trace over an older cache", again, fresh)
}

// Scenario: every compressed copy of a log with NUL bytes answers as the
// text says, the way the plain file does: offsets are positions in the
// decompressed text. A second trace of each copy gives the same answer.
func TestNULBytesInACompressedCopyKeepEveryLineWhole(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	text := nulNeedleText(2 << 20)
	table := newTextLineTable(text)
	putNULsAround(text, table, 485, 486, len(table.starts)/2, len(table.starts)/2+97)
	table = newTextLineTable(text)

	formats := map[string]string{
		compressedcopy.Gzip:         "nul.log.gz",
		compressedcopy.Zstd:         "nul.log.zst",
		compressedcopy.SeekableZstd: "nul.seekable.zst",
		compressedcopy.Xz:           "nul.log.xz",
		compressedcopy.Bzip2:        "nul.log.bz2",
	}
	for format, name := range formats {
		t.Run(format, func(t *testing.T) {
			encoded := compressedcopy.Encode(t, format, text)
			if encoded == nil {
				t.Skipf("no encoder for %s on this host", format)
			}
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			opts := Options{ContextBefore: nulContext, ContextAfter: nulContext}

			first := traceOnce(t, path, []string{"NEEDLE"}, opts)
			checkAnswerAgainstText(t, first, table, true)
			second := traceOnce(t, path, []string{"NEEDLE"}, opts)
			traceanswer.RequireSame(t, "second trace", second, first)
		})
	}
}

// Scenario: a NUL in the first 8 KiB still makes a plain file binary,
// and the trace skips it.
func TestNULInTheFirst8KiBStillSkipsThePlainFile(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	text := nulNeedleText(64 << 10)
	table := newTextLineTable(text)
	putNULs(text, table, 3, 4)
	path := writeTextFile(t, "early-nul.log", text)

	resp, err := New().RunWithOptions(context.Background(), []string{path}, []string{"NEEDLE"}, Options{NoCache: true})
	if err != nil {
		t.Fatalf("trace: %v", err)
	}

	if len(resp.Matches) != 0 {
		t.Errorf("%d matches in a binary file, want none", len(resp.Matches))
	}
	if len(resp.SkippedFiles) != 1 || resp.SkippedFiles[0] != path {
		t.Errorf("skipped_files = %v, want [%s]", resp.SkippedFiles, path)
	}
}
