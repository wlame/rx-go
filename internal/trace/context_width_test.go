package trace

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A match's window is the matched line plus up to ContextBefore lines
// before it and up to ContextAfter lines after it, each counted on its
// own: `-B 12 -A 1` gives at most 12 lines before and 1 after, never 12
// after because a neighboring match asked for 12 before itself.

// contextWidthCases are the -B/-A pairs every file kind is checked with.
var contextWidthCases = []struct{ before, after int }{
	{12, 1}, {0, 3}, {3, 0}, {2, 2},
}

// textLines splits a text into its lines, without line breaks.
func textLines(text []byte) []string {
	return strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
}

// checkContextWindows checks that every match in resp has exactly the
// window lines[L-before .. L+after] (clamped to the file), in order,
// with the line numbers and texts of the file.
func checkContextWindows(t *testing.T, resp *rxtypes.TraceResponse, lines []string, before, after int) {
	t.Helper()
	if len(resp.Matches) == 0 {
		t.Fatal("no matches")
	}
	for _, m := range resp.Matches {
		key := fmt.Sprintf("%s:%s:%d", m.Pattern, m.File, m.Offset)
		matchLine := m.AbsoluteLineNumber
		var want []string
		for n := max(1, matchLine-before); n <= min(len(lines), matchLine+after); n++ {
			want = append(want, fmt.Sprintf("%d %s", n, lines[n-1]))
		}
		var got []string
		for _, cl := range resp.ContextLines[key] {
			got = append(got, fmt.Sprintf("%d %s", cl.AbsoluteLineNumber, cl.LineText))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("-B %d -A %d, match on line %d: window\n%s\nwant\n%s",
				before, after, matchLine, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// contextFixture is a 200-line log with NEEDLE on lines 50 and 64 (so
// the leading context of 64 reaches back to 52, inside a symmetric
// window of 50), on the neighbors 100 and 101, and near both ends.
func contextFixture() []byte {
	needles := map[int]bool{2: true, 50: true, 64: true, 100: true, 101: true, 199: true}
	var b bytes.Buffer
	for n := 1; n <= 200; n++ {
		word := "filler"
		if needles[n] {
			word = "NEEDLE"
		}
		fmt.Fprintf(&b, "LINE %d %s %s\n", n, word, strings.Repeat("x", n%23))
	}
	return b.Bytes()
}

// writeContextCopies writes text as a plain file, a gzip file and a
// seekable zstd file of several frames, and returns their paths.
func writeContextCopies(t *testing.T, text []byte) map[string]string {
	t.Helper()
	dir := t.TempDir()
	plain := filepath.Join(dir, "ctx.log")
	if err := os.WriteFile(plain, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(text)
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := os.WriteFile(plain+".gz", gz.Bytes(), 0o600); err != nil {
		t.Fatalf("write gzip: %v", err)
	}
	var zst bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 1024, Level: 1, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &zst); err != nil {
		t.Fatalf("encode seekable: %v", err)
	}
	if err := os.WriteFile(plain+".zst", zst.Bytes(), 0o600); err != nil {
		t.Fatalf("write seekable: %v", err)
	}
	return map[string]string{"plain": plain, "gzip": plain + ".gz", "seekable zstd": plain + ".zst"}
}

func TestContextWindowsTakeBeforeAndAfterOnTheirOwn(t *testing.T) {
	requireRipgrep(t)
	text := contextFixture()
	lines := textLines(text)
	for kind, path := range writeContextCopies(t, text) {
		for _, tc := range contextWidthCases {
			t.Run(fmt.Sprintf("%s/B%d-A%d", kind, tc.before, tc.after), func(t *testing.T) {
				resp := traceOnce(t, path, []string{"NEEDLE"},
					Options{NoCache: true, ContextBefore: tc.before, ContextAfter: tc.after})
				checkContextWindows(t, resp, lines, tc.before, tc.after)
			})
		}
	}
}

// The same windows come from a scan of a large file, which writes the
// trace cache, and from the cache hit that follows.
func TestContextWindowsFromACacheHitTakeBeforeAndAfterOnTheirOwn(t *testing.T) {
	path, _, _ := cacheFixture(t, []string{"NEEDLE"})
	text, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	lines := textLines(text)
	opts := Options{ContextBefore: 12, ContextAfter: 1}

	checkContextWindows(t, traceOnce(t, path, []string{"NEEDLE"}, opts), lines, 12, 1)
	checkContextWindows(t, cappedTraceFromCache(t, path, []string{"NEEDLE"}, opts), lines, 12, 1)
}

// A result cap can cut a match that lies inside the window of the last
// match kept. That line is still part of the window, as in the answer
// without the cap.
func TestContextWindowOfTheLastKeptMatchHoldsAMatchPastTheCap(t *testing.T) {
	match := func(line int) rxtypes.Match {
		text := fmt.Sprintf("LINE %d NEEDLE", line)
		return rxtypes.Match{
			Pattern: "p1", File: "f1", Offset: int64(line * 100),
			RelativeLineNumber: ptrInt(line), AbsoluteLineNumber: line, LineText: &text,
		}
	}
	context := func(line int) contextWithFile {
		return contextWithFile{fileID: "f1", ctx: rxtypes.ContextLine{
			RelativeLineNumber: line, AbsoluteLineNumber: line,
			LineText: fmt.Sprintf("LINE %d filler", line), AbsoluteOffset: int64(line * 100),
		}}
	}
	kept := []rxtypes.Match{match(100)}
	found := []rxtypes.Match{match(100), match(101)}
	contexts := []contextWithFile{context(98), context(99), context(102)}

	// Each line is 100 bytes long, so it ends where the next starts.
	ends := lineEnds{}
	for line := 98; line <= 102; line++ {
		ends.record("f1", int64(line*100), int64(line*100+100))
	}

	windows := buildContextDict(kept, found, contexts, ends, 2, 2)
	var got []int
	for _, cl := range windows["p1:f1:10000"] {
		got = append(got, cl.AbsoluteLineNumber)
	}
	if want := []int{98, 99, 100, 101, 102}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("window lines %v, want %v", got, want)
	}
	if len(windows) != 1 {
		t.Errorf("windows for %d matches, want one for the match kept", len(windows))
	}
}

// A result cap does not shorten the window of a match it keeps: the
// last match kept has the lines after it that the trace without the cap
// gives it, and so does a match whose window holds a match the cap cut
// (line 100, whose window holds the match on line 101).
func TestContextWindowsOfACappedTraceAreTheUncappedOnes(t *testing.T) {
	requireRipgrep(t)
	text := contextFixture()
	lines := textLines(text)
	const before, after = 1, 2
	for kind, path := range writeContextCopies(t, text) {
		for limit := 1; limit <= 6; limit++ {
			t.Run(fmt.Sprintf("%s/max%d", kind, limit), func(t *testing.T) {
				resp := traceOnce(t, path, []string{"NEEDLE"}, Options{
					NoCache: true, ContextBefore: before, ContextAfter: after, MaxResults: &limit,
				})
				if len(resp.Matches) != limit {
					t.Fatalf("got %d matches, want %d", len(resp.Matches), limit)
				}
				checkContextWindows(t, resp, lines, before, after)
			})
		}
	}
}

// A seekable batch that the cap stopped part-way returns the matches
// whose windows rg finished writing. A later match whose trailing
// context was cut off was never complete; it comes back as a context
// line, a line of the windows around it, rather than as a match with a
// shortened window.
func TestAStoppedSeekableBatchKeepsOnlyMatchesWithWholeWindows(t *testing.T) {
	requireRipgrep(t)
	text := "LINE 1 NEEDLE\nLINE 2\nLINE 3\nLINE 4\nLINE 5 NEEDLE\nLINE 6 NEEDLE\nLINE 7\nLINE 8\n"
	cmd := exec.Command("rg", "--json", "--no-config", "-A", "2", "-e", "NEEDLE", "-")
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rg: %v", err)
	}
	// Keep rg's events up to the line after the match on line 5, as if
	// rg was killed there: line 5's window misses line 7, and line 6's
	// misses lines 7 and 8.
	var cut []byte
	for _, event := range bytes.SplitAfter(out, []byte("\n")) {
		cut = append(cut, event...)
		if bytes.Contains(event, []byte(`"line_number":6`)) {
			break
		}
	}
	segments := []streamSegment{{frame: seekable.FrameInfo{DecompressedSize: int64(len(text))}, lineShift: 0}}
	locs := []frameLoc{{frameIdx: 0, lineCount: 8, decoded: true}}

	matches, contexts, _, err := matchesFromPartialBatch(context.Background(), cut, wholeStream(segments), locs, []string{"p1"}, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	var matchLines, contextLines []int
	for _, m := range matches {
		matchLines = append(matchLines, m.LineNumber)
	}
	for _, c := range contexts {
		contextLines = append(contextLines, c.LineNumber)
	}
	if fmt.Sprint(matchLines) != "[1]" || fmt.Sprint(contextLines) != "[2 3 5 6]" {
		t.Errorf("matches on lines %v, context lines %v; want matches [1], context lines [2 3 5 6]",
			matchLines, contextLines)
	}
}
