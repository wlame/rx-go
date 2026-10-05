package clicommand

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
)

// A directory of rotated logs often holds an empty one, and `rx
// compress` turns an empty log into a seekable zstd of no frames. `rx
// index -r` indexes it as a file of no lines beside the others and
// exits 0, rather than failing the directory.
func TestIndex_DirectoryWithAnEmptySeekableLogIndexesEveryFile(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	writeInput(t, dir, "app.log", []byte("LINE 1\nLINE 2\n"))
	empty := filepath.Join(dir, "old.log.zst")
	var body bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 4096, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(nil), 0, &body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(empty, body.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{dir}, recursive: true, threshold: &zero})

	indexed, _ := result["indexed"].([]any)
	if len(indexed) != 2 {
		t.Fatalf("indexed: got %v, want both files", result)
	}
	for _, raw := range indexed {
		entry, _ := raw.(map[string]any)
		if filepath.Base(entry["path"].(string)) == "old.log.zst" && entry["line_count"] != float64(0) {
			t.Errorf("old.log.zst: line_count %v, want 0", entry["line_count"])
		}
	}
}
