package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
)

// writeSplitFrameCopies writes a log of about 3.2 MB as a plain file and
// as a seekable .zst whose frames are cut every 64 KB wherever that
// falls, the way an encoder without line alignment cuts them. Line 5000
// is 200 KB long, so it spans frames that hold no line break. Every
// 50th line, and line 5000, holds NEEDLE. The random text keeps the
// .zst above the 1 MB trace-cache threshold the tests set.
func writeSplitFrameCopies(t *testing.T) (text []byte, plain, zst string) {
	t.Helper()
	rng := rand.New(rand.NewPCG(84, 1))
	var b bytes.Buffer
	for n := 1; n <= 40000; n++ {
		word := "filler"
		if n%50 == 0 || n == 5000 {
			word = "NEEDLE"
		}
		fmt.Fprintf(&b, "LINE %d %s %016x%016x%016x", n, word, rng.Uint64(), rng.Uint64(), rng.Uint64())
		if n == 5000 {
			b.WriteString(strings.Repeat("y", 200<<10))
		}
		b.WriteByte('\n')
	}
	text = b.Bytes()
	dir := t.TempDir()
	plain = filepath.Join(dir, "split.log")
	if err := os.WriteFile(plain, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	zst = plain + ".zst"
	seekablefile.Write(t, zst, seekablefile.SplitEvery(text, 64<<10))
	return text, plain, zst
}

// samplesAnswer is the part of `rx samples --json` that must not depend
// on how the file is stored or whether it has an index.
type samplesAnswer struct {
	Lines   map[string]int64    `json:"lines"`
	Samples map[string][]string `json:"samples"`
}

func samplesJSON(t *testing.T, env []string, args ...string) samplesAnswer {
	t.Helper()
	code, stdout, stderr := runRxEnv(t, env, append([]string{"samples", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("rx samples %v exited %d: %s", args, code, stderr)
	}
	var ans samplesAnswer
	if err := json.Unmarshal([]byte(stdout), &ans); err != nil {
		t.Fatalf("decode samples JSON: %v\n%s", err, stdout)
	}
	return ans
}

// A seekable .zst whose frames split lines answers `rx samples --lines`
// as the plain file does, with the cache empty, without an index, and
// after `rx index` built one.
func TestSamplesLinesOnASeekableFileWithSplitFramesEqualThePlainFile(t *testing.T) {
	_, plain, zst := writeSplitFrameCopies(t)
	env := []string{"RX_LARGE_FILE_MB=1", "RX_CACHE_DIR=" + t.TempDir()}
	lines := "--lines=4999,5000,5001,5002,12345,29999,-1"

	want := samplesJSON(t, env, plain, lines, "--context=2", "--no-index")
	runs := map[string][]string{
		"no index":    {zst, lines, "--context=2", "--no-index"},
		"cache empty": {zst, lines, "--context=2"},
	}
	for name, args := range runs {
		if got := samplesJSON(t, env, args...); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: lines %v, want %v", name, got.Lines, want.Lines)
		}
	}
	if code, _, stderr := runRxEnv(t, env, "index", zst); code != 0 {
		t.Fatalf("rx index exited %d: %s", code, stderr)
	}
	if got := samplesJSON(t, env, zst, lines, "--context=2"); !reflect.DeepEqual(got, want) {
		t.Errorf("indexed: lines %v, want %v", got.Lines, want.Lines)
	}
}

// A full `rx trace` of such a file numbers every match with the line
// that holds its offset, and the trace cache gives the same answer back
// in every field.
func TestTraceOfASeekableFileWithSplitFramesNumbersEveryMatch(t *testing.T) {
	text, _, zst := writeSplitFrameCopies(t)
	cacheDir := t.TempDir()
	env := []string{"RX_LARGE_FILE_MB=1", "RX_CACHE_DIR=" + cacheDir}

	fresh := traceAnswerJSON(t, env, "NEEDLE", zst)
	written := traceCacheFiles(t, cacheDir)
	cached := traceAnswerJSON(t, env, "NEEDLE", zst)
	if !maps.Equal(traceCacheFiles(t, cacheDir), written) {
		t.Fatal("the second trace scanned the file instead of reading the cache")
	}
	matches, _ := fresh["matches"].([]any)
	if len(matches) < 700 {
		t.Fatalf("got %d matches; the fixture has 801 NEEDLE lines", len(matches))
	}
	for _, item := range matches {
		m := item.(map[string]any)
		offset, line := int64(m["offset"].(float64)), int64(m["absolute_line_number"].(float64))
		if want := int64(bytes.Count(text[:offset], []byte{'\n'})) + 1; line != want {
			t.Errorf("match at byte %d: line %d, want %d", offset, line, want)
		}
	}
	traceanswer.RequireSame(t, "cache hit", cached, fresh)
}

// traceAnswerJSON runs `rx trace --json` and returns its parsed answer.
func traceAnswerJSON(t *testing.T, env []string, args ...string) map[string]any {
	t.Helper()
	code, stdout, stderr := runRxEnv(t, env, append([]string{"trace", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("rx trace %v exited %d: %s", args, code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode trace JSON: %v", err)
	}
	return doc
}

// traceDocument runs `rx trace --json` and returns its answer without
// the fields that name the file: the path, the file table and the chunk
// counts. Answers about two copies of one text can then be compared.
func traceDocument(t *testing.T, env []string, args ...string) map[string]any {
	t.Helper()
	doc := traceAnswerJSON(t, env, args...)
	for _, key := range []string{"path", "files", "file_chunks"} {
		delete(doc, key)
	}
	return doc
}

// A full `rx trace` of a seekable .zst whose frames split lines answers
// as a trace of the plain file does, in every field of every match and
// every context window: scanned with the cache empty, read back from
// the trace cache, and with --no-index. The pattern matches every line,
// so each frame boundary falls inside a matching line, and the end of
// the 200 KB line, which lies frames away from its start.
func TestTraceOfASeekableFileWithSplitFramesEqualsThePlainFile(t *testing.T) {
	text, plain, _ := writeSplitFrameCopies(t)
	// 8 KB frames make about 400 of them, so the scan crosses the
	// boundaries between batches of frames as well as between frames.
	zst := filepath.Join(t.TempDir(), "split8k.log.zst")
	seekablefile.Write(t, zst, seekablefile.SplitEvery(text, 8<<10))
	args := []string{`^LINE \d+ \w+ [0-9a-f]{48}|y{5}$`, "--before=1", "--after=1"}

	want := traceDocument(t, []string{"RX_CACHE_DIR=" + t.TempDir()}, append(args, plain, "--no-cache")...)
	if n := len(want["matches"].([]any)); n != 40000 {
		t.Fatalf("the plain trace found %d matches; the fixture has 40000 lines", n)
	}

	cacheDir := t.TempDir()
	env := []string{"RX_LARGE_FILE_MB=1", "RX_CACHE_DIR=" + cacheDir}
	cold := traceDocument(t, env, append(args, zst)...)
	written := traceCacheFiles(t, cacheDir)
	warm := traceDocument(t, env, append(args, zst)...)
	if !maps.Equal(traceCacheFiles(t, cacheDir), written) {
		t.Fatal("the second trace scanned the file instead of reading the cache")
	}
	noIndex := traceDocument(t, []string{"RX_CACHE_DIR=" + t.TempDir()}, append(args, zst, "--no-index")...)

	for name, got := range map[string]map[string]any{"cache empty": cold, "cache hit": warm, "--no-index": noIndex} {
		traceanswer.RequireSame(t, name+": the trace of the .zst against the trace of the plain file", got, want)
	}
}
