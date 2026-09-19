package clicommand

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// `--build-index` defaults to true in both backends. It used to report
// an `index_error` here saying the feature was missing, so a `.zst`
// written by rx-go had no index and every later lookup in it walked the
// stream, while the same file written by rx-python was indexed.

func compressFixture(t *testing.T) (dir, input string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("RX_CACHE_DIR", filepath.Join(dir, "cache"))
	input = filepath.Join(dir, "app.log")

	var body bytes.Buffer
	for n := 1; n <= 20000; n++ {
		fmt.Fprintf(&body, "log line number %d with padding to make frames\n", n)
	}
	if err := os.WriteFile(input, body.Bytes(), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	return dir, input
}

func runCompressJSON(t *testing.T, p compressParams) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	p.jsonOutput = true
	if err := runCompress(context.Background(), &buf, p); err != nil {
		t.Fatalf("runCompress: %v (%s)", err, buf.String())
	}
	var decoded compressResult
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v (%s)", err, buf.String())
	}
	if len(decoded.Files) != 1 {
		t.Fatalf("files: got %d entries, want 1", len(decoded.Files))
	}
	return decoded.Files[0]
}

func TestCompress_BuildsAnIndexForTheFileItWrote(t *testing.T) {
	dir, input := compressFixture(t)
	output := filepath.Join(dir, "app.log.zst")

	entry := runCompressJSON(t, compressParams{
		paths: []string{input}, output: output,
		frameSize: "64K", level: 3, workers: 1, buildIdx: true,
	})

	if msg, ok := entry["index_error"]; ok {
		t.Fatalf("index_error: %v", msg)
	}
	built, ok := entry["index"].(map[string]any)
	if !ok {
		t.Fatalf("no index key in %v", entry)
	}
	if lines, _ := built["line_count"].(float64); int64(lines) != 20000 {
		t.Errorf("line_count: got %v, want 20000", built["line_count"])
	}
	if frames, _ := built["frame_count"].(float64); frames < 2 {
		t.Errorf("frame_count: got %v, want several", built["frame_count"])
	}

	// The index is on disk and describes the .zst, not the source.
	idx, err := index.LoadForSource(output)
	if err != nil {
		t.Fatalf("load index: %v", err)
	}
	if idx == nil {
		t.Fatal("no index was written to the cache")
	}
	if idx.FileType != rxtypes.FileTypeSeekableZstd {
		t.Errorf("file_type: got %q, want seekable_zstd", idx.FileType)
	}
	if idx.Frames == nil || len(*idx.Frames) == 0 {
		t.Error("the index carries no frame table")
	}
	if idx.LineCount == nil || *idx.LineCount != 20000 {
		t.Errorf("line_count in the index: got %v, want 20000", idx.LineCount)
	}
}

func TestCompress_NoIndexWritesNothingAndReportsNothing(t *testing.T) {
	dir, input := compressFixture(t)
	output := filepath.Join(dir, "app.log.zst")

	entry := runCompressJSON(t, compressParams{
		paths: []string{input}, output: output,
		frameSize: "64K", level: 3, workers: 1, buildIdx: false,
	})

	if _, ok := entry["index"]; ok {
		t.Errorf("--no-index still reported an index: %v", entry["index"])
	}
	if _, ok := entry["index_error"]; ok {
		t.Errorf("--no-index reported an index_error: %v", entry["index_error"])
	}
	idx, err := index.LoadForSource(output)
	if err == nil && idx != nil {
		t.Error("--no-index still wrote an index to the cache")
	}
}
