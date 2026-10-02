package trace

import (
	"bytes"
	"encoding/json"
	"maps"
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

// requireSameChunkCount fails the test unless the cache hit reports the
// chunk count of the scan that wrote the entry. The accelerator rule
// does not compare file_chunks, so this is checked on its own.
func requireSameChunkCount(t *testing.T, cached, scan *rxtypes.TraceResponse) {
	t.Helper()
	if !maps.Equal(cached.FileChunks, scan.FileChunks) {
		t.Fatalf("cache hit file_chunks = %v, want %v from the scan that wrote the entry", cached.FileChunks, scan.FileChunks)
	}
}

// A cache hit reports the chunk count of the scan that wrote the cache,
// and every other field of its answer equals the scan's.
func TestCacheHitReportsTheChunkCountOfTheScan(t *testing.T) {
	largeFileCacheEnv(t)
	path, _ := writeChunkedFixture(t, "chunks.log", 4<<20)
	patterns := []string{"NEEDLE"}

	fresh := traceOnce(t, path, patterns, Options{})
	if fresh.FileChunks["f1"] < 2 {
		t.Fatalf("fixture scanned in %d chunks; the test needs several", fresh.FileChunks["f1"])
	}
	cached := traceFromCache(t, path, patterns)

	requireSameChunkCount(t, cached, fresh)
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

	requireSameChunkCount(t, cached, fresh)
	traceanswer.RequireSame(t, "cache hit", cached, fresh)
}

// The chunk settings are not part of the cache key, so a hit after they
// change reports the chunk count of the scan that wrote the entry while
// a fresh scan reports the new one. file_chunks says how an answer was
// produced, not what it is, so the accelerator rule leaves it out: the
// hit and the fresh scan still agree. Any other field that differs still
// breaks the rule.
func TestCacheHitAgreesWithAScanUnderOtherChunkSettings(t *testing.T) {
	largeFileCacheEnv(t)
	path, _ := writeChunkedFixture(t, "chunks.log", 4<<20)
	patterns := []string{"NEEDLE"}

	// A minimum chunk size above the file's size makes the writing scan
	// a single chunk.
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "20")
	written := traceOnce(t, path, patterns, Options{})
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	cached := traceFromCache(t, path, patterns)
	fresh := traceOnce(t, path, patterns, Options{NoCache: true})

	if cached.FileChunks["f1"] == fresh.FileChunks["f1"] {
		t.Fatalf("hit and fresh scan both report %d chunks; the test needs the settings to change the count", fresh.FileChunks["f1"])
	}
	requireSameChunkCount(t, cached, written)
	traceanswer.RequireSame(t, "cache hit under other chunk settings", cached, fresh)

	for name, change := range map[string]func(doc map[string]any){
		"a match's line_text": func(doc map[string]any) {
			doc["matches"].([]any)[0].(map[string]any)["line_text"] = "something else"
		},
		"a match's offset": func(doc map[string]any) {
			doc["matches"].([]any)[0].(map[string]any)["offset"] = float64(1)
		},
		"skipped_files": func(doc map[string]any) { doc["skipped_files"] = []any{"/elsewhere.log"} },
		"files":         func(doc map[string]any) { doc["files"] = map[string]any{"f1": "/elsewhere.log"} },
		"max_results":   func(doc map[string]any) { doc["max_results"] = float64(5) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := answerDocument(t, cached)
			change(changed)
			if diff := traceanswer.Difference(changed, fresh, nil); diff == "" {
				t.Errorf("a cache hit with a different %s agrees with the fresh scan", name)
			}
		})
	}
}

// answerDocument returns resp as a parsed JSON document that a test may
// change.
func answerDocument(t *testing.T, resp *rxtypes.TraceResponse) map[string]any {
	t.Helper()
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}
