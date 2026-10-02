package index

import (
	"os"
	"path/filepath"
	"testing"
)

// Every kind of file rx reads gives an empty index when it holds no
// text: a plain file, each stream-compressed format and a seekable zstd
// of no frames, which is what `rx compress` writes for an empty input.
// The index says there are no lines.
func TestBuildOfAFileOfNoTextIsAnEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	files := compressedCopies(t, nil, dir)
	plain := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	files["app.log"] = plain

	for name, path := range files {
		idx, err := Build(path, BuildOptions{})
		if err != nil {
			t.Errorf("%s: Build: %v", name, err)
			continue
		}
		if idx.LineCount == nil || *idx.LineCount != 0 {
			t.Errorf("%s: line_count %v, want 0", name, idx.LineCount)
		}
		if len(idx.LineIndex) != 0 {
			t.Errorf("%s: line_index %v, want none", name, idx.LineIndex)
		}
	}
}
