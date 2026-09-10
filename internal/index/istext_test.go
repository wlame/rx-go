package index

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// TestIsTextFile covers the rule rx-python uses: a NUL byte in the first
// 8 KiB means binary. rx-go used to index binary files that rx-python
// skipped, so the same directory produced two different index sets.
func TestIsTextFile(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write([]byte("alpha\nbeta\n")); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"plain text", write("a.log", []byte("alpha\nbeta\ngamma\n")), true},
		{"empty file", write("empty.log", nil), true},
		{"nul in the first bytes", write("b.bin", []byte("alpha\x00beta")), false},
		{"nul at the edge of the sample", write("c.bin", append(bytes.Repeat([]byte("x"), textSampleSize-1), 0)), false},
		{"nul past the sample", write("d.log", append(bytes.Repeat([]byte("x"), textSampleSize), 0)), true},
		{"high bytes but no nul", write("e.log", bytes.Repeat([]byte{0xC3, 0xA9}, 100)), true},
		{"gzip is read through the decompressor", write("f.log.gz", gzBuf.Bytes()), true},
		{"missing file", filepath.Join(dir, "nope.log"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTextFile(tc.path); got != tc.want {
				t.Errorf("IsTextFile(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
