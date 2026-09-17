package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/testutil/linktree"
)

// walkWithin builds the link tree, confines the sandbox to its root and
// walks the root recursively. It returns the tree and what the walk
// reported for each path.
func walkWithin(t *testing.T, includeHidden bool) (linktree.Tree, map[string]WalkEntry) {
	t.Helper()
	tree := linktree.Build(t)
	if err := SetSearchRoots([]string{tree.Root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(Reset)
	SetIncludeHidden(includeHidden)
	t.Cleanup(func() { SetIncludeHidden(false) })

	entries, err := WalkDir(tree.Root, true)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	byPath := map[string]WalkEntry{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	return tree, byPath
}

func TestWalkDir_RefusesWhatANamedPathCheckRefuses(t *testing.T) {
	tree, got := walkWithin(t, false)

	// "" means the walk reports the path as a file to read.
	want := map[string]string{
		tree.In:         "",
		tree.InLink:     "",
		tree.Deep:       "",
		tree.DeepByLink: "",
		tree.Out:        ReasonLinkOutsideRoots,
		tree.OutDir:     ReasonLinkOutsideRoots,
		tree.Visible:    "symlink leads into hidden entry '.private'",
		tree.Loop:       ReasonLinkLoop,
		tree.Self:       "cannot resolve symlink",
		tree.Dangling:   "cannot resolve symlink",
	}
	if len(got) != len(want) {
		t.Errorf("walk reported %d paths, want %d: %v", len(got), len(want), got)
	}
	for path, wantReason := range want {
		entry, ok := got[path]
		switch {
		case !ok:
			t.Errorf("%s not reported", path)
		case wantReason == "" && entry.Refused != "":
			t.Errorf("%s refused (%q), want it read", path, entry.Refused)
		case !strings.HasPrefix(entry.Refused, wantReason):
			t.Errorf("%s refused with %q, want %q", path, entry.Refused, wantReason)
		}
	}
}

func TestWalkDir_HiddenOnFollowsALinkIntoAHiddenDirectory(t *testing.T) {
	tree, got := walkWithin(t, true)

	for _, path := range []string{tree.Visible, tree.Private} {
		if entry, ok := got[path]; !ok || entry.Refused != "" {
			t.Errorf("%s: reported %v (present %v), want it read with hidden entries on", path, entry, ok)
		}
	}
}

// Two directories whose links point at each other: each descent finds
// a link back to a directory it is already inside, so the walk ends.
func TestWalkDir_DirectoryLinksThatPointAtEachOtherEnd(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for _, dir := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(base, dir, "x.log"), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "b"), filepath.Join(base, "a", "to-b")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "a"), filepath.Join(base, "b", "to-a")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	Reset()

	// WalkDir runs in a goroutine so the test can stop waiting after a
	// timeout; the channel has room for one result, so the goroutine
	// never blocks on a send nobody receives.
	done := make(chan []WalkEntry, 1)
	go func() {
		entries, _ := WalkDir(base, true)
		done <- entries
	}()
	var entries []WalkEntry
	select {
	case entries = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WalkDir did not end on directory links that point at each other")
	}

	files, loops := 0, 0
	for _, entry := range entries {
		switch entry.Refused {
		case "":
			files++
		case ReasonLinkLoop:
			loops++
		}
	}
	// a/x.log, a/to-b/x.log, b/x.log, b/to-a/x.log; each descent ends
	// at the link that leads back where it came from.
	if files != 4 || loops != 2 {
		t.Errorf("walk reported %d files and %d loops, want 4 and 2: %v", files, loops, entries)
	}
}

func TestWalkDir_NonRecursiveNeverReportsADirectory(t *testing.T) {
	tree := linktree.Build(t)
	Reset()

	entries, err := WalkDir(tree.Root, false)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Path == tree.SubDirLink || entry.Path == tree.OutDir || entry.Path == tree.Loop {
			t.Errorf("non-recursive walk reported the directory link %s (%q)", entry.Path, entry.Refused)
		}
	}
}

func TestWalkDir_KeepsTheCallersSpellingOfTheDirectory(t *testing.T) {
	tree := linktree.Build(t)
	Reset()

	entries, err := WalkDir(tree.Root+string(filepath.Separator), false)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Path, string(filepath.Separator)+string(filepath.Separator)) {
			t.Errorf("walk reported %s with a doubled separator", entry.Path)
		}
	}
}
