package index

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TestBuildIndexesTheTextInsideACompressedFile is the regression test
// for an index built over compressed bytes.
//
// The line index maps line numbers to byte offsets. Built straight off
// a .gz those offsets addressed compressed bytes, so the checkpoints
// pointed at noise and the statistics described the container instead
// of the log — a gzipped log came back with four times its lines and a
// "mixed" line ending. rx-python indexes the decompressed content, and
// this must agree with it.
func TestBuildIndexesTheTextInsideACompressedFile(t *testing.T) {
	dir := t.TempDir()
	text := []byte("line one\nline two\nline three\n")

	plain := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, text, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write(text); err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	compressed := filepath.Join(dir, "app.log.gz")
	if err := os.WriteFile(compressed, gzBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write gzip fixture: %v", err)
	}

	fromPlain, err := Build(plain, BuildOptions{})
	if err != nil {
		t.Fatalf("Build(plain): %v", err)
	}
	fromGzip, err := Build(compressed, BuildOptions{})
	if err != nil {
		t.Fatalf("Build(.gz): %v", err)
	}

	if *fromGzip.LineCount != *fromPlain.LineCount {
		t.Errorf("gzip index counts %d lines, the text has %d",
			*fromGzip.LineCount, *fromPlain.LineCount)
	}
	if *fromGzip.LineEnding != *fromPlain.LineEnding {
		t.Errorf("gzip index reports line ending %q, the text has %q",
			*fromGzip.LineEnding, *fromPlain.LineEnding)
	}
	if fromGzip.CompressionFormat == nil || *fromGzip.CompressionFormat != "gzip" {
		t.Errorf("compression_format = %v, want gzip", fromGzip.CompressionFormat)
	}
	if fromGzip.DecompressedSizeBytes == nil || *fromGzip.DecompressedSizeBytes != int64(len(text)) {
		t.Errorf("decompressed_size_bytes = %v, want %d", fromGzip.DecompressedSizeBytes, len(text))
	}
	if fromGzip.SourceSizeBytes != int64(gzBuf.Len()) {
		t.Errorf("source_size_bytes = %d, want the compressed size %d",
			fromGzip.SourceSizeBytes, gzBuf.Len())
	}
	if fromGzip.FileType != rxtypes.FileTypeCompressed {
		t.Errorf("file_type = %q, want compressed", fromGzip.FileType)
	}
	// Checkpoint offsets address the text, so they match the plain file's.
	if len(fromGzip.LineIndex) != len(fromPlain.LineIndex) {
		t.Fatalf("gzip index has %d checkpoints, plain %d",
			len(fromGzip.LineIndex), len(fromPlain.LineIndex))
	}
	for i := range fromPlain.LineIndex {
		if fromGzip.LineIndex[i] != fromPlain.LineIndex[i] {
			t.Fatalf("checkpoint %d: gzip %+v, plain %+v",
				i, fromGzip.LineIndex[i], fromPlain.LineIndex[i])
		}
	}
}
