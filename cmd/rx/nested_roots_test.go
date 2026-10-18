package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With a hidden directory named as a root beside its parent, a file in
// it is served, in whichever order the roots are given.
func TestExitCode_NestedHiddenRootServesItsFiles(t *testing.T) {
	a := t.TempDir()
	b := filepath.Join(a, ".b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(b, "x.log")
	if err := os.WriteFile(x, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, roots := range [][]string{{a, b}, {b, a}} {
		args := []string{"--search-root=" + roots[0], "--search-root=" + roots[1], "samples", x, "--lines=2", "--context=0"}
		code, stdout, stderr := runRxEnv(t, []string{"RX_CACHE_DIR=" + t.TempDir()}, args...)
		if code != 0 || !strings.Contains(stdout, "two") {
			t.Errorf("roots %v: exit %d, stdout %q, stderr %q; want line 2", roots, code, stdout, stderr)
		}
	}
	code, _, stderr := runRxEnv(t, []string{"RX_CACHE_DIR=" + t.TempDir()}, "--search-root="+a, "samples", x, "--lines=2")
	if code != 4 || !strings.Contains(stderr, "hidden") {
		t.Errorf("with the parent root alone: exit %d, stderr %q; want 4, hidden", code, stderr)
	}
}
