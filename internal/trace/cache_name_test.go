package trace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
)

// A trace cache entry's file name is at most 255 bytes, the longest name
// most filesystems accept, however long the source's base name is, and
// it is cut on a character boundary when the base name is UTF-8.
func TestCachePath_FitsInOneFileName(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	cases := []struct {
		name     string
		basename string
	}{
		{"240 bytes", strings.Repeat("a", 240)},
		{"multibyte letters", strings.Repeat("ж", 200)},
		{"bytes that are not UTF-8", strings.Repeat("\xff", 300)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Base(CachePath("/var/log/"+tc.basename, []string{"x"}, nil))
			if len(file) > index.MaxCacheFileNameBytes {
				t.Errorf("entry name is %d bytes, want at most %d", len(file), index.MaxCacheFileNameBytes)
			}
			if !strings.HasSuffix(file, ".json") {
				t.Errorf("entry name %q lost its suffix", file)
			}
		})
	}

	// A name that already fits keeps the entry name it always had.
	short := filepath.Base(CachePath("/var/log/app.log", []string{"x"}, nil))
	if !strings.HasSuffix(short, "_app.log.json") {
		t.Errorf("a base name that fits was changed: %q", short)
	}
}

// A log whose base name is too long to carry whole into an entry name is
// still cached, and the trace logs nothing about its cache.
func TestTrace_LongBaseNameIsCached(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, _ := writeChunkedFixture(t, strings.Repeat("l", 240)+".log", 2<<20)
	log := captureDefaultLog(t)

	if _, err := New().RunWithOptions(context.Background(), []string{path}, []string{"NEEDLE"}, Options{}); err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if _, err := LoadCache(CachePath(path, []string{"NEEDLE"}, nil)); err != nil {
		t.Fatalf("no trace cache entry was stored: %v", err)
	}
	if log.Len() != 0 {
		t.Errorf("the trace logged about its cache:\n%s", log.String())
	}
}

// When RX_CACHE_DIR names a regular file, no entry can be stored or
// read. The trace answers from a scan, and the operator gets one
// warning, about the write that failed; the lookup that found nothing
// is not reported as an unreadable entry.
func TestTrace_CacheDirectoryIsAFileWarnsOnceAboutTheWrite(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv("RX_CACHE_DIR", blocked)
	cacheWriteWarned.Store(false)
	t.Cleanup(func() { cacheWriteWarned.Store(false) })
	path, _ := writeChunkedFixture(t, "big.log", 2<<20)
	log := captureDefaultLog(t)

	for range 2 {
		resp, err := New().RunWithOptions(context.Background(), []string{path}, []string{"NEEDLE"}, Options{})
		if err != nil {
			t.Fatalf("RunWithOptions: %v", err)
		}
		if len(resp.Matches) == 0 {
			t.Fatal("the trace found no matches")
		}
	}
	logged := log.String()
	if got := strings.Count(logged, "trace_cache_write_failed"); got != 1 {
		t.Errorf("write warnings: %d, want 1:\n%s", got, logged)
	}
	if strings.Contains(logged, "trace_cache_unreadable") {
		t.Errorf("a lookup under a regular file was reported as an unreadable entry:\n%s", logged)
	}
}
