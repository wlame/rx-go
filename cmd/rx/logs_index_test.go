package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/clicommand"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// logsIndexAnswer is what `rx logs index --json` prints for one chain.
type logsIndexAnswer struct {
	Path    string           `json:"path"`
	Indexed []map[string]any `json:"indexed"`
	Skipped []string         `json:"skipped"`
	Errors  []map[string]any `json:"errors"`
	Chain   *rxtypes.ChainResponse
}

// indexJSON runs `rx logs index <args> --json` in dir with the cache in
// cache, and decodes the answer.
func indexJSON(t *testing.T, dir, cache string, args ...string) (int, logsIndexAnswer, string) {
	t.Helper()
	code, stdout, stderr := runRxIn(t, dir, []string{"RX_CACHE_DIR=" + cache},
		append(append([]string{"logs", "index"}, args...), "--json")...)
	var answer logsIndexAnswer
	if stdout != "" {
		if err := json.Unmarshal([]byte(stdout), &answer); err != nil {
			t.Fatalf("decode %q: %v", stdout, err)
		}
	}
	return code, answer, stderr
}

// showIn runs `rx logs show <handle> --json` in dir with the cache in
// cache.
func showIn(t *testing.T, dir, cache, handle string) rxtypes.ChainResponse {
	t.Helper()
	code, stdout, stderr := runRxIn(t, dir, []string{"RX_CACHE_DIR=" + cache}, "logs", "show", handle, "--json")
	var resp rxtypes.ChainResponse
	if code != clicommand.ExitSuccess || json.Unmarshal([]byte(stdout), &resp) != nil {
		t.Fatalf("show %s: exit %d, stdout %q, stderr %s", handle, code, stdout, stderr)
	}
	return resp
}

// `rx logs index` builds and stores the index of every part, the active
// file too, however small (the parts are far below RX_LARGE_FILE_MB,
// which `rx index` skips a file by), reports each part as `rx index`
// reports a file, then prints the chain as `rx logs show` does. After
// it `rx logs show` finds every part indexed, and its answer is the one
// it gave before any index existed (the accelerator rule).
func TestLogsIndex_IndexesEveryPartThenShowsTheChain(t *testing.T) {
	dir := chainFixture(t)
	cache := t.TempDir()
	cold := showIn(t, dir, cache, "app.log")
	for _, p := range cold.Parts {
		if p.IsIndexed {
			t.Fatalf("%s is indexed before rx logs index", p.Name)
		}
	}

	code, stdout, stderr := runRxIn(t, dir, []string{"RX_CACHE_DIR=" + cache}, "logs", "index", "app.log")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) < 9 || !strings.HasPrefix(lines[0], "Indexed 3 parts in ") {
		t.Fatalf("stdout\n%s", stdout)
	}
	for i, name := range []string{"app.log.2.gz", "app.log.1", "app.log"} {
		if !strings.HasSuffix(strings.Fields(lines[1+i])[0], string(filepath.Separator)+name+":") {
			t.Fatalf("line %d %q, want the part %s", 1+i, lines[1+i], name)
		}
	}
	if !strings.Contains(lines[4], "app.log: ready, 3 parts, 18 lines, fingerprint ") {
		t.Fatalf("chain line %q", lines[4])
	}
	for _, row := range lines[6:9] {
		if fields := strings.Fields(row); fields[len(fields)-1] != "idx" {
			t.Fatalf("part row %q is not idx", row)
		}
	}

	indexed := showIn(t, dir, cache, "app.log")
	for _, p := range indexed.Parts {
		if !p.IsIndexed {
			t.Fatalf("%s is not indexed after rx logs index", p.Name)
		}
	}
	cold.CLICommand, indexed.CLICommand = "", ""
	for k := range cold.Parts {
		cold.Parts[k].IsIndexed, indexed.Parts[k].IsIndexed = false, false
	}
	if a, b := mustJSON(t, cold), mustJSON(t, indexed); a != b {
		t.Fatalf("cold and indexed descriptions differ\n%s\n%s", a, b)
	}
}

// mustJSON is v as indented JSON.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// --json prints the members of `rx index --json` for the parts and the
// chain's description after them. A second run keeps the current
// indexes; --force builds them again.
func TestLogsIndex_JSONAndForce(t *testing.T) {
	dir := chainFixture(t)
	cache := t.TempDir()
	code, first, stderr := indexJSON(t, dir, cache, "app.log")
	if code != clicommand.ExitSuccess || len(first.Indexed) != 3 || len(first.Skipped) != 0 || len(first.Errors) != 0 ||
		first.Chain == nil || first.Chain.State != rxtypes.ChainStateReady || first.Path != first.Chain.Path {
		t.Fatalf("exit %d, %+v, %s", code, first, stderr)
	}
	if first.Chain.CLICommand != "rx logs show "+first.Path {
		t.Fatalf("chain cli_command %q", first.Chain.CLICommand)
	}
	builtAt := func(a logsIndexAnswer) map[string]any {
		out := map[string]any{}
		for _, entry := range a.Indexed {
			out[entry["path"].(string)] = entry["created_at"]
		}
		return out
	}
	_, kept, _ := indexJSON(t, dir, cache, "app.log")
	_, forced, _ := indexJSON(t, dir, cache, "app.log", "--force")
	for path, at := range builtAt(first) {
		if builtAt(kept)[path] != at {
			t.Errorf("%s was built again without --force", path)
		}
		if builtAt(forced)[path] == at {
			t.Errorf("%s was not built again with --force", path)
		}
	}
}

// The exit codes are those of `rx index`, and 3 when a handle names no
// chain. An invalid chain is indexed and printed, and exits 0; a part
// that cannot be read exits 4 after the others are indexed.
func TestLogsIndex_ExitCodes(t *testing.T) {
	dir := chainFixture(t)
	cases := []struct {
		label string
		args  []string
		want  int
		says  string
	}{
		{"a lone file", []string{"notes.txt"}, clicommand.ExitFileNotFound, "Not a log chain"},
		{"an invalid chain", []string{"bad.log"}, clicommand.ExitSuccess, ""},
		{"outside the search root", []string{"--search-root=" + t.TempDir(), filepath.Join(dir, "app.log")},
			clicommand.ExitAccessDenied, "outside all search roots"},
		{"no chain among two", []string{"app.log", "notes.txt"}, clicommand.ExitFileNotFound, "no log chain"},
	}
	for _, tc := range cases {
		code, _, stderr := runRxIn(t, dir, nil, append([]string{"logs", "index"}, tc.args...)...)
		if code != tc.want || !strings.Contains(stderr, tc.says) {
			t.Errorf("%s: exit %d, stderr %q; want %d with %q", tc.label, code, stderr, tc.want, tc.says)
		}
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	unreadable := filepath.Join(dir, "app.log.1")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	code, answer, stderr := indexJSON(t, dir, t.TempDir(), "app.log")
	if code != clicommand.ExitAccessDenied || len(answer.Indexed) != 2 || len(answer.Errors) != 1 ||
		answer.Chain == nil || answer.Chain.State != rxtypes.ChainStateInvalid {
		t.Fatalf("an unreadable part: exit %d, %+v, stderr %s", code, answer, stderr)
	}
}
