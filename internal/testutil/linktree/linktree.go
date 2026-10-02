// Package linktree builds a directory tree whose symbolic links lead
// everywhere a directory walk can be led: out of the search root, into a
// hidden directory, back to an ancestor, to themselves and to nothing.
//
// Tests of every surface that walks a directory (trace, index, the tree
// listing) use the same tree, so they all check the same rule.
//
// This is test-only infrastructure. It lives under internal/testutil so
// production binaries never depend on it, and it must not be imported
// from non-test code.
package linktree

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Tree holds the paths of one built tree. Every path is canonical (its
// symlinks resolved), so it compares equal to what the sandbox stores.
//
// Layout, with the text of each file:
//
//	Root/in.log                 "LINE 1 inside NEEDLE"
//	Root/inlink.log         ->  in.log                   (inside the root)
//	Root/sub/deep.log           "LINE 1 deep NEEDLE"
//	Root/subdirlink         ->  sub                      (directory, inside)
//	Root/.private/p.log         "LINE 1 PRIVATE NEEDLE"
//	Root/visible.log        ->  .private/p.log           (into a hidden directory)
//	Root/out.log            ->  ../outside/secret.log    (outside the root)
//	Root/outdir             ->  ../outside/dir           (directory, outside)
//	Root/loop               ->  .                        (an ancestor)
//	Root/self.log           ->  self.log                 (a cycle of links)
//	Root/dangling.log       ->  missing.log              (nothing)
//	Outside/secret.log          "LINE 1 SECRET NEEDLE"
//	Outside/dir/secret2.log     "LINE 1 SECRET2 NEEDLE"
type Tree struct {
	Root    string
	Outside string

	In         string // Root/in.log
	InLink     string // Root/inlink.log
	Deep       string // Root/sub/deep.log
	SubDirLink string // Root/subdirlink
	DeepByLink string // Root/subdirlink/deep.log
	Private    string // Root/.private/p.log
	Visible    string // Root/visible.log
	Out        string // Root/out.log
	OutDir     string // Root/outdir
	Loop       string // Root/loop
	Self       string // Root/self.log
	Dangling   string // Root/dangling.log
	Secret     string // Outside/secret.log
	Secret2    string // Outside/dir/secret2.log
}

// Build creates the tree in a fresh temporary directory. It does not
// configure the sandbox: a test calls paths.SetSearchRoots with
// tree.Root when it wants one.
func Build(t *testing.T) Tree {
	t.Helper()
	// t.TempDir is under /var on macOS, which is a symlink to
	// /private/var; resolving it keeps every path below canonical.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	tree := Tree{
		Root:       root,
		Outside:    outside,
		In:         filepath.Join(root, "in.log"),
		InLink:     filepath.Join(root, "inlink.log"),
		Deep:       filepath.Join(root, "sub", "deep.log"),
		SubDirLink: filepath.Join(root, "subdirlink"),
		DeepByLink: filepath.Join(root, "subdirlink", "deep.log"),
		Private:    filepath.Join(root, ".private", "p.log"),
		Visible:    filepath.Join(root, "visible.log"),
		Out:        filepath.Join(root, "out.log"),
		OutDir:     filepath.Join(root, "outdir"),
		Loop:       filepath.Join(root, "loop"),
		Self:       filepath.Join(root, "self.log"),
		Dangling:   filepath.Join(root, "dangling.log"),
		Secret:     filepath.Join(outside, "secret.log"),
		Secret2:    filepath.Join(outside, "dir", "secret2.log"),
	}

	for _, dir := range []string{
		filepath.Join(root, "sub"),
		filepath.Join(root, ".private"),
		filepath.Join(outside, "dir"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for path, text := range map[string]string{
		tree.In:      "LINE 1 inside NEEDLE\n",
		tree.Deep:    "LINE 1 deep NEEDLE\n",
		tree.Private: "LINE 1 PRIVATE NEEDLE\n",
		tree.Secret:  "LINE 1 SECRET NEEDLE\n",
		tree.Secret2: "LINE 1 SECRET2 NEEDLE\n",
	} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	// Relative targets, as a user or a log rotation tool would write
	// them: the tree still means the same thing if it is moved.
	for link, target := range map[string]string{
		tree.InLink:     "in.log",
		tree.SubDirLink: "sub",
		tree.Visible:    filepath.Join(".private", "p.log"),
		tree.Out:        filepath.Join("..", "outside", "secret.log"),
		tree.OutDir:     filepath.Join("..", "outside", "dir"),
		tree.Loop:       ".",
		tree.Self:       "self.log",
		tree.Dangling:   "missing.log",
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink %s -> %s: %v", link, target, err)
		}
	}
	return tree
}

// FanOut holds the paths of a tree built by BuildFanOut.
type FanOut struct {
	// Root is the directory that holds the levels d0, d1, ...
	Root string
	// Top is Root/d0, where a search starts.
	Top string
	// File is the one file of the tree, in the last level.
	File string
	// Levels and Links are the sizes the tree was built with.
	Levels, Links int
}

// BuildFanOut creates levels+1 directories Root/d0 … Root/d<levels>.
// Every directory but the last holds `links` symbolic links l1, l2, …
// to the next one; the last holds f.log, "LINE 1 NEEDLE".
//
// A walk that follows every link the way it meets it reaches f.log
// links^levels times (6 levels of 6 links: 46,656 paths). A walk that
// enters each directory once reaches it once.
func BuildFanOut(t *testing.T, levels, links int) FanOut {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	root := filepath.Join(base, "root")
	level := func(i int) string { return filepath.Join(root, "d"+strconv.Itoa(i)) }
	for i := 0; i <= levels; i++ {
		if err := os.MkdirAll(level(i), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	for i := 0; i < levels; i++ {
		for k := 1; k <= links; k++ {
			link := filepath.Join(level(i), "l"+strconv.Itoa(k))
			if err := os.Symlink(filepath.Join("..", "d"+strconv.Itoa(i+1)), link); err != nil {
				t.Fatalf("symlink %s: %v", link, err)
			}
		}
	}
	file := filepath.Join(level(levels), "f.log")
	if err := os.WriteFile(file, []byte("LINE 1 NEEDLE\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	return FanOut{Root: root, Top: level(0), File: file, Levels: levels, Links: links}
}
