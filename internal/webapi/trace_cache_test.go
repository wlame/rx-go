package webapi

import (
	"bytes"
	"fmt"
	"maps"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/internal/trace"
)

// GET /v1/trace answered from the trace cache answers as the scan that
// wrote the cache did, its chunk count included.
func TestTrace_CacheHitReportsTheChunkCountOfTheScan(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")

	var b strings.Builder
	for line := 1; b.Len() < 4<<20; line++ {
		kind := "filler"
		if line%10 == 0 {
			kind = "NEEDLE"
		}
		fmt.Fprintf(&b, "line %d %s %s\n", line, kind, strings.Repeat("x", 200))
	}
	file := sandboxedFile(t, b.String())
	ts := newServerWithRipgrep(t)
	query := url.Values{"path": {file}, "regexp": {"NEEDLE"}}

	fresh := getTrace(t, ts.URL, query)
	cachePath := trace.CachePath(file, []string{"NEEDLE"}, nil)
	written, err := os.ReadFile(cachePath) //nolint:gosec // path built by CachePath
	if err != nil {
		t.Fatalf("the first trace wrote no cache: %v", err)
	}
	cached := getTrace(t, ts.URL, query)

	// A scan rewrites the cache with a new created_at; a hit leaves it.
	if after, _ := os.ReadFile(cachePath); !bytes.Equal(after, written) { //nolint:gosec // path built by CachePath
		t.Fatal("the second trace scanned the file instead of reading the cache")
	}
	if fresh.FileChunks["f1"] < 2 {
		t.Fatalf("fixture scanned in %d chunks; the test needs several", fresh.FileChunks["f1"])
	}
	// The accelerator rule leaves file_chunks out, so it is checked here.
	if !maps.Equal(cached.FileChunks, fresh.FileChunks) {
		t.Fatalf("cache hit file_chunks = %v, want %v from the scan", cached.FileChunks, fresh.FileChunks)
	}
	traceanswer.RequireSame(t, "cache hit", cached, fresh)
}
