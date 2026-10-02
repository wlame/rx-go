package samples

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
)

// gotLine is one line a span received, for comparison.
type gotLine struct {
	Number int64
	Text   string
	Length int64
}

// spanLines reads spans of the file at path and returns the lines each
// span received, in order.
func spanLines(t *testing.T, path string, req SpansRequest) [][]gotLine {
	t.Helper()
	got, err := readSpansOf(t, path, req, func(int, SpanLine) SpanAction { return SpanContinue })
	if err != nil {
		t.Fatalf("ReadLineSpans(%s): %v", filepath.Base(path), err)
	}
	return got
}

// readSpansOf pins and classifies path, reads req's spans of it with
// decide choosing each step, and returns what each span received.
func readSpansOf(t *testing.T, path string, req SpansRequest, decide func(int, SpanLine) SpanAction) ([][]gotLine, error) {
	t.Helper()
	src, err := paths.Pin(path)
	if err != nil {
		t.Fatal(err)
	}
	kind, err := filekind.OfPinnedForReading(src)
	if err != nil {
		t.Fatal(err)
	}
	req.Source, req.Kind = src, kind
	got := make([][]gotLine, len(req.Spans))
	err = ReadLineSpans(t.Context(), req, func(span int, line SpanLine) SpanAction {
		got[span] = append(got[span], gotLine{line.Number, string(line.Text), line.Length})
		return decide(span, line)
	})
	return got, err
}

// wantSpanLines is what each span must receive from text: the lines of
// its window, each without its line ending, cut to keep bytes.
func wantSpanLines(text []byte, spans []LineSpan, before, after, keep int) [][]gotLine {
	all := strings.Split(string(text), "\n")
	if strings.HasSuffix(string(text), "\n") {
		all = all[:len(all)-1]
	}
	out := make([][]gotLine, len(spans))
	for i, s := range spans {
		first := max(1, s.FirstLine-int64(before))
		last := min(int64(len(all)), s.LastLine+int64(after))
		for n := first; n <= last; n++ {
			line := strings.TrimSuffix(all[n-1], "\r")
			if n == int64(len(all)) && !strings.HasSuffix(string(text), "\n") {
				line = all[n-1] // a last line without a line break keeps a \r
			}
			out[i] = append(out[i], gotLine{n, line[:min(len(line), keep)], int64(len(line))})
		}
	}
	return out
}

// lineSpansAt returns spans of the given (first, last) line pairs of a
// text whose lines start at starts.
func lineSpansAt(starts []int64, pairs ...[2]int64) []LineSpan {
	out := make([]LineSpan, len(pairs))
	for i, p := range pairs {
		out[i] = LineSpan{FirstLine: p[0], LastLine: p[1], StartOffset: starts[p[0]-1]}
	}
	return out
}

// Every span gets the lines of its window, before and after included,
// numbered as the file numbers them, whatever the file's storage and
// whether an index exists: overlapping spans each get their lines, a
// window is cut at the file's first and last line, and the last line
// without a line break is a line.
func TestReadLineSpans_GivesEachSpanItsLinesOnEveryFormat(t *testing.T) {
	text, starts := variedLog(3000, 5)
	dir := t.TempDir()
	plain := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, text, 0o600); err != nil {
		t.Fatal(err)
	}
	files := compressedCopiesOf(t, text, dir)
	files["app.log"] = plain
	spans := lineSpansAt(starts, [2]int64{1, 2}, [2]int64{10, 12}, [2]int64{11, 11}, [2]int64{37, 37},
		[2]int64{400, 405}, [2]int64{2000, 2000}, [2]int64{2999, 3000})
	const before, after, keep = 2, 3, 1 << 20
	want := wantSpanLines(text, spans, before, after, keep)
	for name, path := range files {
		for loaderName, loader := range loadersFor(t, path) {
			t.Run(name+"/"+loaderName, func(t *testing.T) {
				got := spanLines(t, path, SpansRequest{Spans: spans, Before: before, After: after, KeepBytes: keep, IndexLoader: loader})
				for i := range spans {
					if !slices.Equal(got[i], want[i]) {
						t.Errorf("span %d (lines %d-%d):\ngot  %v\nwant %v", i, spans[i].FirstLine, spans[i].LastLine, got[i], want[i])
					}
				}
			})
		}
	}
}

// A line longer than KeepBytes is cut there, its whole length still
// reported without its line ending; a \r\n ending is not part of the
// text or the length.
func TestReadLineSpans_CutsLongLinesAndKeepsTheirLength(t *testing.T) {
	text := []byte("short\r\n" + strings.Repeat("y", 100) + "\r\n" + strings.Repeat("z", 9) + "\r\nend\r")
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	got := spanLines(t, path, SpansRequest{Spans: []LineSpan{{FirstLine: 1, LastLine: 4}}, KeepBytes: 10})
	want := []gotLine{{1, "short", 5}, {2, strings.Repeat("y", 10), 100}, {3, strings.Repeat("z", 9), 9}, {4, "end\r", 4}}
	if !slices.Equal(got[0], want) {
		t.Errorf("got %v\nwant %v", got[0], want)
	}
}

// A span that says it is done gets no more lines, and StopReading ends
// the pass: no span gets a line after it.
func TestReadLineSpans_DoneAndStop(t *testing.T) {
	text, starts := variedLog(200, 9)
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	spans := lineSpansAt(starts, [2]int64{10, 20}, [2]int64{12, 13}, [2]int64{100, 101})
	got, err := readSpansOf(t, path, SpansRequest{Spans: spans, KeepBytes: 1000}, func(span int, line SpanLine) SpanAction {
		switch {
		case span == 0 && line.Number == 11:
			return SpanDone
		case span == 1 && line.Number == 13:
			return StopReading
		}
		return SpanContinue
	})
	if err != nil {
		t.Fatal(err)
	}
	numbers := func(lines []gotLine) []int64 {
		var out []int64
		for _, l := range lines {
			out = append(out, l.Number)
		}
		return out
	}
	if g := numbers(got[0]); !slices.Equal(g, []int64{10, 11}) {
		t.Errorf("span 0 got lines %v, want 10-11", g)
	}
	if g := numbers(got[1]); !slices.Equal(g, []int64{12, 13}) {
		t.Errorf("span 1 got lines %v, want 12-13", g)
	}
	if len(got[2]) != 0 {
		t.Errorf("span 2 got lines %v after the pass stopped", numbers(got[2]))
	}
}

// A span whose own lines pass the end of the text (the file got shorter
// since it was scanned) is an error naming the file; lines after a
// span that the end of the text cuts are not.
func TestReadLineSpans_SpanPastTheEndIsAnError(t *testing.T) {
	text, starts := variedLog(50, 3)
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSpansOf(t, path, SpansRequest{Spans: lineSpansAt(starts, [2]int64{49, 50}), After: 5, KeepBytes: 100},
		func(int, SpanLine) SpanAction { return SpanContinue }); err != nil {
		t.Errorf("after-lines cut by the end: %v", err)
	}
	_, err := readSpansOf(t, path, SpansRequest{Spans: []LineSpan{{FirstLine: 49, LastLine: 52, StartOffset: starts[48]}}, KeepBytes: 100},
		func(int, SpanLine) SpanAction { return SpanContinue })
	if !errors.Is(err, ErrSpanPastEnd) || !strings.Contains(err.Error(), path) {
		t.Errorf("error %v, want ErrSpanPastEnd naming %s", err, path)
	}
	// A span past the end of the text the caller gives is the same
	// error, though the file holds its lines.
	_, err = readSpansOf(t, path, SpansRequest{Spans: lineSpansAt(starts, [2]int64{40, 45}), KeepBytes: 100, TextLines: 42, TextBytes: starts[42]},
		func(int, SpanLine) SpanAction { return SpanContinue })
	if !errors.Is(err, ErrSpanPastEnd) {
		t.Errorf("a span past the given end: error %v, want ErrSpanPastEnd", err)
	}
}

// Given the end of the text a caller saw (its line count and length),
// the spans of a file that has grown since get exactly the lines that
// text holds: no line past its last, and its last line, which had no
// line break and was written on since, cut at the length seen with that
// length reported. Every storage, with and without an index.
func TestReadLineSpans_StopsAtTheEndOfTheTextGiven(t *testing.T) {
	seen, starts := variedLog(50, 7)
	grown := append(bytes.Clone(seen), []byte(" TAIL-NOT-SEEN\nLINE 51 NOT SEEN\nLINE 52 NOT SEEN\n")...)
	dir := t.TempDir()
	plain := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, grown, 0o600); err != nil {
		t.Fatal(err)
	}
	files := compressedCopiesOf(t, grown, dir)
	files["app.log"] = plain
	spans := lineSpansAt(starts, [2]int64{44, 45}, [2]int64{49, 50})
	const before, after, keep = 2, 4, 1 << 10
	want := wantSpanLines(seen, spans, before, after, keep)
	for name, path := range files {
		for loaderName, loader := range loadersFor(t, path) {
			t.Run(name+"/"+loaderName, func(t *testing.T) {
				got := spanLines(t, path, SpansRequest{Spans: spans, Before: before, After: after, KeepBytes: keep,
					IndexLoader: loader, TextLines: 50, TextBytes: int64(len(seen))})
				for i := range spans {
					if !slices.Equal(got[i], want[i]) {
						t.Errorf("span %d (lines %d-%d):\ngot  %v\nwant %v", i, spans[i].FirstLine, spans[i].LastLine, got[i], want[i])
					}
				}
			})
		}
	}
}

// A plain file is read only around its spans: the pass seeks to each
// span's first line (finding the lines before it by reading back), so
// spans far apart in a large file cost their own bytes, not the bytes
// between them.
func TestReadLineSpans_ReadsOnlyAroundTheSpansOfAPlainFile(t *testing.T) {
	text, starts := variedLog(80000, 21)
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	spans := lineSpansAt(starts, [2]int64{20000, 20004}, [2]int64{50000, 50000}, [2]int64{79000, 79010})
	const before, after = 2, 1
	windowBytes := int64(0)
	for _, s := range spans {
		first, last := s.FirstLine-before, s.LastLine+after
		windowBytes += starts[last] - starts[first-1]
	}
	counter := withCountingOpen(t)
	got := spanLines(t, path, SpansRequest{Spans: spans, Before: before, After: after, KeepBytes: 1 << 20})
	if want := wantSpanLines(text, spans, before, after, 1<<20); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("lines differ:\ngot  %v\nwant %v", got, want)
	}
	// Each span: its window, the lines before it read twice (back, then
	// forward), and a read buffer and a back block of slack.
	budget := 2*windowBytes + int64(len(spans))*(spanPlainBuffer+spanBackBlock)
	if read := counter.Load(); read > budget || read*20 > int64(len(text)) {
		t.Errorf("read %d bytes of a %d-byte file for %d bytes of windows (budget %d)", read, len(text), windowBytes, budget)
	}
}

// A stream that cannot seek is read from its start, and the pass stops
// after the last span: spans near the start of a gzip file do not
// decompress the rest.
func TestReadLineSpans_StreamStopsAfterTheLastSpan(t *testing.T) {
	text, starts := variedLog(60000, 31)
	gz := compressedCopiesOf(t, text, t.TempDir())["app.log.gz"]
	info, err := os.Stat(gz)
	if err != nil {
		t.Fatal(err)
	}
	spans := lineSpansAt(starts, [2]int64{100, 102}, [2]int64{2000, 2001})
	counter := withCountingOpen(t)
	got := spanLines(t, gz, SpansRequest{Spans: spans, Before: 1, After: 1, KeepBytes: 1 << 20, IndexLoader: NoIndex})
	if want := wantSpanLines(text, spans, 1, 1, 1<<20); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("lines differ")
	}
	if read := counter.Load(); read*4 > info.Size() {
		t.Errorf("read %d of %d compressed bytes for spans in the first 4%% of the text", read, info.Size())
	}
}

// A seekable file with its frame table decodes only the frames around
// each span, however far apart the spans are.
func TestReadLineSpans_SeekableDecodesOnlyTheFramesAroundTheSpans(t *testing.T) {
	zstPath, loader, _ := makeIndexedSeekable(t, 50000, 64*1024)
	lineText := func(n int) string { return fmt.Sprintf("log line number %d with padding to make frames", n) }
	offsetOf := func(line int) int64 {
		var off int64
		for n := 1; n < line; n++ {
			off += int64(len(lineText(n)) + 1)
		}
		return off
	}
	decoded := map[int]int{}
	orig := decodeSeekableFrame
	decodeSeekableFrame = func(d *seekable.Decoder, src paths.Pinned, frame int, table *seekable.SeekTable) ([]byte, error) {
		decoded[frame]++
		return orig(d, src, frame, table)
	}
	t.Cleanup(func() { decodeSeekableFrame = orig })
	spans := []LineSpan{{FirstLine: 5000, LastLine: 5001, StartOffset: offsetOf(5000)}, {FirstLine: 45000, LastLine: 45000, StartOffset: offsetOf(45000)}}
	got := spanLines(t, zstPath, SpansRequest{Spans: spans, Before: 2, After: 2, KeepBytes: 1000, IndexLoader: loader})
	if g := got[1]; len(g) != 5 || g[2].Text != lineText(45000) {
		t.Fatalf("span 1 got %v", g)
	}
	if len(decoded) > 8 {
		t.Errorf("decoded %d frames for two spans far apart; the frames around them are enough", len(decoded))
	}
	if bytes.Contains([]byte(got[0][0].Text), []byte("\n")) {
		t.Errorf("a line holds a line break: %q", got[0][0].Text)
	}
}

// A plain file entered again at the very line the pass has reached
// (the lines before a span are long enough to look like a gap) reads
// on correctly from the one handle.
func TestReadLineSpans_PlainFileEnteredAtTheLineThePassIsAt(t *testing.T) {
	long := strings.Repeat("y", 40<<10)
	text := []byte("LINE 1\n" + long + "2\n" + long + "3\nLINE 4\nLINE 5\n")
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	starts := []int64{0}
	for i, b := range text {
		if b == '\n' && i+1 < len(text) {
			starts = append(starts, int64(i+1))
		}
	}
	spans := lineSpansAt(starts, [2]int64{1, 1}, [2]int64{4, 4})
	got := spanLines(t, path, SpansRequest{Spans: spans, Before: 2, KeepBytes: 1 << 20})
	if want := wantSpanLines(text, spans, 2, 0, 1<<20); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("lines differ:\ngot  %d lines %v\nwant %d lines", len(got[1]), got[1][len(got[1])-1], len(want[1]))
	}
}
