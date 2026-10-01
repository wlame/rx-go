package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A match's window holds the lines around it in the file's text. These
// tests check every window line against the plain text at the line's
// own byte offset, which needs no line number: a capped trace leaves
// some numbers unknown, and a window line taken from another chunk or
// frame shows as a wrong offset or a wrong text.

// textLineTable lists the lines of a text by the offset of their first
// byte.
type textLineTable struct {
	starts  []int64
	texts   []string
	byStart map[int64]int // first byte → 0-based line index
}

func newTextLineTable(text []byte) textLineTable {
	table := textLineTable{byStart: map[int64]int{}}
	var pos int64
	for _, line := range bytes.SplitAfter(text, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		table.byStart[pos] = len(table.starts)
		table.starts = append(table.starts, pos)
		table.texts = append(table.texts, strings.TrimSuffix(string(line), "\n"))
		pos += int64(len(line))
	}
	return table
}

// checkWindowsByOffset checks that every match in resp has exactly the
// lines [i-before, i+after] of the text around its own line i (clamped
// to the text) as its window, in order, each with the offset and the
// text the plain file has there. A line number, where the answer gives
// one, must be the line's.
func checkWindowsByOffset(t *testing.T, resp *rxtypes.TraceResponse, table textLineTable, before, after int) {
	t.Helper()
	if len(resp.Matches) == 0 {
		t.Fatal("no matches")
	}
	for _, m := range resp.Matches {
		i, ok := table.byStart[m.Offset]
		if !ok {
			t.Fatalf("match at offset %d is not at the start of a line", m.Offset)
		}
		if m.AbsoluteLineNumber != -1 && m.AbsoluteLineNumber != i+1 {
			t.Errorf("match at offset %d: line %d, want %d", m.Offset, m.AbsoluteLineNumber, i+1)
		}
		var want []string
		for j := max(0, i-before); j <= min(len(table.starts)-1, i+after); j++ {
			want = append(want, fmt.Sprintf("@%d %s", table.starts[j], table.texts[j]))
		}
		key := fmt.Sprintf("%s:%s:%d", m.Pattern, m.File, m.Offset)
		var got []string
		for _, cl := range resp.ContextLines[key] {
			got = append(got, fmt.Sprintf("@%d %s", cl.AbsoluteOffset, cl.LineText))
			j, known := table.byStart[cl.AbsoluteOffset]
			if known && cl.AbsoluteLineNumber != -1 && cl.AbsoluteLineNumber != j+1 {
				t.Errorf("window of offset %d: line at offset %d numbered %d, want %d",
					m.Offset, cl.AbsoluteOffset, cl.AbsoluteLineNumber, j+1)
			}
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("-B %d -A %d, match at offset %d (line %d): window\n%s\nwant\n%s",
				before, after, m.Offset, i+1, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// writeNeedleText returns a log of about targetBytes whose lines read
// "line <n> <kind> <payload>", kind NEEDLE where isNeedle(n) and filler
// elsewhere. Both kinds have the same length, so turning a filler line
// into a needle moves no byte. The payload is random hex from a fixed
// seed, which keeps a compressed copy near half the size of the text.
func writeNeedleText(targetBytes int, isNeedle func(line int) bool) []byte {
	rng := rand.New(rand.NewPCG(87, 1))
	var b bytes.Buffer
	for line := 1; b.Len() < targetBytes; line++ {
		kind := "filler"
		if isNeedle(line) {
			kind = "NEEDLE"
		}
		fmt.Fprintf(&b, "line %d %s %016x%016x%016x\n", line, kind, rng.Uint64(), rng.Uint64(), rng.Uint64())
	}
	return b.Bytes()
}

// markNeedles turns the filler lines numbered in lines into needles,
// in place.
func markNeedles(text []byte, table textLineTable, lines ...int) {
	for _, n := range lines {
		if n < 1 || n > len(table.starts) {
			continue
		}
		start := table.starts[n-1]
		at := bytes.Index(text[start:], []byte(" filler "))
		copy(text[start+int64(at):], " NEEDLE ")
	}
}

// writeTextFile writes text to name in a new temporary directory.
func writeTextFile(t *testing.T, name string, text []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// lineFramesOf cuts text into frames of whole lines, each at least size
// bytes but the last, the way rx compress cuts them.
func lineFramesOf(text []byte, size int) [][]byte {
	var cuts []int
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	pos, frameStart := 0, 0
	for sc.Scan() {
		pos += len(sc.Bytes()) + 1
		if pos-frameStart >= size {
			cuts = append(cuts, pos)
			frameStart = pos
		}
	}
	return seekablefile.SplitAt(text, cuts...)
}

// chunkedTraceEnv makes a plain file of a few MB split into four chunks
// scanned in parallel, with an empty cache of its own.
func chunkedTraceEnv(t *testing.T) {
	t.Helper()
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	t.Setenv("RX_WORKERS", "4")
}

// A window is built from the lines next to the match in the file, never
// from lines that merely carry the same chunk-relative number: the
// unnumbered line from another chunk (relative number 49, offset 900000)
// is not a neighbor of the match on line 50.
func TestContextWindowTakesNoLineFromAnotherChunk(t *testing.T) {
	line := func(n int, offset int64, text string) rxtypes.ContextLine {
		return rxtypes.ContextLine{RelativeLineNumber: n, AbsoluteLineNumber: n, LineText: text, AbsoluteOffset: offset}
	}
	matchText := "LINE 50 NEEDLE"
	match := rxtypes.Match{
		Pattern: "p1", File: "f1", Offset: 5000,
		RelativeLineNumber: ptrInt(50), AbsoluteLineNumber: 50, LineText: &matchText,
	}
	elsewhere := line(49, 900000, "LINE 9049 of the next chunk")
	elsewhere.AbsoluteLineNumber = -1
	contexts := []contextWithFile{
		{fileID: "f1", ctx: line(48, 4800, "LINE 48")},
		{fileID: "f1", ctx: line(49, 4900, "LINE 49")},
		{fileID: "f1", ctx: line(51, 5100, "LINE 51")},
		{fileID: "f1", ctx: line(52, 5200, "LINE 52")},
		{fileID: "f1", ctx: elsewhere},
	}
	ends := lineEnds{}
	for _, c := range contexts {
		ends[lineAt{fileID: "f1", offset: c.ctx.AbsoluteOffset}] = c.ctx.AbsoluteOffset + 100
	}
	ends[lineAt{fileID: "f1", offset: 5000}] = 5100

	windows := buildContextDict([]rxtypes.Match{match}, []rxtypes.Match{match}, contexts, ends, 2, 2)

	var got []int64
	for _, cl := range windows["p1:f1:5000"] {
		got = append(got, cl.AbsoluteOffset)
	}
	if want := []int64{4800, 4900, 5000, 5100, 5200}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("window offsets %v, want %v", got, want)
	}
}

// Scenario: a capped trace of a plain file in four chunks, without an
// index, so the chunks after the first one the cap cut keep their lines
// unnumbered. Every window line is the file's line at its offset, next
// to the match. The cap stops the chunks at points that vary from run to
// run, so each layout runs several caps.
func TestCappedTraceOfAChunkedFileShowsTheFileLinesAroundEachMatch(t *testing.T) {
	chunkedTraceEnv(t)
	const size = 8 << 20
	layouts := map[string]func(int) bool{
		"needles everywhere": func(n int) bool { return n%10 == 0 },
		// No match in the first chunks: the matches kept come from a
		// chunk whose start the cap left uncounted.
		"needles in the last half": func(n int) bool { return n > 55000 && n%10 == 0 },
	}
	for name, isNeedle := range layouts {
		text := writeNeedleText(size, isNeedle)
		table := newTextLineTable(text)
		path := writeTextFile(t, "capped.log", text)
		for _, limit := range []int{3, 40, 400} {
			for _, window := range []struct{ before, after int }{{2, 2}, {3, 1}} {
				t.Run(fmt.Sprintf("%s/max%d/B%d-A%d", name, limit, window.before, window.after), func(t *testing.T) {
					resp := traceOnce(t, path, []string{"NEEDLE"}, Options{
						NoCache: true, MaxResults: &limit,
						ContextBefore: window.before, ContextAfter: window.after,
					})
					checkWindowsByOffset(t, resp, table, window.before, window.after)
				})
			}
		}
	}
}

// Scenario: the same for a seekable zstd file of several batches of
// frames, with frames that end at line breaks and frames that cut
// lines.
func TestCappedTraceOfASeekableFileShowsTheFileLinesAroundEachMatch(t *testing.T) {
	chunkedTraceEnv(t)
	text := writeNeedleText(1<<20, func(n int) bool { return n%7 == 0 })
	table := newTextLineTable(text)
	layouts := map[string][][]byte{
		"frames of whole lines":      lineFramesOf(text, 1024),
		"frames cut every 997 bytes": seekablefile.SplitEvery(text, 997),
	}
	for name, frames := range layouts {
		if len(frames) <= 2*framesPerBatch {
			t.Fatalf("%s: %d frames, the test needs more than %d", name, len(frames), 2*framesPerBatch)
		}
		path := filepath.Join(t.TempDir(), "capped.log.zst")
		seekablefile.Write(t, path, frames)
		for _, limit := range []int{3, 40, 400} {
			t.Run(fmt.Sprintf("%s/max%d", name, limit), func(t *testing.T) {
				resp := traceOnce(t, path, []string{"NEEDLE"}, Options{
					NoCache: true, MaxResults: &limit, ContextBefore: 2, ContextAfter: 2,
				})
				checkWindowsByOffset(t, resp, table, 2, 2)
			})
		}
	}
}

// comparableAnswer is a trace answer as a parsed JSON document, without
// the fields that differ between two runs of one request.
func comparableAnswer(t *testing.T, resp *rxtypes.TraceResponse) map[string]any {
	t.Helper()
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"time", "request_id", "cli_command"} {
		delete(doc, field)
	}
	return doc
}

// requireEqualAnswers fails when two answers differ, naming the first
// window that does.
func requireEqualAnswers(t *testing.T, name string, got, want *rxtypes.TraceResponse) {
	t.Helper()
	if reflect.DeepEqual(comparableAnswer(t, got), comparableAnswer(t, want)) {
		return
	}
	for key, window := range want.ContextLines {
		if !reflect.DeepEqual(got.ContextLines[key], window) {
			t.Fatalf("%s: window %s differs:\n got %+v\nwant %+v", name, key, got.ContextLines[key], window)
		}
	}
	t.Fatalf("%s: answers differ outside the context windows", name)
}

// Scenario: one match next to each chunk boundary, alternately the
// chunk's first line, the line before it and the chunk's second line,
// so its window reaches across the boundary and no other match's window
// holds the lines it needs there. A scan that writes the
// trace cache, the cache hit after it and a --no-index scan answer the
// same, and each window is the file's lines around its match.
func TestWindowsAcrossAChunkBoundaryAreTheSameColdWarmAndWithoutAnIndex(t *testing.T) {
	chunkedTraceEnv(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	const before, after = 3, 3
	text := writeNeedleText(4<<20, func(n int) bool { return n == 5 })
	path := writeTextFile(t, "boundary.log", text)
	tasks, err := CreateFileTasks(path)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) < 2 {
		t.Fatalf("%d chunk(s); the test needs at least 2", len(tasks))
	}
	table := newTextLineTable(text)
	for i, task := range tasks[1:] {
		first := table.byStart[task.Offset] + 1 // 1-based number of the chunk's first line
		markNeedles(text, table, first+[]int{0, -1, 1}[i%3])
	}
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	table = newTextLineTable(text)

	opts := Options{ContextBefore: before, ContextAfter: after}
	cold := traceOnce(t, path, []string{"NEEDLE"}, opts)
	checkWindowsByOffset(t, cold, table, before, after)
	requireTraceCache(t, path)
	warm := cappedTraceFromCache(t, path, []string{"NEEDLE"}, opts)
	requireEqualAnswers(t, "cache hit", warm, cold)
	noIndex := traceOnce(t, path, []string{"NEEDLE"}, Options{
		ContextBefore: before, ContextAfter: after, NoCache: true, NoIndex: true,
	})
	requireEqualAnswers(t, "--no-index", noIndex, cold)
}

// Scenario: the same for a seekable zstd file, with one match next to
// each place where a batch of frames hands over to the next: the line
// holding the first byte of a batch is the last line the batch before
// it reads, so the match is alternately that line and the one after.
func TestWindowsAcrossABatchBoundaryAreTheSameColdWarmAndWithoutAnIndex(t *testing.T) {
	chunkedTraceEnv(t)
	const before, after = 3, 3
	text := writeNeedleText(3<<20, func(n int) bool { return n == 5 })
	table := newTextLineTable(text)
	layouts := map[string]func([]byte) [][]byte{
		"frames of whole lines":       func(text []byte) [][]byte { return lineFramesOf(text, 4096) },
		"frames cut every 4001 bytes": func(text []byte) [][]byte { return seekablefile.SplitEvery(text, 4001) },
	}
	for name, cut := range layouts {
		t.Run(name, func(t *testing.T) {
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			marked := append([]byte(nil), text...)
			frames := cut(marked)
			if len(frames) <= 2*framesPerBatch {
				t.Fatalf("%d frames, the test needs more than %d", len(frames), 2*framesPerBatch)
			}
			// The line holding the first byte of each batch's first frame.
			offset := 0
			for i, frame := range frames {
				if i > 0 && i%framesPerBatch == 0 {
					line := lineHolding(table, int64(offset))
					markNeedles(marked, table, line+(i/framesPerBatch)%2)
				}
				offset += len(frame)
			}
			path := filepath.Join(t.TempDir(), "boundary.log.zst")
			seekablefile.Write(t, path, cut(marked))
			markedTable := newTextLineTable(marked)

			opts := Options{ContextBefore: before, ContextAfter: after}
			cold := traceOnce(t, path, []string{"NEEDLE"}, opts)
			checkWindowsByOffset(t, cold, markedTable, before, after)
			requireTraceCache(t, path)
			warm := traceOnce(t, path, []string{"NEEDLE"}, opts)
			requireEqualAnswers(t, "cache hit", warm, cold)
			noIndex := traceOnce(t, path, []string{"NEEDLE"}, Options{
				ContextBefore: before, ContextAfter: after, NoCache: true, NoIndex: true,
			})
			requireEqualAnswers(t, "--no-index", noIndex, cold)
		})
	}
}

// requireTraceCache fails unless the trace cache holds an answer for a
// search of path for NEEDLE, so the next trace is a cache hit.
func requireTraceCache(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(CachePath(path, []string{"NEEDLE"}, nil)); err != nil {
		t.Fatalf("no trace cache for %s: %v", path, err)
	}
}

// lineHolding is the 1-based number of the line holding byte offset.
func lineHolding(table textLineTable, offset int64) int {
	i := 0
	for i+1 < len(table.starts) && table.starts[i+1] <= offset {
		i++
	}
	return i + 1
}
