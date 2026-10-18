package trace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeNeedleTree writes a directory holding a.log and sub/b.log, each
// with one NEEDLE line, and returns the directory.
func writeNeedleTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.log", filepath.Join("sub", "b.log")} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("LINE 1 NEEDLE\nLINE 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runNeedle(t *testing.T, paths ...string) (files map[string]string, matchFiles []string) {
	t.Helper()
	resp, err := New().RunWithOptions(context.Background(), paths, []string{"NEEDLE"}, Options{})
	if err != nil {
		t.Fatalf("Run(%v): %v", paths, err)
	}
	for _, m := range resp.Matches {
		matchFiles = append(matchFiles, m.File)
	}
	return resp.Files, matchFiles
}

// A file reached twice is searched once, under one id, so each of its
// matches comes once: the same path twice, a directory and a file in
// it, a directory and its subdirectory.
func TestEngine_Run_AFileReachedTwiceIsSearchedOnce(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := writeNeedleTree(t)
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "sub", "b.log")

	for _, tc := range []struct {
		name      string
		paths     []string
		wantFiles map[string]string
	}{
		{"the same path twice", []string{a, a}, map[string]string{"f1": a}},
		{"a directory and a file in it", []string{dir, a}, map[string]string{"f1": a, "f2": b}},
		{"a file and then its directory", []string{b, dir}, map[string]string{"f1": b, "f2": a}},
		{"a directory and its subdirectory", []string{dir, filepath.Join(dir, "sub")}, map[string]string{"f1": a, "f2": b}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, matched := runNeedle(t, tc.paths...)
			if len(files) != len(tc.wantFiles) {
				t.Fatalf("files = %v, want %v", files, tc.wantFiles)
			}
			for id, path := range tc.wantFiles {
				if files[id] != path {
					t.Errorf("files[%s] = %q, want %q (files %v)", id, files[id], path, files)
				}
			}
			if len(matched) != len(tc.wantFiles) {
				t.Errorf("matches in %v, want one per file", matched)
			}
		})
	}
}

// Two spellings of one path (a cleaned and an uncleaned form, a path
// and a symbolic link to it) name one file: it is searched once under
// the first spelling given.
func TestEngine_Run_SpellingsOfOnePathAreOneFile(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := writeNeedleTree(t)
	a := filepath.Join(dir, "a.log")
	uncleaned := filepath.Join(dir, "sub", "..", "a.log")
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(a, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	files, matched := runNeedle(t, link, uncleaned, a)
	if len(files) != 1 || files["f1"] != link {
		t.Errorf("files = %v, want the one file under its first spelling %q", files, link)
	}
	if len(matched) != 1 {
		t.Errorf("matches in %v, want one", matched)
	}
}

// The plan lists a file once however many paths lead to it, searched
// or skipped, and a path that cannot be pinned is named once as well.
func TestExpandPaths_PlansEachFileOnce(t *testing.T) {
	dir := writeNeedleTree(t)
	a := filepath.Join(dir, "a.log")
	bin := filepath.Join(dir, "sub", "binary.dat")
	if err := os.WriteFile(bin, []byte{0, 1, 2, 'x', 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.log")

	plan := ExpandPaths([]string{a, dir, bin, missing, missing}, true)
	if got := len(plan.Files); got != 2 {
		t.Errorf("planned %d files, want 2 (a.log, sub/b.log): %v", got, plan.Files)
	}
	var skipped []string
	for _, s := range plan.Skipped {
		skipped = append(skipped, s.Path)
	}
	if want := []string{bin, missing}; len(skipped) != 2 || skipped[0] != want[0] || skipped[1] != want[1] {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
}
