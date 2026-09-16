package trace

import (
	"bytes"
	"os"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// traceFromCache runs a trace that must be answered from the trace
// cache, and fails the test when the file was scanned instead.
func traceFromCache(t *testing.T, path string, patterns []string) *rxtypes.TraceResponse {
	t.Helper()
	scanned := false
	resp := traceOnce(t, path, patterns, Options{afterScan: func(string) { scanned = true }})
	if scanned {
		t.Fatal("the trace scanned the file instead of reading the cache")
	}
	return resp
}

// A cache hit reports the chunk count of the scan that wrote the cache:
// the cache may make an answer faster, never different.
func TestCacheHitReportsTheChunkCountOfTheScan(t *testing.T) {
	largeFileCacheEnv(t)
	path, _ := writeChunkedFixture(t, "chunks.log", 4<<20)
	patterns := []string{"NEEDLE"}

	fresh := traceOnce(t, path, patterns, Options{})
	if fresh.FileChunks["f1"] < 2 {
		t.Fatalf("fixture scanned in %d chunks; the test needs several", fresh.FileChunks["f1"])
	}
	cached := traceFromCache(t, path, patterns)

	traceanswer.RequireSame(t, "cache hit", cached, fresh)
}

// The same holds for a seekable-zstd file, whose chunks are its frames.
func TestSeekableCacheHitReportsTheFrameCountOfTheScan(t *testing.T) {
	largeFileCacheEnv(t)
	path := writeSeekableLog(t)
	patterns := []string{"NEEDLE"}

	fresh := traceOnce(t, path, patterns, Options{})
	if fresh.FileChunks["f1"] < 2 {
		t.Fatalf("fixture has %d frames; the test needs several", fresh.FileChunks["f1"])
	}
	// A scan rewrites the cache with a new created_at; a cache hit
	// leaves the file as it was.
	cachePath := CachePath(path, patterns, nil)
	written, err := os.ReadFile(cachePath) //nolint:gosec // path built by CachePath
	if err != nil {
		t.Fatalf("the scan wrote no cache: %v", err)
	}
	cached := traceOnce(t, path, patterns, Options{})
	if after, _ := os.ReadFile(cachePath); !bytes.Equal(after, written) { //nolint:gosec // path built by CachePath
		t.Fatal("the second trace rewrote the cache instead of reading it")
	}

	traceanswer.RequireSame(t, "cache hit", cached, fresh)
}
