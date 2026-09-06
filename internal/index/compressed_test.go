package index

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestBuildRefusesACompressedSource pins the refusal that replaced an
// index built over compressed bytes.
//
// The line index maps line numbers to byte offsets in the file it
// describes. Built over a .gz those offsets address compressed bytes,
// so the index pointed at noise and the statistics beside it described
// the container instead of the log — a 600 MB text file came back with
// a "mixed" line ending and 50 checkpoints into nothing.
func TestBuildRefusesACompressedSource(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, []byte("line one\nline two\nline three\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write([]byte("line one\nline two\nline three\n")); err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	compressed := filepath.Join(dir, "app.log.gz")
	if err := os.WriteFile(compressed, gzBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write gzip fixture: %v", err)
	}

	if _, err := Build(compressed, BuildOptions{}); !errors.Is(err, ErrCompressedSource) {
		t.Fatalf("Build(.gz) error = %v, want ErrCompressedSource", err)
	}
	// The plain file beside it still indexes.
	if _, err := Build(plain, BuildOptions{}); err != nil {
		t.Fatalf("Build(plain): %v", err)
	}
}
