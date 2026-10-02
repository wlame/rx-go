package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// Build checks its source as a named path before it reads a byte: a
// directory walk hands it paths that were checked earlier, and a link
// among them can be retargeted out of the search root in between.
func TestBuild_RefusesASourceOutsideTheSearchRoots(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(base, "secret.log")
	if err := os.WriteFile(secret, []byte("LINE 1 SECRET\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(root, "app.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(paths.Reset)

	idx, err := Build(link, BuildOptions{})

	var outside *paths.ErrPathOutsideRoots
	if !errors.As(err, &outside) {
		t.Errorf("Build = %+v, %v; want ErrPathOutsideRoots", idx, err)
	}
}
