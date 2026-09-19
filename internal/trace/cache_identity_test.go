package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// largeFileCacheEnv makes a few-megabyte fixture count as large, so a
// completed scan of it writes a trace cache, and splits it into several
// chunks. The cache goes to a directory of its own.
func largeFileCacheEnv(t *testing.T) {
	t.Helper()
	requireRipgrep(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	t.Setenv("RX_CACHE_DIR", t.TempDir())
}

// traceOnce runs one trace of path for patterns and fails the test on
// an error.
func traceOnce(t *testing.T, path string, patterns []string, opts Options) *rxtypes.TraceResponse {
	t.Helper()
	resp, err := New().RunWithOptions(context.Background(), []string{path}, patterns, opts)
	if err != nil {
		t.Fatalf("trace %v: %v", patterns, err)
	}
	return resp
}

// appendLine adds one line to the end of the file at path.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close after append: %v", err)
	}
}

// A log that grows while it is traced: the scan covers the bytes that
// were there when its chunks were planned, so the cache it leaves must
// not claim to describe the longer file. The next trace has to rescan
// and find the line that arrived during the first one.
func TestCacheOfAFileThatGrewDuringTheScanIsNotTrusted(t *testing.T) {
	largeFileCacheEnv(t)
	path, _ := writeChunkedFixture(t, "growing.log", 2<<20)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	plannedSize := info.Size()
	// NEEDLE matches throughout the fixture, so the first scan has
	// matches to cache; APPENDED matches only the line that arrives
	// while it runs.
	patterns := []string{"NEEDLE", "APPENDED"}

	first := traceOnce(t, path, patterns, Options{
		afterScan: func(scanned string) { appendLine(t, scanned, "line 999999 APPENDED") },
	})
	if countLinesContaining(first, "APPENDED") != 0 {
		t.Fatal("the scan planned before the append reported the appended line")
	}

	if data, err := LoadCache(CachePath(path, patterns, nil)); err == nil &&
		data.SourceSizeBytes != plannedSize {
		t.Fatalf("cache stamped with size %d; the scan covered %d bytes",
			data.SourceSizeBytes, plannedSize)
	}

	second := traceOnce(t, path, patterns, Options{})
	if got := countLinesContaining(second, "APPENDED"); got != 1 {
		t.Fatalf("trace after the append reported the appended line %d times, want 1", got)
	}
	if len(second.Matches) != len(first.Matches)+1 {
		t.Fatalf("trace after the append found %d matches, want %d",
			len(second.Matches), len(first.Matches)+1)
	}
}

// countLinesContaining counts the matches whose line text holds s.
func countLinesContaining(resp *rxtypes.TraceResponse, s string) int {
	n := 0
	for _, m := range resp.Matches {
		if m.LineText != nil && strings.Contains(*m.LineText, s) {
			n++
		}
	}
	return n
}

// A file replaced by another one with the same size and mtime is a
// different file. The edit sits in the middle, outside the windows the
// fingerprint reads, so only the inode and ctime can tell.
func TestCacheOfAReplacedFileIsNotTrusted(t *testing.T) {
	largeFileCacheEnv(t)
	path, _ := writeChunkedFixture(t, "replaced.log", 2<<20)
	patterns := []string{"NEEDLE"}

	original := traceOnce(t, path, patterns, Options{})
	if _, err := os.Stat(CachePath(path, patterns, nil)); err != nil {
		t.Fatalf("first trace wrote no cache: %v", err)
	}

	content, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	middle := len(content) / 2
	at := middle + strings.Index(string(content[middle:]), "NEEDLE")
	copy(content[at:], "NEEDLX")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	replacement := filepath.Join(filepath.Dir(path), "replacement.log")
	if err := os.WriteFile(replacement, content, 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Chtimes(replacement, time.Now(), info.ModTime()); err != nil {
		t.Fatalf("set replacement mtime: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replace fixture: %v", err)
	}

	after := traceOnce(t, path, patterns, Options{})
	if len(after.Matches) != len(original.Matches)-1 {
		t.Fatalf("trace of the replaced file found %d matches, want %d",
			len(after.Matches), len(original.Matches)-1)
	}
}

// A cache written before the identity fields existed was stamped after
// the scan, so it may describe a longer file than it covers. Such a
// cache is treated as absent even when its size and mtime still match.
func TestCacheWrittenWithoutTheSourceIdentityIsIgnored(t *testing.T) {
	largeFileCacheEnv(t)
	path, _ := writeChunkedFixture(t, "old-format.log", 2<<20)
	patterns := []string{"NEEDLE"}
	fresh := traceOnce(t, path, patterns, Options{})

	cachePath := CachePath(path, patterns, nil)
	raw, err := os.ReadFile(cachePath) //nolint:gosec // path built by CachePath
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse cache: %v", err)
	}
	// The shape of the format before the identity fields, with no
	// matches, so an answer read from it is visibly wrong.
	doc["version"] = 3
	doc["matches"] = []any{}
	for _, key := range []string{"source_inode", "source_changed_at", "source_fingerprint"} {
		delete(doc, key)
	}
	old, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(cachePath, old, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	again := traceOnce(t, path, patterns, Options{})
	if len(again.Matches) != len(fresh.Matches) {
		t.Fatalf("trace over the old-format cache found %d matches, want %d",
			len(again.Matches), len(fresh.Matches))
	}
}

// writeSeekableLog encodes about 4 MB of log lines as a seekable-zstd
// file of several frames, every hundredth line holding NEEDLE. Random
// hex compresses poorly, so the file passes the 1 MB size at which a
// compressed file's scan is cached.
func writeSeekableLog(t *testing.T) string {
	t.Helper()
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic fixture, not crypto
	var b strings.Builder
	for line := 1; b.Len() < 4<<20; line++ {
		kind := "filler"
		if line%100 == 0 {
			kind = "NEEDLE"
		}
		fmt.Fprintf(&b, "line %d %s %016x%016x%016x%016x\n",
			line, kind, rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64())
	}
	return writeSeekableZstdFile(t, []byte(b.String()), 512<<10)
}

// A completed scan of a large seekable-zstd file is cached, stamped
// with the file's identity, and the next trace is answered from it.
func TestSeekableScanIsCached(t *testing.T) {
	largeFileCacheEnv(t)
	path := writeSeekableLog(t)
	patterns := []string{"NEEDLE"}

	fresh := traceOnce(t, path, patterns, Options{})
	data, err := LoadCache(CachePath(path, patterns, nil))
	if err != nil {
		t.Fatalf("the scan wrote no cache: %v", err)
	}
	if data.CompressionFormat != "zstd-seekable" || len(data.Matches) != len(fresh.Matches) {
		t.Fatalf("cache holds format %q and %d matches; the scan found %d",
			data.CompressionFormat, len(data.Matches), len(fresh.Matches))
	}
	if !IsCacheValid(CachePath(path, patterns, nil), path, patterns, nil) {
		t.Fatal("the cache does not validate against the file it was written for")
	}
}

// A trace-cache entry written under one local time zone is a hit under
// another: a serve started with TZ=UTC and a CLI in the user's zone
// share one cache. See index.useLocalZone for why the test sets
// time.Local instead of TZ.
func TestCacheEntryStaysValidWhenTheLocalTimeZoneChanges(t *testing.T) {
	useLocalZone(t, "UTC")
	path, cachePath, _ := cacheFixture(t, []string{"NEEDLE"})

	for _, zone := range []string{"Asia/Tokyo", "America/New_York"} {
		useLocalZone(t, zone)
		if !IsCacheValid(cachePath, path, []string{"NEEDLE"}, nil) {
			t.Errorf("TZ=%s: the entry written under TZ=UTC is not valid", zone)
		}
	}
}

// useLocalZone makes name the process's local time zone until the test
// ends, by assigning time.Local: Go reads TZ only once per process.
func useLocalZone(t *testing.T, name string) {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone %s is not available: %v", name, err)
	}
	saved := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = saved })
}
