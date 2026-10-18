package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The completion scripts `rx completion` writes call `rx __complete`
// (or `__completeNoDesc`) with the words typed so far; rx answers the
// candidates and cobra's directive line. A word that only starts like
// the request is a pattern, as any other word.
func TestExitCode_ShellCompletionRequests(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runRx(t, "__complete", "")
	if code != 0 {
		t.Fatalf("rx __complete \"\": exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"trace\t", "samples\t", "serve\t", "index\t", ":4"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("rx __complete \"\" printed %q, want %q among the candidates", stdout, want)
		}
	}

	code, stdout, _ = runRx(t, "__completeNoDesc", "")
	if code != 0 || !strings.Contains(stdout, "\nsamples\n") || strings.Contains(stdout, "samples\t") {
		t.Errorf("rx __completeNoDesc \"\": exit %d, stdout %q; want bare command names", code, stdout)
	}

	code, stdout, _ = runRx(t, "__complete", "sam")
	if code != 0 || !strings.HasPrefix(stdout, "samples\t") || strings.Contains(stdout, "\ntrace\t") {
		t.Errorf("rx __complete sam: exit %d, stdout %q; want samples only", code, stdout)
	}

	code, stdout, _ = runRx(t, "__complete", "serve", "--po")
	if code != 0 || !strings.Contains(stdout, "--port\t") {
		t.Errorf("rx __complete serve --po: exit %d, stdout %q; want --port", code, stdout)
	}

	code, _, stderr = runRxIn(t, dir, nil, "__completeX", "a.log")
	if code != 0 || strings.Contains(stderr, "unknown command") {
		t.Errorf("rx __completeX a.log: exit %d, stderr %q; want a search of the pattern __completeX (no match, exit 0)", code, stderr)
	}
}

// The space form of --search-root routes the command after its value:
// `rx --search-root DIR index a.log` builds an index.
func TestExitCode_SearchRootValueThenSubcommand(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	if err := os.WriteFile(a, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runRxEnv(t, []string{"RX_CACHE_DIR=" + t.TempDir()}, "--search-root", dir, "index", a, "--json", "--threshold=0")
	if code != 0 || !strings.Contains(stdout, `"indexed"`) {
		t.Errorf("exit %d, stdout %q, stderr %q; want an index answer", code, stdout, stderr)
	}
}
