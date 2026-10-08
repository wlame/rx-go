package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/clicommand"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// indexedChainFixture is chainFixture with the parts of both its
// chains indexed by rx logs index, in a cache directory of its own; it
// returns the directory and the environment that names that cache.
func indexedChainFixture(t *testing.T) (string, []string) {
	t.Helper()
	dir := chainFixture(t)
	env := []string{"RX_CACHE_DIR=" + t.TempDir()}
	if code, _, stderr := runRxIn(t, dir, env, "logs", "index", "app.log", "bad.log"); code != clicommand.ExitSuccess {
		t.Fatalf("rx logs index: exit %d: %s", code, stderr)
	}
	return dir, env
}

// The human output: the trace header, then one row per match, a part's
// as CHAIN:LINE (PART:LINE): TEXT with its line in the chain first, a
// file's of its own as FILE:LINE: TEXT. The chains' parts are indexed,
// so app.log is ready and every row of it has its chain line. The
// invalid chain of the directory is named on stderr.
func TestLogsTrace_HumanOutput(t *testing.T) {
	dir, env := indexedChainFixture(t)
	code, stdout, stderr := runRxIn(t, dir, env, "logs", "trace", `LINE 1[01]? `, "app.log", "notes.txt", "bad.log")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var rows []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "  ") {
			rows = append(rows, line)
		}
	}
	want := []string{
		"  app.log:1 (app.log.2.gz:1): 2026-10-01 00:00:00.000 LINE 1 app.log.2.gz",
		"  app.log:10 (app.log.2.gz:10): 2026-10-01 00:00:09.000 LINE 10 app.log.2.gz",
		"  app.log:11 (app.log.1:1): 2026-10-01 01:00:00.000 LINE 11 app.log.1",
		"  notes.txt:1: LINE 1 notes",
		"  bad.log:? (bad.log:1): 2026-10-01 00:00:00.000 LINE 1 bad.log",
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("rows\n%s\nwant\n%s\nstdout\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"), stdout)
	}
	if !strings.Contains(stdout, "Matches (chain:line (part:line), or file:line):") {
		t.Fatalf("stdout\n%s", stdout)
	}
	if !strings.Contains(stderr, "Warning: the log chain bad.log is invalid (no_timestamps)") {
		t.Fatalf("stderr %q", stderr)
	}
}

// --json prints the GET /v1/logs/trace body: chains by id, a chain_line
// on each match of a part, none on a file's of its own.
func TestLogsTrace_JSON(t *testing.T) {
	dir, env := indexedChainFixture(t)
	code, stdout, stderr := runRxIn(t, dir, env, "logs", "trace", "--json", `LINE 1[01]? `, ".")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var resp rxtypes.ChainTraceResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	app, bad := resp.Chains["c1"], resp.Chains["c2"]
	if app.Name != "app.log" || app.State != rxtypes.ChainStateReady || len(app.Parts) != 3 ||
		bad.Name != "bad.log" || bad.State != rxtypes.ChainStateInvalid || len(resp.Chains) != 2 {
		t.Fatalf("chains %+v", resp.Chains)
	}
	var lines []int64
	for _, m := range resp.Matches {
		lines = append(lines, m.ChainLine)
	}
	// app.log's three matches, then bad.log's (invalid), then notes.txt.
	if !slices.Equal(lines, []int64{1, 10, 11, -1, -1}) || resp.Matches[4].Chain != nil {
		t.Fatalf("chain lines %v, last match %+v", lines, resp.Matches[4])
	}
	if len(resp.ScannedFiles) == 0 || resp.CLICommand != nil {
		t.Fatalf("scanned_files %v, cli_command %v", resp.ScannedFiles, resp.CLICommand)
	}
}

// The exit codes: 3 for a path that is no directory, chain or file, 2
// for a pattern that does not compile and for standard input, 4 outside
// the search roots.
func TestLogsTrace_ExitCodes(t *testing.T) {
	dir := chainFixture(t)
	cases := []struct {
		label string
		env   []string
		args  []string
		want  int
	}{
		{"a missing path", nil, []string{"LINE", "nope.log"}, clicommand.ExitFileNotFound},
		{"a pattern that does not compile", nil, []string{"(unclosed", "app.log"}, clicommand.ExitUsageError},
		{"standard input", nil, []string{"LINE", "-"}, clicommand.ExitUsageError},
		{"outside the search roots", []string{"RX_SEARCH_ROOTS=" + dir}, []string{"LINE", "/etc"}, clicommand.ExitAccessDenied},
	}
	for _, tc := range cases {
		code, _, stderr := runRxIn(t, dir, tc.env, append([]string{"logs", "trace"}, tc.args...)...)
		if code != tc.want {
			t.Errorf("%s: exit %d, want %d: %s", tc.label, code, tc.want, stderr)
		}
	}
}

// A capped search of a chain whose parts have no line index describes
// the chain from what is stored, never by reading a part: the chain is
// pending, each match has chain_line -1 (`?` in the rows), the exit
// code is 0, and stderr says how to get the chain lines. After
// rx logs index the same search gives them, and stderr says nothing
// more.
func TestLogsTrace_AnUnindexedChainIsPendingWithAHint(t *testing.T) {
	dir := chainFixture(t)
	env := []string{"RX_CACHE_DIR=" + t.TempDir()}
	search := func(args ...string) (string, string) {
		t.Helper()
		code, stdout, stderr := runRxIn(t, dir, env, append([]string{"logs", "trace", "--max-results=5"}, args...)...)
		if code != clicommand.ExitSuccess {
			t.Fatalf("rx logs trace %v: exit %d: %s", args, code, stderr)
		}
		return stdout, stderr
	}
	chainLines := func(stdout string) (string, []int64) {
		t.Helper()
		var resp rxtypes.ChainTraceResponse
		if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
			t.Fatalf("decode %q: %v", stdout, err)
		}
		var lines []int64
		for _, m := range resp.Matches {
			lines = append(lines, m.ChainLine)
		}
		return resp.Chains["c1"].State, lines
	}
	const hint = "Hint: the log chain app.log is pending (a part has no line index), so its matches have no line " +
		"in the chain: run rx logs index -- app.log for chain line numbers.\n"

	stdout, stderr := search("--json", "LINE", "app.log")
	if state, lines := chainLines(stdout); state != rxtypes.ChainStatePending || !slices.Equal(lines, []int64{-1, -1, -1, -1, -1}) {
		t.Fatalf("cold: state %s, chain lines %v", state, lines)
	}
	if stderr != hint {
		t.Fatalf("cold stderr %q, want %q", stderr, hint)
	}
	stdout, _ = search("LINE", "app.log")
	if !strings.Contains(stdout, "  app.log:? (app.log.2.gz:1): ") {
		t.Fatalf("cold rows\n%s", stdout)
	}

	if code, _, stderr := runRxIn(t, dir, env, "logs", "index", "app.log"); code != clicommand.ExitSuccess {
		t.Fatalf("rx logs index: exit %d: %s", code, stderr)
	}
	stdout, stderr = search("--json", "LINE", "app.log")
	if state, lines := chainLines(stdout); state != rxtypes.ChainStateReady || !slices.Equal(lines, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("indexed: state %s, chain lines %v", state, lines)
	}
	if stderr != "" {
		t.Fatalf("indexed stderr %q", stderr)
	}
}

// A handle that starts with a dash: the hint ends the options with --
// before the handle, so the command it names runs as printed, and
// after it the same search numbers every match in the chain.
func TestLogsTrace_TheHintRunsForAHandleThatStartsWithADash(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	files := map[string][]byte{
		"-x.log.1": timedChainLines(start, 1, 3, "-x.log.1"),
		"-x.log":   timedChainLines(start.Add(time.Hour), 4, 2, "-x.log"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"RX_CACHE_DIR=" + t.TempDir()}
	search := func() (rxtypes.ChainTraceResponse, string) {
		t.Helper()
		code, stdout, stderr := runRxIn(t, dir, env, "logs", "trace", "--json", "LINE", "--", "-x.log")
		if code != clicommand.ExitSuccess {
			t.Fatalf("rx logs trace: exit %d: %s", code, stderr)
		}
		var resp rxtypes.ChainTraceResponse
		if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
			t.Fatalf("decode %q: %v", stdout, err)
		}
		return resp, stderr
	}

	resp, stderr := search()
	const want = "Hint: the log chain -x.log is pending (a part has no line index), so its matches have no line " +
		"in the chain: run rx logs index -- -x.log for chain line numbers.\n"
	if resp.Chains["c1"].State != rxtypes.ChainStatePending || stderr != want {
		t.Fatalf("cold: state %s, stderr %q, want %q", resp.Chains["c1"].State, stderr, want)
	}

	// The command between "run " and " for" as a shell splits it: no
	// argument in it needs quotes.
	command := strings.Fields(stderr[strings.Index(stderr, "run rx ")+len("run rx ") : strings.Index(stderr, " for chain")])
	if code, _, out := runRxIn(t, dir, env, command...); code != clicommand.ExitSuccess {
		t.Fatalf("the hinted rx %v: exit %d: %s", command, code, out)
	}
	resp, stderr = search()
	var lines []int64
	for _, m := range resp.Matches {
		lines = append(lines, m.ChainLine)
	}
	if resp.Chains["c1"].State != rxtypes.ChainStateReady || !slices.Equal(lines, []int64{1, 2, 3, 4, 5}) || stderr != "" {
		t.Fatalf("after the hinted command: state %s, chain lines %v, stderr %q", resp.Chains["c1"].State, lines, stderr)
	}
}
