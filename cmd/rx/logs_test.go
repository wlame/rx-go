package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/clicommand"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// logsFixture writes a directory with two chains (one named with
// spaces and brackets, one numbered with a hole) and a lone file.
func logsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = w.Write([]byte("LINE 1 syslog.2\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"syslog":              []byte("LINE 1 syslog\n"),
		"syslog.1":            []byte("LINE 1 syslog.1\n"),
		"syslog.2.gz":         compressed.Bytes(),
		"syslog.4":            []byte("LINE 1 syslog.4\n"),
		"my app (1).log.1":    []byte("LINE 1 a\n"),
		"my app (1).log.2.gz": compressed.Bytes(),
		"notes.txt":           []byte("LINE 1 notes\n"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The human table: the directory, then one row per chain.
func TestLogsList_HumanTable(t *testing.T) {
	dir := logsFixture(t)
	code, stdout, stderr := runRxIn(t, dir, nil, "logs", "list")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 4 || !strings.HasSuffix(lines[0], ": 2 chains") {
		t.Fatalf("stdout\n%s", stdout)
	}
	if strings.Join(strings.Fields(lines[1]), " ") != "NAME PARTS SIZE IDX MISSING" {
		t.Fatalf("header %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "my app (1).log  ") || !strings.Contains(lines[2], " 2 ") {
		t.Fatalf("first row %q", lines[2])
	}
	if fields := strings.Fields(lines[3]); fields[0] != "syslog" || fields[1] != "4" || fields[len(fields)-1] != "syslog.3" ||
		fields[len(fields)-2] != "-" {
		t.Fatalf("second row %q", lines[3])
	}
}

// --json gives one ChainsResponse for one directory and an array for
// several; the default directory is the current one.
func TestLogsList_JSONShapes(t *testing.T) {
	dir := logsFixture(t)
	code, stdout, stderr := runRxIn(t, dir, nil, "logs", "list", "--json")
	var one rxtypes.ChainsResponse
	if code != 0 || json.Unmarshal([]byte(stdout), &one) != nil {
		t.Fatalf("one directory: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	if (one.Path != dir && one.Path != realDir) || len(one.Chains) != 2 {
		t.Fatalf("answer %+v", one)
	}
	app := one.Chains[0]
	if app.Name != "my app (1).log" || app.Path != filepath.Join(one.Path, "my app (1).log") ||
		!slices.Equal(app.Parts, []string{"my app (1).log.2.gz", "my app (1).log.1"}) {
		t.Fatalf("chain %+v", app)
	}

	other := t.TempDir()
	code, stdout, stderr = runRxIn(t, dir, nil, "logs", "list", dir, other, "--json")
	var several []rxtypes.ChainsResponse
	if code != 0 || json.Unmarshal([]byte(stdout), &several) != nil || len(several) != 2 ||
		several[0].Path != dir || several[1].Path != other || several[1].Chains == nil || len(several[1].Chains) != 0 {
		t.Fatalf("two directories: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
}

// The exit codes of a directory that cannot be listed: 3 when it does
// not exist, 2 for a file, 4 outside the search roots or unreadable; a
// run over several keeps answering the others.
func TestLogsList_ExitCodes(t *testing.T) {
	dir := logsFixture(t)
	cases := []struct {
		label string
		args  []string
		want  int
		says  string
	}{
		{"a missing directory", []string{"logs", "list", filepath.Join(dir, "absent")}, clicommand.ExitFileNotFound, "Directory not found"},
		{"a file", []string{"logs", "list", filepath.Join(dir, "notes.txt")}, clicommand.ExitUsageError, "Path is not a directory"},
		{"outside the search root", []string{"logs", "list", "--search-root=" + dir, t.TempDir()}, clicommand.ExitAccessDenied, "outside all search roots"},
		{"an unknown subcommand", []string{"logs", "frobnicate"}, clicommand.ExitUsageError, "Unknown command"},
	}
	for _, tc := range cases {
		code, _, stderr := runRxIn(t, dir, nil, tc.args...)
		if code != tc.want || !strings.Contains(stderr, tc.says) {
			t.Errorf("%s: exit %d, stderr %q; want %d with %q", tc.label, code, stderr, tc.want, tc.says)
		}
	}

	code, stdout, stderr := runRxIn(t, dir, nil, "logs", "list", filepath.Join(dir, "absent"), dir)
	if code != clicommand.ExitFileNotFound || !strings.Contains(stdout, ": 2 chains") ||
		!strings.Contains(stderr, "Directory not found") || !strings.Contains(stderr, "One or more directories do not exist") {
		t.Fatalf("several: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
		code, _, stderr = runRxIn(t, dir, nil, "logs", "list", locked)
		if code != clicommand.ExitAccessDenied || !strings.Contains(stderr, "Permission denied") {
			t.Fatalf("unreadable: exit %d, stderr %q", code, stderr)
		}
	}
}

// `logs` is a subcommand: a first argument "logs" is not a pattern.
func TestLogs_IsAKnownSubcommand(t *testing.T) {
	if got := preprocessArgs([]string{"logs", "list"}); !slices.Equal(got, []string{"logs", "list"}) {
		t.Fatalf("preprocessArgs = %v", got)
	}
	if got := preprocessArgs([]string{"--json", "logs", "list"}); got[0] == "trace" {
		t.Fatalf("preprocessArgs = %v", got)
	}
}

// The chain exit codes are part of the CLI contract.
func TestExitCodes_ChainCodes(t *testing.T) {
	if clicommand.ExitChainInvalid != 6 || clicommand.ExitChainChanged != 7 {
		t.Fatalf("chain exit codes %d %d", clicommand.ExitChainInvalid, clicommand.ExitChainChanged)
	}
}
