package index

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/filekind"
)

// No line index is ever built for a file that is not text, whoever asks
// for one: a binary file, a .tar.gz (its decompressed text starts with a
// NUL-padded tar header) and a UTF-16 file are refused with an error
// wrapping filekind.ErrNotText.
func TestBuildRefusesAFileThatIsNotText(t *testing.T) {
	dir := t.TempDir()
	var archive bytes.Buffer
	gw := gzip.NewWriter(&archive)
	_, _ = gw.Write(append([]byte("app.log"), make([]byte, 505)...))
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	files := map[string][]byte{
		"bin.dat":     []byte("LINE 1\x00\n"),
		"logs.tar.gz": archive.Bytes(),
		"utf16.log":   {0xFF, 0xFE, 'L', 0, '\n', 0},
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		idx, err := Build(path, BuildOptions{})
		if !errors.Is(err, filekind.ErrNotText) || idx != nil {
			t.Errorf("%s: got %v, %v; want an error wrapping ErrNotText", name, idx, err)
		}
	}
}
