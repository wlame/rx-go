package security

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- ValidatePath: path within single root ---

func TestValidatePath_FileInsideRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0644))

	resolved, err := ValidatePath(file, []string{root})
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

func TestValidatePath_NestedFileInsideRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b", "c")
	require.NoError(t, os.MkdirAll(sub, 0755))
	file := filepath.Join(sub, "deep.txt")
	require.NoError(t, os.WriteFile(file, []byte("deep"), 0644))

	resolved, err := ValidatePath(file, []string{root})
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

func TestValidatePath_RootItself(t *testing.T) {
	root := t.TempDir()
	resolved, err := ValidatePath(root, []string{root})
	require.NoError(t, err)
	assert.Equal(t, root, resolved)
}

// --- ValidatePath: path outside root ---

func TestValidatePath_AbsolutePathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	_, err := ValidatePath("/etc/passwd", []string{root})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside all search roots")
}

func TestValidatePath_PathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	_, err := ValidatePath(outside, []string{root})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside all search roots")
}

// --- ValidatePath: traversal with ../ ---

func TestValidatePath_DotDotEscapeBlocked(t *testing.T) {
	root := t.TempDir()
	// Try to escape: root/../etc/passwd
	escapePath := filepath.Join(root, "..", "etc", "passwd")
	_, err := ValidatePath(escapePath, []string{root})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside")
}

func TestValidatePath_MultipleDotDotEscapeBlocked(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b", "c")
	require.NoError(t, os.MkdirAll(sub, 0755))

	// Try deep escape: root/a/b/c/../../../../../../../etc/passwd
	escapePath := filepath.Join(sub, "..", "..", "..", "..", "..", "..", "..", "etc", "passwd")
	_, err := ValidatePath(escapePath, []string{root})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside")
}

func TestValidatePath_DotDotInsideRootAllowed(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0755))
	file := filepath.Join(root, "a", "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0644))

	// root/a/b/../file.txt resolves to root/a/file.txt — still within root.
	dotPath := filepath.Join(sub, "..", "file.txt")
	resolved, err := ValidatePath(dotPath, []string{root})
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

// --- ValidatePath: symlinks ---

func TestValidatePath_SymlinkInsideRootToFileInsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests require Unix")
	}

	root := t.TempDir()
	target := filepath.Join(root, "real.txt")
	require.NoError(t, os.WriteFile(target, []byte("real"), 0644))

	link := filepath.Join(root, "link.txt")
	require.NoError(t, os.Symlink(target, link))

	resolved, err := ValidatePath(link, []string{root})
	require.NoError(t, err)
	assert.Equal(t, target, resolved)
}

func TestValidatePath_SymlinkInsideRootToFileOutsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests require Unix")
	}

	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0644))

	link := filepath.Join(root, "escape.txt")
	require.NoError(t, os.Symlink(outsideFile, link))

	_, err := ValidatePath(link, []string{root})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside")
}

func TestValidatePath_SymlinkDirectoryEscapeBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests require Unix")
	}

	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644))

	// Create symlink directory inside root pointing outside.
	link := filepath.Join(root, "escape_dir")
	require.NoError(t, os.Symlink(outside, link))

	// Try to access file through the escaped symlink.
	_, err := ValidatePath(filepath.Join(link, "secret.txt"), []string{root})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside")
}

// --- ValidatePath: relative paths ---

func TestValidatePath_RelativePathResolvedFromRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "subdir")
	require.NoError(t, os.MkdirAll(sub, 0755))
	file := filepath.Join(sub, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0644))

	resolved, err := ValidatePath("subdir/file.txt", []string{root})
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

func TestValidatePath_RelativeDotDotEscape(t *testing.T) {
	root := t.TempDir()
	_, err := ValidatePath("../etc/passwd", []string{root})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside")
}

// --- ValidatePath: multiple search roots ---

func TestValidatePath_MultipleRoots_FirstRoot(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	file := filepath.Join(root1, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0644))

	resolved, err := ValidatePath(file, []string{root1, root2})
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

func TestValidatePath_MultipleRoots_SecondRoot(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	file := filepath.Join(root2, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0644))

	resolved, err := ValidatePath(file, []string{root1, root2})
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

func TestValidatePath_MultipleRoots_OutsideAll(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(outside, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0644))

	_, err := ValidatePath(file, []string{root1, root2})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside all search roots")
}

func TestValidatePath_MultipleRoots_SymlinkBetweenRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests require Unix")
	}

	root1 := t.TempDir()
	root2 := t.TempDir()
	target := filepath.Join(root2, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("content"), 0644))

	// Symlink in root1 pointing to file in root2 — should be valid since
	// root2 is also a search root.
	link := filepath.Join(root1, "cross_link.txt")
	require.NoError(t, os.Symlink(target, link))

	resolved, err := ValidatePath(link, []string{root1, root2})
	require.NoError(t, err)
	assert.Equal(t, target, resolved)
}

func TestValidatePath_MultipleRoots_SymlinkEscapeFromOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests require Unix")
	}

	root1 := t.TempDir()
	root2 := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0644))

	link := filepath.Join(root1, "escape.txt")
	require.NoError(t, os.Symlink(outsideFile, link))

	_, err := ValidatePath(link, []string{root1, root2})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside all search roots")
}

// --- ValidatePath: no search roots ---

func TestValidatePath_NoSearchRoots(t *testing.T) {
	_, err := ValidatePath("/some/path", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no search roots configured")
}

func TestValidatePath_EmptySearchRoots(t *testing.T) {
	_, err := ValidatePath("/some/path", []string{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no search roots configured")
}

// --- ValidatePath: edge cases ---

func TestValidatePath_PathWithSpaces(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "path with spaces")
	require.NoError(t, os.MkdirAll(dir, 0755))
	file := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0644))

	resolved, err := ValidatePath(file, []string{root})
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

func TestValidatePath_PathWithSpecialChars(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "special-dir_v2.0")
	require.NoError(t, os.MkdirAll(dir, 0755))
	file := filepath.Join(dir, "file[1].txt")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0644))

	resolved, err := ValidatePath(file, []string{root})
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

func TestValidatePath_NonExistentFileInsideRoot(t *testing.T) {
	root := t.TempDir()
	// The file doesn't exist but the path is within the root.
	nonexistent := filepath.Join(root, "nonexistent.txt")

	resolved, err := ValidatePath(nonexistent, []string{root})
	require.NoError(t, err)
	assert.Equal(t, nonexistent, resolved)
}

func TestValidatePath_DeeplyNestedEscape(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c", "d")
	require.NoError(t, os.MkdirAll(deep, 0755))

	escapePath := filepath.Join(deep, "..", "..", "..", "..", "..", "etc", "passwd")
	_, err := ValidatePath(escapePath, []string{root})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outside")
}

// --- isWithin tests ---

func TestIsWithin_ExactMatch(t *testing.T) {
	assert.True(t, isWithin("/a/b", "/a/b"))
}

func TestIsWithin_ChildPath(t *testing.T) {
	assert.True(t, isWithin("/a/b/c", "/a/b"))
}

func TestIsWithin_NotChild(t *testing.T) {
	assert.False(t, isWithin("/a/bx/c", "/a/b"))
}

func TestIsWithin_PrefixAttack(t *testing.T) {
	// "/tmp/rootfoo" should NOT be considered within "/tmp/root".
	assert.False(t, isWithin("/tmp/rootfoo", "/tmp/root"))
}

func TestIsWithin_Root(t *testing.T) {
	assert.True(t, isWithin("/a/b", "/"))
}

// --- resolvePathBestEffort tests ---

func TestResolvePathBestEffort_ExistingPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "exists.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0644))

	resolved, err := resolvePathBestEffort(file)
	require.NoError(t, err)
	assert.Equal(t, file, resolved)
}

func TestResolvePathBestEffort_NonExistentFile(t *testing.T) {
	dir := t.TempDir()
	nonexistent := filepath.Join(dir, "ghost.txt")

	resolved, err := resolvePathBestEffort(nonexistent)
	require.NoError(t, err)
	assert.Equal(t, nonexistent, resolved)
}

func TestResolvePathBestEffort_SymlinkResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests require Unix")
	}

	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	require.NoError(t, os.MkdirAll(real, 0755))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(real, link))

	resolved, err := resolvePathBestEffort(link)
	require.NoError(t, err)
	assert.Equal(t, real, resolved)
}
