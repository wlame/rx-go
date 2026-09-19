package trace

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// fixtureLineOffsets returns the byte offset of the first byte of every
// line of path, keyed by the line's 1-based number.
func fixtureLineOffsets(t *testing.T, path string) map[int]int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[int]int64{}
	r := bufio.NewReader(f)
	var pos int64
	for line := 1; ; line++ {
		text, err := r.ReadString('\n')
		if text != "" {
			out[line] = pos
			pos += int64(len(text))
		}
		if err != nil {
			return out
		}
	}
}

// unnumberedMatches builds matches for the given lines of a fixture the
// way a scan cut short by a cap leaves them: offset known, line -1.
// Each match points into the middle of its line, as a match usually
// does.
func unnumberedMatches(offsets map[int]int64, lines []int) []rxtypes.Match {
	out := make([]rxtypes.Match, 0, len(lines))
	for _, line := range lines {
		out = append(out, rxtypes.Match{
			File:               "f1",
			Offset:             offsets[line] + 5,
			AbsoluteLineNumber: -1,
		})
	}
	return out
}

// saveShiftedIndex builds and stores a valid index for path, then moves
// every checkpoint's line number up by shift. The index still describes
// the file as far as its identity goes, so a resolver that reads it
// answers with numbers that are wrong by exactly shift.
func saveShiftedIndex(t *testing.T, path string, shift int64) {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	for i := range idx.LineIndex {
		idx.LineIndex[i].LineNumber += shift
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("save index: %v", err)
	}
}

// cacheFiles lists every regular file under dir.
func cacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cache dir: %v", err)
	}
	return files
}

func TestLineResolver_NoIndexCountsLinesAndNeverReadsTheIndex(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	path, _ := writeChunkedFixture(t, "resolve.log", 256<<10)
	offsets := fixtureLineOffsets(t, path)
	saveShiftedIndex(t, path, 1000)
	before := cacheFiles(t, cacheDir)

	lines := []int{1, 2, 437, 900, len(offsets)}
	matches := unnumberedMatches(offsets, lines)
	resolveUnknownLineNumbers(map[string]sandbox.Pinned{"f1": pinForTest(t, path)}, matches, nil,
		lineResolverFor(Options{NoIndex: true}))

	for i, m := range matches {
		if m.AbsoluteLineNumber != lines[i] {
			t.Errorf("offset %d: absolute_line_number = %d, want %d",
				m.Offset, m.AbsoluteLineNumber, lines[i])
		}
		if m.RelativeLineNumber == nil || *m.RelativeLineNumber != lines[i] {
			t.Errorf("offset %d: relative_line_number = %v, want %d",
				m.Offset, m.RelativeLineNumber, lines[i])
		}
	}
	if after := cacheFiles(t, cacheDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("cache directory changed: before %v, after %v", before, after)
	}
}

func TestLineResolver_DefaultUsesTheIndex(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, _ := writeChunkedFixture(t, "resolve.log", 256<<10)
	offsets := fixtureLineOffsets(t, path)
	saveShiftedIndex(t, path, 1000)

	matches := unnumberedMatches(offsets, []int{900})
	resolveUnknownLineNumbers(map[string]sandbox.Pinned{"f1": pinForTest(t, path)}, matches, nil,
		lineResolverFor(Options{}))

	// The shifted checkpoints show the number came from the index.
	if got := matches[0].AbsoluteLineNumber; got != 1900 {
		t.Errorf("absolute_line_number = %d, want 1900 from the shifted index", got)
	}
}

func TestLineResolver_DefaultWithoutAnIndexLeavesTheLineUnknown(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, _ := writeChunkedFixture(t, "resolve.log", 256<<10)
	offsets := fixtureLineOffsets(t, path)

	matches := unnumberedMatches(offsets, []int{900})
	resolveUnknownLineNumbers(map[string]sandbox.Pinned{"f1": pinForTest(t, path)}, matches, nil,
		lineResolverFor(Options{}))

	if got := matches[0].AbsoluteLineNumber; got != -1 {
		t.Errorf("absolute_line_number = %d, want -1 without an index", got)
	}
}

func TestLineResolver_CountingNumbersContextLines(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, _ := writeChunkedFixture(t, "resolve.log", 256<<10)
	offsets := fixtureLineOffsets(t, path)

	contexts := []contextWithFile{{
		fileID: "f1",
		ctx:    rxtypes.ContextLine{AbsoluteLineNumber: -1, RelativeLineNumber: 7, AbsoluteOffset: offsets[512]},
	}}
	resolveUnknownLineNumbers(map[string]sandbox.Pinned{"f1": pinForTest(t, path)}, nil, contexts,
		lineResolverFor(Options{NoIndex: true}))

	if got := contexts[0].ctx; got.AbsoluteLineNumber != 512 || got.RelativeLineNumber != 512 {
		t.Errorf("context line numbered %d/%d, want 512", got.RelativeLineNumber, got.AbsoluteLineNumber)
	}
}

// Counting from the start of the file reads up to the line holding the
// highest offset asked about, plus at most one read buffer, and no
// further.
func TestLineResolver_CountingStopsAtTheHighestOffset(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, total := writeChunkedFixture(t, "resolve.log", 4<<20)
	offsets := fixtureLineOffsets(t, path)
	var read *atomic.Int64
	original := openForLineCount
	openForLineCount = func(src sandbox.Pinned) (io.ReadSeekCloser, error) {
		f, counter := counting.OpenCounting(t, src.Path())
		read = counter
		return f, nil
	}
	t.Cleanup(func() { openForLineCount = original })

	target := total / 4
	got := resolveLinesByCounting(pinForTest(t, path), []int64{offsets[target] + 5})

	if got[offsets[target]+5] != target {
		t.Fatalf("resolved %v, want line %d", got, target)
	}
	budget := offsets[target+1] + lineCountBufferBytes
	if read == nil || read.Load() > budget {
		t.Errorf("read %v bytes, budget %d", read, budget)
	}
}

// A capped trace of a chunked file under --no-index numbers every match
// it returns, from the file and not from an index: the answer matches
// the line each match's own text names, and no index file is written.
func TestTrace_NoIndexCappedTraceNumbersEveryMatchWithoutAnIndex(t *testing.T) {
	requireRipgrep(t)
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	path, _ := writeChunkedFixture(t, "capped.log", 8<<20)

	limit := 40
	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"},
		Options{MaxResults: &limit, NoCache: true, NoIndex: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.Matches) == 0 {
		t.Fatal("no matches")
	}
	for _, m := range resp.Matches {
		want := lineNumberFromText(t, *m.LineText)
		if m.AbsoluteLineNumber != want {
			t.Errorf("offset %d: absolute_line_number = %d, want %d", m.Offset, m.AbsoluteLineNumber, want)
		}
	}
	if files := cacheFiles(t, cacheDir); len(files) != 0 {
		raw, _ := json.Marshal(files)
		t.Errorf("--no-index wrote to the cache directory: %s", raw)
	}
}

// truncateStoredIndex builds and stores a valid index for path, then
// cuts the stored file short, as a power loss during a write would.
func truncateStoredIndex(t *testing.T, path string) {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	cachePath, err := index.Save(idx)
	if err != nil {
		t.Fatalf("save index: %v", err)
	}
	if err := os.Truncate(cachePath, 300); err != nil {
		t.Fatalf("truncate index: %v", err)
	}
}

// A damaged index is no index: a line the scan left unknown stays
// unknown rather than failing, and the operator hears about the file.
func TestLineResolver_DamagedIndexLeavesTheLineUnknown(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, _ := writeChunkedFixture(t, "resolve.log", 256<<10)
	offsets := fixtureLineOffsets(t, path)
	truncateStoredIndex(t, path)
	log := captureDefaultLog(t)

	matches := unnumberedMatches(offsets, []int{900})
	resolveUnknownLineNumbers(map[string]sandbox.Pinned{"f1": pinForTest(t, path)}, matches, nil,
		lineResolverFor(Options{}))

	if got := matches[0].AbsoluteLineNumber; got != -1 {
		t.Errorf("absolute_line_number = %d, want -1 with a damaged index", got)
	}
	if !strings.Contains(log.String(), "index_unreadable") {
		t.Errorf("no warning about the damaged index; log:\n%s", log.String())
	}
}

// A capped trace of a file whose index is damaged answers as one
// without an index: every match is numbered rightly or left at -1, and
// the search does not fail.
func TestTrace_CappedTraceWithADamagedIndexAnswersWithoutIt(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	path, _ := writeChunkedFixture(t, "capped.log", 8<<20)
	truncateStoredIndex(t, path)

	limit := 40
	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"},
		Options{MaxResults: &limit, NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.Matches) == 0 {
		t.Fatal("no matches")
	}
	for _, m := range resp.Matches {
		if want := lineNumberFromText(t, *m.LineText); m.AbsoluteLineNumber != -1 && m.AbsoluteLineNumber != want {
			t.Errorf("offset %d: absolute_line_number = %d, want %d or -1", m.Offset, m.AbsoluteLineNumber, want)
		}
	}
}
