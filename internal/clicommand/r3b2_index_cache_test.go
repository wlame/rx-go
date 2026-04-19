package clicommand

// Test for R3-B2: rx index without --force must honor the cache and NOT
// rebuild on every call. Python's rx-python/src/rx/indexer.py::_index_single_file
// calls load_index() first; only rebuilds when the cache is missing or stale.
//
// Contract:
//   1. First invocation:  cache populated. JSON result reports indexed file.
//   2. Second invocation without --force: cache file's mtime and inode MUST
//      be unchanged (cache was reused, not rewritten).
//   3. Third invocation with --force: cache WAS rewritten (mtime newer).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
)

// setCacheDir redirects the cache to a test-scoped temp dir via RX_CACHE_DIR.
// Internal/config consults XDG paths otherwise, which would pollute the real
// user cache during tests.
func setCacheDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", dir)
}

// writeIndexableFile writes a file larger than the config threshold so
// `rx index` will actually emit it (below-threshold files go to skipped).
func writeIndexableFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	// Default index threshold is 50 MB per config.LargeFileMB. For the
	// test we use --threshold=1 so a small file qualifies.
	if err := os.WriteFile(p, []byte(strings.Repeat("hello world\n", 120000)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestIndex_ReusesCacheWhenNotForced is the R3-B2 regression test.
func TestIndex_ReusesCacheWhenNotForced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode check is POSIX-only")
	}

	cacheDir := t.TempDir()
	setCacheDir(t, cacheDir)

	srcDir := t.TempDir()
	src := writeIndexableFile(t, srcDir, "data.log")

	// 1. First run — populate cache.
	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{src, "--json", "--threshold=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first index: %v\n%s", err, buf.String())
	}

	cachePath := index.GetCachePath(src)
	firstStat, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("cache file missing after first build: %v", err)
	}
	firstInode := statInode(t, firstStat)
	firstMtime := firstStat.ModTime()

	// Give the filesystem mtime resolution a moment so a re-write would be
	// observably different. On most filesystems mtime has ~1 second
	// resolution; 50ms is plenty to distinguish the cases but cheap enough
	// that the test stays fast.
	time.Sleep(50 * time.Millisecond)

	// 2. Second run — MUST reuse the cache.
	buf.Reset()
	cmd = NewIndexCommand(&buf)
	cmd.SetArgs([]string{src, "--json", "--threshold=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("second index: %v\n%s", err, buf.String())
	}

	secondStat, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("cache file missing after second build: %v", err)
	}
	secondInode := statInode(t, secondStat)
	secondMtime := secondStat.ModTime()

	if secondInode != firstInode {
		t.Errorf("cache rebuilt (inode changed %d → %d) — --force=false should reuse",
			firstInode, secondInode)
	}
	if !secondMtime.Equal(firstMtime) {
		t.Errorf("cache rebuilt (mtime changed %s → %s) — --force=false should reuse",
			firstMtime, secondMtime)
	}

	// Response envelope sanity — second call must still report the file
	// as indexed (not skipped, not errored).
	var resp map[string]any
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("second json parse: %v\n%s", err, buf.String())
	}
	indexed := resp["indexed"].([]any)
	if len(indexed) != 1 {
		t.Fatalf("indexed: got %d, want 1 (response=%+v)", len(indexed), resp)
	}
	entry := indexed[0].(map[string]any)
	if entry["path"] != src {
		t.Errorf("indexed path: got %v, want %v", entry["path"], src)
	}
}

// TestIndex_RebuildsWhenForced verifies the force-path still works.
// After a valid cache exists, --force must actually rewrite the file.
func TestIndex_RebuildsWhenForced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode check is POSIX-only")
	}

	cacheDir := t.TempDir()
	setCacheDir(t, cacheDir)

	srcDir := t.TempDir()
	src := writeIndexableFile(t, srcDir, "data.log")

	// Build once.
	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{src, "--json", "--threshold=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first index: %v", err)
	}

	cachePath := index.GetCachePath(src)
	firstStat, _ := os.Stat(cachePath)
	firstMtime := firstStat.ModTime()

	time.Sleep(50 * time.Millisecond)

	// Build again with --force.
	buf.Reset()
	cmd = NewIndexCommand(&buf)
	cmd.SetArgs([]string{src, "--json", "--threshold=1", "--force"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("forced index: %v", err)
	}

	secondStat, _ := os.Stat(cachePath)
	secondMtime := secondStat.ModTime()

	if !secondMtime.After(firstMtime) {
		t.Errorf("--force did not rewrite cache: mtime %s still %s", firstMtime, secondMtime)
	}
}

// TestIndex_RebuildsWhenCacheStale verifies the stale-cache path: if the
// source file changes, the next non-forced call must rebuild automatically.
func TestIndex_RebuildsWhenCacheStale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode check is POSIX-only")
	}

	cacheDir := t.TempDir()
	setCacheDir(t, cacheDir)

	srcDir := t.TempDir()
	src := writeIndexableFile(t, srcDir, "data.log")

	// First build.
	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{src, "--json", "--threshold=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	cachePath := index.GetCachePath(src)
	firstStat, _ := os.Stat(cachePath)
	firstMtime := firstStat.ModTime()

	// Mutate the source (add bytes → size changes → cache invalid).
	time.Sleep(50 * time.Millisecond)
	f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(strings.Repeat("extra\n", 1000))
	_ = f.Close()

	// Second build — cache should be detected stale and rebuilt.
	buf.Reset()
	cmd = NewIndexCommand(&buf)
	cmd.SetArgs([]string{src, "--json", "--threshold=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	secondStat, _ := os.Stat(cachePath)
	secondMtime := secondStat.ModTime()
	if !secondMtime.After(firstMtime) {
		t.Errorf("stale cache not rebuilt: mtime %s should be > %s",
			secondMtime, firstMtime)
	}
}

// statInode extracts the POSIX inode from a FileInfo.
func statInode(t *testing.T, info os.FileInfo) uint64 {
	t.Helper()
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("info.Sys() is %T, not *syscall.Stat_t", info.Sys())
	}
	return sys.Ino
}
