package trace

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A UTF-8 byte-order mark is three bytes of the file, and rx treats it
// as such: the BOM is part of the text of the line it starts, every
// offset counts it, and samples, the index and the trace cache read the
// same bytes. These tests check trace answers for files that start with
// a BOM, and for files with a BOM at the start of a later line (two
// logs joined with cat), against the bytes of the file.

// utf8BOM is the byte-order mark Windows tools and some exporters put
// at the start of a UTF-8 file. In Go source the same three bytes read
// as the rune U+FEFF.
const utf8BOM = "\xef\xbb\xbf"

// bomNeedleText returns a log of about size bytes that starts with a
// BOM and has a needle on line 1 and on every 97th line after it.
func bomNeedleText(size int) []byte {
	text := writeNeedleText(size, func(line int) bool { return line == 1 || line%97 == 0 })
	return append([]byte(utf8BOM), text...)
}

// putBOMAt overwrites the first three bytes of the 1-based line n with a
// BOM. No byte moves, so every offset stays where it was, and the line
// keeps its number in its text (the BOM, then "e <n> …").
func putBOMAt(text []byte, table textLineTable, n int) {
	copy(text[table.starts[n-1]:], utf8BOM)
}

// requireSamplesInvertTrace checks every match of a trace of a plain
// file against samples: samples --offsets=<offset> answers the match's
// line number and its line_text, and samples --lines=<n> answers the
// match's offset. Both surfaces must read a BOM the same way.
func requireSamplesInvertTrace(t *testing.T, path string, resp *rxtypes.TraceResponse) {
	t.Helper()
	var offsets, lines []samples.OffsetOrRange
	for _, m := range resp.Matches {
		offsets = append(offsets, samples.OffsetOrRange{Start: m.Offset})
		lines = append(lines, samples.OffsetOrRange{Start: int64(m.AbsoluteLineNumber)})
	}
	byOffset, err := samples.Resolve(samples.Request{Path: path, Offsets: offsets, IndexLoader: samples.NoIndex})
	if err != nil {
		t.Fatalf("samples --offsets: %v", err)
	}
	byLine, err := samples.Resolve(samples.Request{Path: path, Lines: lines, IndexLoader: samples.NoIndex})
	if err != nil {
		t.Fatalf("samples --lines: %v", err)
	}
	for _, m := range resp.Matches {
		offsetKey := strconv.FormatInt(m.Offset, 10)
		lineKey := strconv.Itoa(m.AbsoluteLineNumber)
		if got := byOffset.Offsets[offsetKey]; got != int64(m.AbsoluteLineNumber) {
			t.Errorf("samples --offsets=%d answers line %d; trace says line %d", m.Offset, got, m.AbsoluteLineNumber)
		}
		if got := byOffset.Samples[offsetKey]; len(got) != 1 || m.LineText == nil || got[0] != *m.LineText {
			t.Errorf("samples --offsets=%d answers %q; trace's line_text is %q", m.Offset, got, derefText(m.LineText))
		}
		if got := byLine.Lines[lineKey]; got != m.Offset {
			t.Errorf("samples --lines=%d answers offset %d; trace says offset %d", m.AbsoluteLineNumber, got, m.Offset)
		}
	}
}

// derefText returns the text a line_text pointer holds, or "<nil>".
func derefText(text *string) string {
	if text == nil {
		return "<nil>"
	}
	return *text
}

// requireBOMLineMatch checks the match on line 1, the line that starts
// with the BOM: its line_text holds the BOM, as samples shows it, and
// the submatch positions count the BOM's three bytes.
func requireBOMLineMatch(t *testing.T, resp *rxtypes.TraceResponse, table textLineTable) {
	t.Helper()
	wantText := table.texts[0]
	if !strings.HasPrefix(wantText, utf8BOM) {
		t.Fatalf("fixture line 1 %q does not start with a BOM", wantText)
	}
	wantStart := strings.Index(wantText, "NEEDLE")
	for _, m := range resp.Matches {
		if m.Offset != 0 {
			continue
		}
		if m.AbsoluteLineNumber != 1 || derefText(m.LineText) != wantText {
			t.Errorf("match at offset 0: line %d, line_text %q; want line 1, %q", m.AbsoluteLineNumber, derefText(m.LineText), wantText)
		}
		want := []rxtypes.Submatch{{Text: "NEEDLE", Start: wantStart, End: wantStart + len("NEEDLE")}}
		if len(m.Submatches) != 1 || m.Submatches[0] != want[0] {
			t.Errorf("match at offset 0: submatches %+v, want %+v", m.Submatches, want)
		}
		return
	}
	t.Error("no match on line 1, which holds NEEDLE after the BOM")
}

// Scenario: the three-line file of the report. The matches sit at the
// offsets the file has, line 1 keeps its BOM, and samples answers the
// same lines for those offsets.
func TestBOMFileOffsetsAreTheFilesOwn(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	text := []byte(utf8BOM + "LINE 1 a\nLINE 2 NEEDLE\nLINE 3 NEEDLE\n")
	path := writeTextFile(t, "bom.log", text)

	needles := traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
	var got []int64
	for _, m := range needles.Matches {
		got = append(got, m.Offset)
	}
	if len(got) != 2 || got[0] != 12 || got[1] != 26 {
		t.Errorf("NEEDLE offsets %v, want [12 26]", got)
	}
	requireSamplesInvertTrace(t, path, needles)

	lines := traceOnce(t, path, []string{"LINE"}, Options{NoCache: true})
	if len(lines.Matches) != 3 {
		t.Fatalf("LINE: %d matches, want 3", len(lines.Matches))
	}
	first := lines.Matches[0]
	wantFirst := rxtypes.Submatch{Text: "LINE", Start: 3, End: 7}
	if first.Offset != 0 || derefText(first.LineText) != utf8BOM+"LINE 1 a" ||
		len(first.Submatches) != 1 || first.Submatches[0] != wantFirst {
		t.Errorf("LINE on line 1: offset %d, line_text %q, submatches %+v; want 0, %q, [%+v]",
			first.Offset, derefText(first.LineText), first.Submatches, utf8BOM+"LINE 1 a", wantFirst)
	}
	requireSamplesInvertTrace(t, path, lines)

	// The BOM is the first thing on line 1, so a pattern anchored at the
	// start of a line does not match line 1, as `rg --encoding=none` and
	// grep answer.
	anchored := traceOnce(t, path, []string{"^LINE"}, Options{NoCache: true})
	if len(anchored.Matches) != 2 || anchored.Matches[0].Offset != 12 {
		t.Errorf("^LINE: %d matches, first at %d; want 2, first at 12", len(anchored.Matches), firstOffset(anchored))
	}
}

// firstOffset is the offset of the first match in resp, or -1.
func firstOffset(resp *rxtypes.TraceResponse) int64 {
	if len(resp.Matches) == 0 {
		return -1
	}
	return resp.Matches[0].Offset
}

// bomLayouts are the two shapes a plain BOM file is scanned in: one
// chunk, and several chunks scanned in parallel, where every chunk after
// the first also starts with a BOM (logs joined with cat). Both are
// large enough to be cached.
var bomLayouts = []struct {
	name       string
	size       int
	env        func(t *testing.T)
	wantChunks func(n int) bool
}{
	{
		name: "one chunk",
		size: 2 << 20,
		env: func(t *testing.T) {
			requireRipgrep(t)
			t.Setenv("RX_CACHE_DIR", t.TempDir())
		},
		wantChunks: func(n int) bool { return n == 1 },
	},
	{
		name:       "several chunks",
		size:       8 << 20,
		env:        chunkedTraceEnv,
		wantChunks: func(n int) bool { return n >= 3 },
	},
}

// writeBOMFixture writes a BOM log of about size bytes and puts a BOM on
// the first line of every chunk after the first, as a needle line, so a
// match and a window start at each chunk's first byte.
func writeBOMFixture(t *testing.T, size int) (path string, table textLineTable, chunks int) {
	t.Helper()
	text := bomNeedleText(size)
	path = writeTextFile(t, "bom.log", text)
	tasks, err := CreateFileTasks(path)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	table = newTextLineTable(text)
	for _, task := range tasks[1:] {
		n := table.byStart[task.Offset] + 1
		markNeedles(text, table, n)
		putBOMAt(text, table, n)
	}
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	return path, newTextLineTable(text), len(tasks)
}

// Scenarios: a BOM file of one chunk, and one of several chunks with a
// BOM at the start of each. The cold scan answers with the file's own
// offsets, line 1 keeps its BOM, samples inverts every match, and the
// cache hit, a scan with --no-index and a scan with a line index give
// the same answer.
func TestBOMInAPlainFileKeepsTheFilesOffsets(t *testing.T) {
	for _, layout := range bomLayouts {
		t.Run(layout.name, func(t *testing.T) {
			layout.env(t)
			t.Setenv("RX_LARGE_FILE_MB", "1")
			path, table, chunks := writeBOMFixture(t, layout.size)
			if !layout.wantChunks(chunks) {
				t.Fatalf("fixture scans in %d chunk(s); not the layout under test", chunks)
			}
			opts := Options{ContextBefore: nulContext, ContextAfter: nulContext}

			cold := traceOnce(t, path, []string{"NEEDLE"}, opts)
			checkAnswerAgainstText(t, cold, table, true)
			requireBOMLineMatch(t, cold, table)
			requireSamplesInvertTrace(t, path, cold)

			requireTraceCache(t, path)
			warm := traceFromCacheWith(t, path, opts)
			traceanswer.RequireSame(t, "cache hit", warm, cold)

			uncached := opts
			uncached.NoCache = true
			withoutIndex := uncached
			withoutIndex.NoIndex = true
			noIndex := traceOnce(t, path, []string{"NEEDLE"}, withoutIndex)
			traceanswer.RequireSame(t, "--no-index", noIndex, cold)

			idx, err := index.Build(path, index.BuildOptions{StepBytes: 64 << 10})
			if err != nil {
				t.Fatalf("build index: %v", err)
			}
			if _, err := index.Save(idx); err != nil {
				t.Fatalf("save index: %v", err)
			}
			indexed := traceOnce(t, path, []string{"NEEDLE"}, uncached)
			traceanswer.RequireSame(t, "line index", indexed, cold)
		})
	}
}

// Scenario: every compressed copy of a BOM log answers as the text says,
// the way the plain file does. A gzip, bzip2, xz or plain zstd stream is
// searched as one input, and a seekable copy in batches of frames, the
// first of which starts with the BOM. A second trace gives the same
// answer; for a seekable copy it is read from the trace cache, while a
// stream format is not cached and is scanned again.
func TestBOMInACompressedCopyKeepsTheTextsOffsets(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	// 4 MiB of text compresses to more than the 1 MiB from which a
	// seekable copy is cached.
	text := bomNeedleText(4 << 20)
	table := newTextLineTable(text)

	formats := []struct {
		format   string
		name     string
		isCached bool
	}{
		{compressedcopy.Gzip, "bom.log.gz", false},
		{compressedcopy.Bzip2, "bom.log.bz2", false},
		{compressedcopy.Xz, "bom.log.xz", false},
		{compressedcopy.Zstd, "bom.log.zst", false},
		{compressedcopy.SeekableZstd, "bom.seekable.zst", true},
	}
	for _, f := range formats {
		t.Run(f.format, func(t *testing.T) {
			encoded := compressedcopy.Encode(t, f.format, text)
			if encoded == nil {
				t.Skipf("no encoder for %s on this host", f.format)
			}
			path := filepath.Join(t.TempDir(), f.name)
			if err := os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatalf("write %s: %v", f.name, err)
			}
			opts := Options{ContextBefore: nulContext, ContextAfter: nulContext}

			first := traceOnce(t, path, []string{"NEEDLE"}, opts)
			checkAnswerAgainstText(t, first, table, true)
			requireBOMLineMatch(t, first, table)

			second := traceOnce(t, path, []string{"NEEDLE"}, opts)
			if f.isCached {
				requireTraceCache(t, path)
				second = traceFromCacheWith(t, path, opts)
			}
			traceanswer.RequireSame(t, "second trace", second, first)
		})
	}
}
