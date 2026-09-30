package trace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Two traces of the same file and patterns can finish at the same time
// and write the same cache entry. Each writer must go through its own
// temporary file, so the entry left behind is one writer's whole
// answer, never bytes of two answers interleaved.
func TestSaveCache_ConcurrentWritersLeaveAValidEntry(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "entry.json")

	// Two answers of different lengths, so interleaved bytes cannot
	// form either of them by accident.
	answers := []*rxtypes.TraceCacheData{
		{Version: TraceCacheVersion, SourcePath: "/logs/a.log", Matches: cacheMatches(10)},
		{Version: TraceCacheVersion, SourcePath: "/logs/a.log", Matches: cacheMatches(3000)},
	}

	const writers, rounds = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds)
	for w := range writers {
		// Each goroutine writes the entry repeatedly; wg.Wait below
		// blocks until all of them have returned.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				if err := SaveCache(cachePath, answers[w%len(answers)]); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("SaveCache: %v", err)
	}

	loaded, err := LoadCache(cachePath)
	if err != nil {
		t.Fatalf("LoadCache after concurrent writes: %v", err)
	}
	if n := len(loaded.Matches); n != len(answers[0].Matches) && n != len(answers[1].Matches) {
		t.Errorf("entry holds %d matches, want one writer's answer (10 or 3000)", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache directory holds %v, want only the entry", names)
	}
}

// cacheMatches returns n distinct cache records.
func cacheMatches(n int) []rxtypes.TraceCacheMatch {
	out := make([]rxtypes.TraceCacheMatch, n)
	for i := range out {
		out[i] = rxtypes.TraceCacheMatch{Offset: int64(i) * 100, LineNumber: int64(i + 1)}
	}
	return out
}

// An entry that cannot be parsed (a write cut short by a crash, a full
// disk) is treated as absent: the trace scans the file, answers what
// the scan finds, logs the unreadable entry, and replaces it.
func TestTrace_CorruptCacheEntryIsTreatedAsAbsent(t *testing.T) {
	path, cachePath, fresh := cacheFixture(t, []string{"NEEDLE"})
	body, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if err := os.WriteFile(cachePath, body[:len(body)/2], 0o600); err != nil {
		t.Fatalf("truncate cache: %v", err)
	}
	log := captureDefaultLog(t)

	resp, err := New().RunWithOptions(context.Background(), []string{path}, []string{"NEEDLE"}, Options{})
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}

	if len(resp.Matches) != len(fresh.Matches) {
		t.Fatalf("matches: got %d, want %d as the first scan", len(resp.Matches), len(fresh.Matches))
	}
	for i := range fresh.Matches {
		if resp.Matches[i].Offset != fresh.Matches[i].Offset ||
			resp.Matches[i].AbsoluteLineNumber != fresh.Matches[i].AbsoluteLineNumber {
			t.Fatalf("match %d: got offset %d line %d, want offset %d line %d", i,
				resp.Matches[i].Offset, resp.Matches[i].AbsoluteLineNumber,
				fresh.Matches[i].Offset, fresh.Matches[i].AbsoluteLineNumber)
		}
	}
	logged := log.String()
	if !strings.Contains(logged, "trace_cache_unreadable") || !strings.Contains(logged, filepath.Base(cachePath)) {
		t.Errorf("no warning naming the unreadable entry; log:\n%s", logged)
	}
	if _, err := LoadCache(cachePath); err != nil {
		t.Errorf("entry not replaced by the scan: %v", err)
	}
}

// A missing entry is an ordinary miss and is not logged.
func TestTrace_MissingCacheEntryIsNotLogged(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, _ := writeChunkedFixture(t, "fresh.log", 2<<20)
	log := captureDefaultLog(t)

	if _, err := New().RunWithOptions(context.Background(), []string{path}, []string{"NEEDLE"}, Options{}); err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if strings.Contains(log.String(), "trace_cache_unreadable") {
		t.Errorf("a missing entry was logged as unreadable:\n%s", log.String())
	}
}
