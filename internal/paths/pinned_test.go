package paths

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/linktree"
)

// sandboxed builds the link tree and confines the sandbox to its root,
// with hidden entries off.
func sandboxed(t *testing.T) linktree.Tree {
	t.Helper()
	tree := linktree.Build(t)
	if err := SetSearchRoots([]string{tree.Root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(Reset)
	SetIncludeHidden(false)
	return tree
}

// retarget points the symbolic link at link to target, the way a user
// who can write in a served directory would, between a check and a read.
func retarget(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove %s: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// readPinned opens p and reads it whole.
func readPinned(p Pinned) (string, error) {
	f, err := p.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	return string(data), err
}

func TestPin_RefusesWhatANamedPathCheckRefuses(t *testing.T) {
	tree := sandboxed(t)

	var outside *ErrPathOutsideRoots
	if _, err := Pin(tree.Out); !errors.As(err, &outside) {
		t.Errorf("Pin(%s) = %v, want ErrPathOutsideRoots", tree.Out, err)
	}
	var hidden *ErrHiddenPath
	if _, err := Pin(tree.Visible); !errors.As(err, &hidden) {
		t.Errorf("Pin(%s) = %v, want ErrHiddenPath", tree.Visible, err)
	}
	if _, err := Pin(tree.Dangling); err == nil {
		t.Errorf("Pin(%s) accepted a link that leads nowhere", tree.Dangling)
	}
}

func TestPinnedOpen_ReadsTheFileItWasPinnedToUnderTheCallersSpelling(t *testing.T) {
	tree := sandboxed(t)

	pinned, err := Pin(tree.InLink)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if pinned.Path() != tree.InLink {
		t.Errorf("Path() = %s, want the caller's spelling %s", pinned.Path(), tree.InLink)
	}
	if text, err := readPinned(pinned); err != nil || text != "LINE 1 inside NEEDLE\n" {
		t.Errorf("read %q, %v; want the text of in.log", text, err)
	}
}

// A link checked while it led inside the root and retargeted before the
// read is not followed to its new target.
func TestPinnedOpen_RefusesALinkRetargetedAfterThePin(t *testing.T) {
	tree := sandboxed(t)
	pinned, err := Pin(tree.InLink)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	retarget(t, tree.InLink, filepath.Join("..", "outside", "secret.log"))

	text, err := readPinned(pinned)
	if !errors.Is(err, ErrFileChanged) {
		t.Errorf("read %q, %v; want ErrFileChanged", text, err)
	}
}

// The general form: a named file replaced by a link after its check.
func TestPinnedOpen_RefusesANamedFileReplacedByALinkAfterThePin(t *testing.T) {
	tree := sandboxed(t)
	pinned, err := Pin(tree.In)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	retarget(t, tree.In, tree.Secret)

	text, err := readPinned(pinned)
	if !errors.Is(err, ErrFileChanged) {
		t.Errorf("read %q, %v; want ErrFileChanged", text, err)
	}
	if _, err := pinned.Stat(); !errors.Is(err, ErrFileChanged) {
		t.Errorf("Stat() = %v, want ErrFileChanged", err)
	}
}

// walkedFile walks the tree's root and returns the entry the walk
// reported for path.
func walkedFile(t *testing.T, tree linktree.Tree, path string) WalkEntry {
	t.Helper()
	entries, err := WalkDir(tree.Root, true)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Path == path {
			if entry.Refused != "" {
				t.Fatalf("%s refused: %s", path, entry.Refused)
			}
			return entry
		}
	}
	t.Fatalf("the walk did not report %s", path)
	return WalkEntry{}
}

// A walk hands out each file with the identity it checked. A link
// retargeted between the walk and the read is not followed.
func TestWalkDir_LinkRetargetedAfterTheWalkIsNotRead(t *testing.T) {
	tree := sandboxed(t)
	entry := walkedFile(t, tree, tree.InLink)

	retarget(t, tree.InLink, filepath.Join("..", "outside", "secret.log"))

	if text, err := readPinned(entry.File); !errors.Is(err, ErrFileChanged) {
		t.Errorf("read %q, %v; want ErrFileChanged", text, err)
	}
}

// A directory of the walk swapped for a link to a directory outside the
// root, between the walk and the read: the files the walk found in it
// are not read from the other directory.
func TestWalkDir_DirectorySwappedForALinkAfterTheWalkIsNotRead(t *testing.T) {
	tree := sandboxed(t)
	entry := walkedFile(t, tree, tree.Deep)

	sub := filepath.Dir(tree.Deep)
	if err := os.Rename(sub, sub+"-moved"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	outsideDir := filepath.Dir(tree.Secret2)
	if err := os.Rename(tree.Secret2, filepath.Join(outsideDir, filepath.Base(tree.Deep))); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Symlink(outsideDir, sub); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if text, err := readPinned(entry.File); !errors.Is(err, ErrFileChanged) {
		t.Errorf("read %q, %v; want ErrFileChanged", text, err)
	}
}
