package paths

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Running `rx serve` from a home directory used to serve everything in
// it, including ~/.ssh, ~/.aws and ~/.gnupg — readable through
// /v1/samples by anyone who could reach the port. Hiding those entries
// from the directory listing is not enough on its own: a caller who
// knows the path can ask for the file directly. The rule therefore lives
// in the sandbox, where every entry point inherits it.
//
// The rule matches ripgrep's: a hidden entry is one whose name starts
// with a dot, and --hidden opts back in.
func setupRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Resolve symlinks so the sandbox's canonical form matches on macOS.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for _, dir := range []string{".ssh", ".config/gcloud", "logs", "logs/.cache"} {
		if err := os.MkdirAll(filepath.Join(resolved, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	for _, file := range []string{
		".bashrc", ".ssh/id_rsa", ".config/gcloud/creds.json",
		"logs/app.log", "logs/.hidden.log", "logs/.cache/warm.bin",
	} {
		if err := os.WriteFile(filepath.Join(resolved, file), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if err := SetSearchRoots([]string{resolved}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(Reset)
	return resolved
}

func TestValidatePathWithinRoots_RefusesHiddenByDefault(t *testing.T) {
	root := setupRoot(t)
	SetIncludeHidden(false)

	for _, rel := range []string{
		".bashrc",
		".ssh",
		".ssh/id_rsa",
		".config/gcloud/creds.json",
		"logs/.hidden.log",
		"logs/.cache",
		"logs/.cache/warm.bin",
	} {
		_, err := ValidatePathWithinRoots(filepath.Join(root, rel))
		if err == nil {
			t.Errorf("%s was served; a hidden entry must be refused by default", rel)
			continue
		}
		var hidden *ErrHiddenPath
		if !errors.As(err, &hidden) {
			t.Errorf("%s was refused with %v, want ErrHiddenPath", rel, err)
		}
	}
}

func TestValidatePathWithinRoots_AllowsVisiblePaths(t *testing.T) {
	root := setupRoot(t)
	SetIncludeHidden(false)

	for _, rel := range []string{"logs", "logs/app.log"} {
		if _, err := ValidatePathWithinRoots(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s was refused: %v", rel, err)
		}
	}
	// The root itself is always reachable.
	if _, err := ValidatePathWithinRoots(root); err != nil {
		t.Errorf("the search root itself was refused: %v", err)
	}
}

// A root the operator named explicitly is a deliberate choice, even when
// it is itself hidden. Only components *below* the root are subject to
// the rule.
func TestValidatePathWithinRoots_HiddenComponentsOfTheRootAreFine(t *testing.T) {
	base := t.TempDir()
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root := filepath.Join(resolved, ".local", "share", "logs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.log"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(Reset)
	SetIncludeHidden(false)

	if _, err := ValidatePathWithinRoots(filepath.Join(root, "app.log")); err != nil {
		t.Errorf("a file under an explicitly named hidden root was refused: %v", err)
	}
}

func TestValidatePathWithinRoots_IncludeHiddenOptsBackIn(t *testing.T) {
	root := setupRoot(t)
	SetIncludeHidden(true)
	t.Cleanup(func() { SetIncludeHidden(false) })

	for _, rel := range []string{".ssh/id_rsa", ".bashrc", "logs/.hidden.log"} {
		if _, err := ValidatePathWithinRoots(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s was refused with hidden access enabled: %v", rel, err)
		}
	}
}

// The sandbox error must stay distinguishable from the hidden error, so
// the HTTP layer can explain which rule was hit.
func TestValidatePathWithinRoots_OutsideRootsIsNotAHiddenError(t *testing.T) {
	setupRoot(t)
	SetIncludeHidden(false)

	_, err := ValidatePathWithinRoots("/etc/passwd")
	var hidden *ErrHiddenPath
	if errors.As(err, &hidden) {
		t.Error("a path outside the roots was reported as hidden")
	}
	var outside *ErrPathOutsideRoots
	if !errors.As(err, &outside) {
		t.Errorf("got %v, want ErrPathOutsideRoots", err)
	}
}

func TestIsHiddenName(t *testing.T) {
	for name, want := range map[string]bool{
		".ssh":       true,
		".bashrc":    true,
		".":          false, // Clean removes these; never a real component
		"..":         false,
		"logs":       false,
		"app.log":    false,
		"v1.2":       false,
		"":           false,
		"a.hidden":   false,
		".gitignore": true,
	} {
		if got := isHiddenName(name); got != want {
			t.Errorf("isHiddenName(%q) = %v, want %v", name, got, want)
		}
	}
}
