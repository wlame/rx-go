package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/traceanswer"
)

// writeMultiChunkLog writes a log of about 4 MB in which every tenth
// line holds NEEDLE, and returns its path.
func writeMultiChunkLog(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	pad := strings.Repeat("x", 200)
	for line := 1; b.Len() < 4<<20; line++ {
		kind := "filler"
		if line%10 == 0 {
			kind = "NEEDLE"
		}
		fmt.Fprintf(&b, "line %d %s %s\n", line, kind, pad)
	}
	path := filepath.Join(t.TempDir(), "chunks.log")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// traceCacheFiles returns the content of every trace cache file under
// cacheDir, keyed by path. A trace that scans a file rewrites its cache
// with a new created_at, and a trace answered from the cache leaves it
// alone, so an unchanged snapshot shows a cache hit.
func traceCacheFiles(t *testing.T, cacheDir string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(cacheDir, "rx", "trace_cache", "*", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no trace cache file under %s (%v)", cacheDir, err)
	}
	contents := make(map[string]string, len(files))
	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // path found under the test's cache dir
		if err != nil {
			t.Fatalf("read cache: %v", err)
		}
		contents[f] = string(body)
	}
	return contents
}

// `rx trace --json` answered from the trace cache answers as the scan
// that wrote the cache did, its chunk count included.
func TestTraceCacheHitReportsTheChunkCountOfTheScan(t *testing.T) {
	path := writeMultiChunkLog(t)
	cacheDir := t.TempDir()
	env := []string{
		"RX_LARGE_FILE_MB=1", "RX_MIN_CHUNK_SIZE_MB=1", "RX_MAX_SUBPROCESSES=4",
		"RX_CACHE_DIR=" + cacheDir,
	}

	fresh := traceAnswerJSON(t, env, "NEEDLE", path)
	written := traceCacheFiles(t, cacheDir)
	cached := traceAnswerJSON(t, env, "NEEDLE", path)

	if !maps.Equal(traceCacheFiles(t, cacheDir), written) {
		t.Fatal("the second trace scanned the file instead of reading the cache")
	}
	if chunks, _ := fresh["file_chunks"].(map[string]any)["f1"].(float64); chunks < 2 {
		t.Fatalf("fixture scanned in %v chunks; the test needs several", chunks)
	}
	traceanswer.RequireSame(t, "cache hit", cached, fresh)
}
