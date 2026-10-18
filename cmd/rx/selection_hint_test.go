package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// `--signatures WORD` reads as the default set followed by the path
// WORD; when WORD is a selection the catalogue knows, the path-not-found
// error gets a hint that names the right spelling, on stderr only, for
// rx scan and rx trace alike. A word that is no selection gets none.
func TestRxBinary_HintsWhenASignatureSelectionIsTakenForAPath(t *testing.T) {
	_, path := writeLog(t)
	cache := "RX_CACHE_DIR=" + t.TempDir()
	hint := "Hint: traceback is a signature selection, not a path; write --signatures=traceback"

	for _, args := range [][]string{
		{"scan", "--signatures", "traceback", path},
		{"trace", "--signatures", "traceback", "-e", "error", path},
	} {
		code, _, stderr := runRxEnv(t, []string{cache}, args...)
		if code != 3 || !strings.Contains(stderr, hint) {
			t.Errorf("%v: exit %d, stderr %q; want 3 and the hint", args, code, stderr)
		}
	}
	for _, word := range []string{"sql", "tag:postgres", "group:secret", "all", "default", "traceback,sql"} {
		code, _, stderr := runRxEnv(t, []string{cache}, "scan", "--signatures", word, path)
		if code != 3 || !strings.Contains(stderr, "Hint: "+word+" is a signature selection") {
			t.Errorf("%s: exit %d, stderr %q; want the hint", word, code, stderr)
		}
	}
	code, _, stderr := runRxEnv(t, []string{cache}, "scan", "--signatures", "bogus", path)
	if code != 3 || strings.Contains(stderr, "Hint:") {
		t.Errorf("bogus: exit %d, stderr %q; want 3 and no hint", code, stderr)
	}
	code, stdout, stderr := runRxEnv(t, []string{cache}, "scan", "--signatures", "traceback", path, "--json")
	var answer map[string]any
	if code != 3 || !strings.Contains(stderr, hint) || json.Unmarshal([]byte(stdout), &answer) != nil {
		t.Errorf("--json: exit %d, stderr %q, stdout %q; want the hint on stderr and JSON on stdout", code, stderr, stdout)
	}
}
