package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// An index is looked up and validated by path, and a pinned file is
// read through its handle. When the path led to another file at the
// moment of the look-up — a link retargeted and later put back — the
// index stored for the path describes that other file, and its
// checkpoints would number the pinned file's lines wrongly.
// LoadForPinned treats such an index as absent.
func TestLoadForPinned_AnIndexOfAnotherFileIsAbsent(t *testing.T) {
	paths.Reset()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	writePlain(t, filepath.Join(dir, "a.log"), numberedText(50, "A"))
	writePlain(t, filepath.Join(dir, "b.log"), numberedText(80, "B"))
	link := filepath.Join(dir, "app.log")
	if err := os.Symlink("a.log", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	pinnedA, err := paths.Pin(link)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	// The link now leads to b.log, and the index stored for its path
	// describes b.log: valid for the path, not for the pinned file.
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink("b.log", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	built, err := Build(link, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := Save(built); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if byPath, err := LoadForSource(link); err != nil || byPath == nil {
		t.Fatalf("LoadForSource = %v, %v; want the index of b.log", byPath, err)
	}

	if got, err := LoadForPinned(pinnedA); err != nil || got != nil {
		t.Errorf("LoadForPinned(a.log's pin) = %v, %v; want no index", got, err)
	}
	pinnedB, err := paths.Pin(link)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if got, err := LoadForPinned(pinnedB); err != nil || got == nil {
		t.Errorf("LoadForPinned(b.log's pin) = %v, %v; want the index of b.log", got, err)
	}
}
