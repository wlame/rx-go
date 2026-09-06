package trace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// cacheFixture writes a chunked fixture, scans it once so a cache is
// written, and returns the fixture path and the path of its cache file.
func cacheFixture(t *testing.T, patterns []string) (string, string, *rxtypes.TraceResponse) {
	t.Helper()
	requireRipgrep(t)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	// Cache writes are gated on the large-file threshold; 1 MB puts the
	// fixture over it without making the test slow.
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	path, _ := writeChunkedFixture(t, "cached.log", 4<<20)
	fresh, err := New().RunWithOptions(
		context.Background(), []string{path}, patterns, Options{},
	)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	cachePath := CachePath(path, patterns, nil)
	if _, statErr := os.Stat(cachePath); statErr != nil {
		t.Fatalf("no cache written at %s: %v", cachePath, statErr)
	}
	return path, cachePath, fresh
}

// TestCacheHitMatchesFreshScan is the regression test for a cache hit
// that returned a different line's text than the scan that filled it.
func TestCacheHitMatchesFreshScan(t *testing.T) {
	path, _, fresh := cacheFixture(t, []string{"NEEDLE"})

	cached, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"}, Options{},
	)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(cached.Matches) != len(fresh.Matches) {
		t.Fatalf("cache hit returned %d matches, fresh scan %d",
			len(cached.Matches), len(fresh.Matches))
	}
	// A cache hit is served from disk, which the chunk count reports as 0.
	for fileID, chunks := range cached.FileChunks {
		if chunks != 0 {
			t.Fatalf("file %s reports %d chunks; the second scan did not hit the cache",
				fileID, chunks)
		}
	}
	for i := range fresh.Matches {
		f, c := fresh.Matches[i], cached.Matches[i]
		if f.Offset != c.Offset {
			t.Fatalf("match %d: offset %d from cache, %d fresh", i, c.Offset, f.Offset)
		}
		if *f.LineText != *c.LineText {
			t.Fatalf("match %d at offset %d: cache returned %q, fresh scan %q",
				i, c.Offset, *c.LineText, *f.LineText)
		}
		if f.AbsoluteLineNumber != c.AbsoluteLineNumber {
			t.Fatalf("match %d at offset %d: cache says line %d, fresh scan %d",
				i, c.Offset, c.AbsoluteLineNumber, f.AbsoluteLineNumber)
		}
		want := lineNumberFromText(t, *c.LineText)
		if c.AbsoluteLineNumber != want {
			t.Fatalf("match %d: cache line number %d, line text says %d",
				i, c.AbsoluteLineNumber, want)
		}
	}
}

// TestCacheHitIgnoresAStoredLineNumber pins the rule that keeps a cache
// written by an older version from returning the wrong text: the byte
// offset addresses the line, and the line number is counted during the
// pass rather than read from the file.
func TestCacheHitIgnoresAStoredLineNumber(t *testing.T) {
	path, cachePath, fresh := cacheFixture(t, []string{"NEEDLE"})

	raw, err := os.ReadFile(cachePath) //nolint:gosec // path built by CachePath
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var data rxtypes.TraceCacheData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse cache: %v", err)
	}
	// What a chunk-relative line number looks like on disk.
	for i := range data.Matches {
		data.Matches[i].LineNumber = 7
	}
	patched, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(cachePath, patched, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	cached, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"}, Options{},
	)
	if err != nil {
		t.Fatalf("scan with patched cache: %v", err)
	}
	if len(cached.Matches) != len(fresh.Matches) {
		t.Fatalf("patched cache returned %d matches, want %d",
			len(cached.Matches), len(fresh.Matches))
	}
	for i, m := range cached.Matches {
		want := lineNumberFromText(t, *m.LineText)
		if m.AbsoluteLineNumber != want {
			t.Fatalf("match %d: line %d reported for text that says %d",
				i, m.AbsoluteLineNumber, want)
		}
		if *m.LineText != *fresh.Matches[i].LineText {
			t.Fatalf("match %d: patched cache returned %q, want %q",
				i, *m.LineText, *fresh.Matches[i].LineText)
		}
	}
}

// TestCacheFromAnOlderVersionIsDiscarded covers the version gate: a
// cache written before line numbers were file-absolute must be ignored,
// not read back.
func TestCacheFromAnOlderVersionIsDiscarded(t *testing.T) {
	path, cachePath, _ := cacheFixture(t, []string{"NEEDLE"})

	raw, err := os.ReadFile(cachePath) //nolint:gosec // path built by CachePath
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var data rxtypes.TraceCacheData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse cache: %v", err)
	}
	data.Version = TraceCacheVersion - 1
	patched, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(cachePath, patched, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	if _, err := GetCachedMatches(path, []string{"NEEDLE"}, nil); err == nil {
		t.Fatal("a cache from an older version was accepted")
	}

	// The scan that finds it stale rewrites it at the current version.
	if _, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"}, Options{},
	); err != nil {
		t.Fatalf("scan over a stale cache: %v", err)
	}
	rewritten, err := LoadCache(cachePath)
	if err != nil {
		t.Fatalf("load rewritten cache: %v", err)
	}
	if rewritten.Version != TraceCacheVersion {
		t.Fatalf("rewritten cache is version %d, want %d", rewritten.Version, TraceCacheVersion)
	}
}

// TestCacheStoresFileLineNumbers checks what lands on disk, since
// rx-python reads these records by line number.
func TestCacheStoresFileLineNumbers(t *testing.T) {
	path, cachePath, fresh := cacheFixture(t, []string{"NEEDLE"})
	_ = path

	data, err := LoadCache(cachePath)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if len(data.Matches) != len(fresh.Matches) {
		t.Fatalf("cache holds %d matches, scan found %d", len(data.Matches), len(fresh.Matches))
	}
	byOffset := map[int64]int{}
	for _, m := range fresh.Matches {
		byOffset[m.Offset] = m.AbsoluteLineNumber
	}
	for _, cm := range data.Matches {
		want, ok := byOffset[cm.Offset]
		if !ok {
			t.Fatalf("cache holds an offset the scan never reported: %d", cm.Offset)
		}
		if int(cm.LineNumber) != want {
			t.Fatalf("offset %d cached as line %d, scan reported %d", cm.Offset, cm.LineNumber, want)
		}
	}
}

// TestCacheHitRebuildsContextLines covers the context window a cache hit
// has to rebuild from the source.
func TestCacheHitRebuildsContextLines(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path, _ := writeChunkedFixture(t, "ctx.log", 2<<20)

	opts := Options{ContextBefore: 2, ContextAfter: 2}
	fresh, err := New().RunWithOptions(context.Background(), []string{path}, []string{"NEEDLE"}, opts)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	cached, err := New().RunWithOptions(context.Background(), []string{path}, []string{"NEEDLE"}, opts)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(cached.ContextLines) != len(fresh.ContextLines) {
		t.Fatalf("cache hit built %d context windows, fresh scan %d",
			len(cached.ContextLines), len(fresh.ContextLines))
	}
	for key, lines := range cached.ContextLines {
		freshLines, ok := fresh.ContextLines[key]
		if !ok {
			t.Fatalf("cache hit invented context window %s", key)
		}
		if len(lines) != len(freshLines) {
			t.Fatalf("%s: cache hit has %d lines, fresh scan %d", key, len(lines), len(freshLines))
		}
		for i := range lines {
			if lines[i].LineText != freshLines[i].LineText {
				t.Fatalf("%s line %d: cache %q, fresh %q",
					key, i, lines[i].LineText, freshLines[i].LineText)
			}
			if lines[i].AbsoluteLineNumber != freshLines[i].AbsoluteLineNumber {
				t.Fatalf("%s line %d: cache numbers it %d, fresh %d",
					key, i, lines[i].AbsoluteLineNumber, freshLines[i].AbsoluteLineNumber)
			}
		}
	}
}

// TestReconstructFromCacheReadsOnlyWhatItNeeds keeps the bounded-read
// contract on the reconstruction path: with an index in place the pass
// starts near the first match instead of at byte 0.
func TestReconstructFromCacheStartsFromTheIndex(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "indexed.log")

	// 200 lines, the only match at the end.
	var content []byte
	for i := 1; i <= 200; i++ {
		line := "line " + itoa(i) + " filler\n"
		if i == 200 {
			line = "line 200 NEEDLE\n"
		}
		content = append(content, line...)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	matches, _, err := ReconstructFromCache(ReconstructRequest{
		SourcePath: path,
		Cached: []rxtypes.TraceCacheMatch{
			{PatternIndex: 0, Offset: int64(len(content)) - int64(len("line 200 NEEDLE\n")), LineNumber: 1},
		},
		Patterns: []string{"NEEDLE"},
		FileID:   "f1",
	})
	if err != nil {
		t.Fatalf("ReconstructFromCache: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if matches[0].AbsoluteLineNumber != 200 {
		t.Fatalf("line number %d, want 200", matches[0].AbsoluteLineNumber)
	}
	if *matches[0].LineText != "line 200 NEEDLE" {
		t.Fatalf("line text %q", *matches[0].LineText)
	}
	if len(matches[0].Submatches) != 1 || matches[0].Submatches[0].Text != "NEEDLE" {
		t.Fatalf("submatches = %+v", matches[0].Submatches)
	}
}

// itoa keeps the fixture builder above free of an fmt import.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
