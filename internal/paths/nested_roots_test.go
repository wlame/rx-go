package paths

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// nestedRoots builds A with A/.b/x.log, A/.b/.c/y.log, A/.other/z.log
// and a link A/tob -> A/.b, and returns A.
func nestedRoots(t *testing.T) string {
	t.Helper()
	a := t.TempDir()
	for _, dir := range []string{filepath.Join(a, ".b", ".c"), filepath.Join(a, ".other")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{filepath.Join(".b", "x.log"), filepath.Join(".b", ".c", "y.log"), filepath.Join(".other", "z.log")} {
		if err := os.WriteFile(filepath.Join(a, name), []byte("LINE 1 NEEDLE\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(a, ".b"), filepath.Join(a, "tob")); err != nil {
		t.Fatal(err)
	}
	return a
}

// A hidden directory named as a root serves its files although a parent
// directory is a root too, whatever the order of the two roots: the
// hidden rule applies below the nearest root. A hidden component below
// that root, and one below the parent root only, stay refused; with
// hidden entries on everything is served.
func TestValidate_NestedRootsCheckTheHiddenRuleBelowTheNearestRoot(t *testing.T) {
	a := nestedRoots(t)
	b := filepath.Join(a, ".b")
	x, y, z := filepath.Join(b, "x.log"), filepath.Join(b, ".c", "y.log"), filepath.Join(a, ".other", "z.log")
	for name, roots := range map[string][]string{"parent first": {a, b}, "hidden root first": {b, a}} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(Reset)
			if err := SetSearchRoots(roots); err != nil {
				t.Fatal(err)
			}
			SetIncludeHidden(false)
			if _, err := ValidatePathWithinRoots(x); err != nil {
				t.Errorf("%s: %v, want accepted (the root %s holds it)", x, err, b)
			}
			for _, refused := range []string{y, z} {
				var hidden *ErrHiddenPath
				if _, err := ValidatePathWithinRoots(refused); !errors.As(err, &hidden) {
					t.Errorf("%s: %v, want a hidden-path refusal", refused, err)
				}
			}
			SetIncludeHidden(true)
			t.Cleanup(func() { SetIncludeHidden(false) })
			for _, path := range []string{x, y, z} {
				if _, err := ValidatePathWithinRoots(path); err != nil {
					t.Errorf("--hidden: %s: %v, want accepted", path, err)
				}
			}
		})
	}
}

// A walk of the parent root follows a link into the hidden root, since
// naming a file there is allowed, and still leaves the hidden entry
// itself out of the listing.
func TestWalkDir_FollowsALinkIntoAHiddenRoot(t *testing.T) {
	t.Cleanup(Reset)
	a := nestedRoots(t)
	b := filepath.Join(a, ".b")
	if err := SetSearchRoots([]string{a, b}); err != nil {
		t.Fatal(err)
	}
	SetIncludeHidden(false)
	root, err := Pin(a)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := WalkPinned(root, true)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]WalkEntry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	viaLink := filepath.Join(a, "tob", "x.log")
	if e, ok := byPath[viaLink]; !ok || e.Refused != "" {
		t.Errorf("%s: reported %+v, want it read through the link into the root %s", viaLink, e, b)
	}
	for path := range byPath {
		if filepath.Base(filepath.Dir(path)) == ".b" || filepath.Base(path) == ".b" {
			t.Errorf("the walk listed the hidden entry %s by name", path)
		}
	}
	if e, ok := byPath[filepath.Join(a, "tob", ".c", "y.log")]; ok && e.Refused == "" {
		t.Errorf("%s read, want refused: .c is hidden below the nearest root", e.Path)
	}
}
