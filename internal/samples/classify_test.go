package samples

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// A lookup reads a file by what its bytes are, never by its name: text
// named .gz is read as text, gzip named .log through its decompressor,
// and both answer the line the text holds.
func TestResolve_ReadsAFileByItsBytesNotItsName(t *testing.T) {
	text := []byte("LINE 1 alpha\nLINE 2 beta\nLINE 3 gamma\n")
	dir := t.TempDir()
	files := map[string][]byte{
		"text-named.log.gz": text,
		"gzip-named.log":    compressedcopy.Encode(t, compressedcopy.Gzip, text),
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		for _, req := range []Request{
			{Path: path, Lines: []OffsetOrRange{{Start: 2}}},
			{Path: path, Offsets: []OffsetOrRange{{Start: 13}}},
		} {
			resp, err := Resolve(t.Context(), req)
			if err != nil {
				t.Fatalf("%s: Resolve: %v", name, err)
			}
			for key, lines := range resp.Samples {
				if len(lines) != 1 || lines[0] != "LINE 2 beta" {
					t.Errorf("%s %s: got %q, want line 2", name, key, lines)
				}
			}
		}
	}
}

// A file that is not text has no lines to give: the lookup is refused
// with the reason, never answered with its bytes cut at newlines.
func TestResolve_RefusesAFileThatIsNotText(t *testing.T) {
	dir := t.TempDir()
	tarHeader := append([]byte("app.log"), make([]byte, 505)...)
	files := map[string][]byte{
		"logs.tar.gz": compressedcopy.Encode(t, compressedcopy.Gzip, tarHeader),
		"utf16.log":   {0xFE, 0xFF, 0, 'L', 0, '\n'},
		"bin.dat":     []byte("LINE 1\x00\nLINE 2\n"),
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := Resolve(t.Context(), Request{Path: path, Lines: []OffsetOrRange{{Start: 1}}})
		if !errors.Is(err, filekind.ErrNotText) {
			t.Errorf("%s: got %v, want a refusal wrapping ErrNotText", name, err)
		}
	}
}
