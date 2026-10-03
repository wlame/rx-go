package compressfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// An HTTP compression runs as a background task, after the request's
// path check. Compress checks its input again as it opens it, so a link
// retargeted out of the search root in between is not copied into a
// .zst inside the root.
func TestCompress_RefusesAnInputThatLeadsOutsideTheSearchRoots(t *testing.T) {
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
	output := filepath.Join(root, "app.log.zst")

	_, err = Compress(context.Background(), Options{InputPath: link, OutputPath: output, FrameSize: 1 << 20, Level: 1})

	var outside *paths.ErrPathOutsideRoots
	if !errors.As(err, &outside) {
		t.Errorf("Compress = %v, want ErrPathOutsideRoots", err)
	}
	if _, statErr := os.Stat(output); statErr == nil {
		t.Errorf("an output was written for a file outside the root")
	}
}
