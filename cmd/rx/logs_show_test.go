package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/clicommand"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// timedChainLines is n lines `<time> LINE <global> <part>`, a second
// apart from start.
func timedChainLines(start time.Time, firstGlobal, n int, part string) []byte {
	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		fmt.Fprintf(&buf, "%s LINE %d %s\n", at.Format("2006-01-02 15:04:05.000"), firstGlobal+i, part)
	}
	return buf.Bytes()
}

// chainFixture writes a directory with a ready chain `app.log` of three
// parts (one gzipped, none indexed), an invalid chain `bad.log` whose
// frozen part has no timestamps, and a lone file.
func chainFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = w.Write(timedChainLines(start, 1, 10, "app.log.2.gz"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"app.log.2.gz": compressed.Bytes(),
		"app.log.1":    timedChainLines(start.Add(time.Hour), 11, 5, "app.log.1"),
		"app.log":      timedChainLines(start.Add(2*time.Hour), 16, 3, "app.log"),
		"bad.log.1":    []byte("no time here\n"),
		"bad.log":      timedChainLines(start, 1, 2, "bad.log"),
		"notes.txt":    []byte("LINE 1 notes\n"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// showJSON runs `rx logs show <handle> --json` in dir and decodes it.
func showJSON(t *testing.T, dir string, args ...string) (int, rxtypes.ChainResponse, string) {
	t.Helper()
	code, stdout, stderr := runRxIn(t, dir, nil, append(append([]string{"logs", "show"}, args...), "--json")...)
	var resp rxtypes.ChainResponse
	if stdout != "" {
		if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
			t.Fatalf("decode %q: %v", stdout, err)
		}
	}
	return code, resp, stderr
}

// The human output: a line with the handle, the state, the counts, the
// fingerprint and the zone; then the parts in order, with their global
// lines.
func TestLogsShow_HumanOutput(t *testing.T) {
	dir := chainFixture(t)
	code, stdout, stderr := runRxIn(t, dir, nil, "logs", "show", "app.log")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], "app.log: ready, 3 parts, 18 lines, fingerprint ") ||
		!strings.HasSuffix(lines[0], ", times in UTC") {
		t.Fatalf("stdout\n%s", stdout)
	}
	if strings.Join(strings.Fields(lines[1]), " ") != "# NAME COMPRESSION LINES GLOBAL LINES FIRST TIME HIGHEST TIME IDX" {
		t.Fatalf("header %q", lines[1])
	}
	want := [][]string{
		{"1", "app.log.2.gz", "gzip", "10", "1-10", "2026-10-01", "00:00:00.000", "2026-10-01", "00:00:09.000", "-"},
		{"2", "app.log.1", "-", "5", "11-15", "2026-10-01", "01:00:00.000", "2026-10-01", "01:00:04.000", "-"},
		{"3", "app.log", "-", "3", "16-18", "2026-10-01", "02:00:00.000", "2026-10-01", "02:00:02.000", "-"},
	}
	for i, row := range want {
		if got := strings.Fields(lines[2+i]); strings.Join(got, " ") != strings.Join(row, " ") {
			t.Fatalf("row %d %q, want %q", i+1, lines[2+i], strings.Join(row, " "))
		}
	}
}

// --json gives the description GET /v1/logs/chain gives, one object for
// one chain and an array for several, with the command that gives it.
func TestLogsShow_JSON(t *testing.T) {
	dir := chainFixture(t)
	code, resp, stderr := showJSON(t, dir, "app.log")
	if code != clicommand.ExitSuccess || resp.State != rxtypes.ChainStateReady || len(resp.Parts) != 3 ||
		resp.LineCount == nil || *resp.LineCount != 18 || *resp.Parts[2].GlobalStart != 16 {
		t.Fatalf("exit %d, %+v, %s", code, resp, stderr)
	}
	if resp.CLICommand != "rx logs show "+resp.Path {
		t.Fatalf("cli_command %q", resp.CLICommand)
	}
	tokyo, _, _ := showJSON(t, dir, "app.log", "--file-tz=Asia/Tokyo")
	if tokyo != clicommand.ExitSuccess {
		t.Fatalf("--file-tz: exit %d", tokyo)
	}

	code, stdout, stderr := runRxIn(t, dir, nil, "logs", "show", "app.log", "bad.log", "--json")
	var several []rxtypes.ChainResponse
	if json.Unmarshal([]byte(stdout), &several) != nil || len(several) != 2 || several[1].State != rxtypes.ChainStateInvalid {
		t.Fatalf("two chains: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	if code != clicommand.ExitChainInvalid {
		t.Fatalf("an invalid chain among two: exit %d", code)
	}
}

// The exit codes: 3 when a handle names no chain, 6 for an invalid chain
// and 7 for a fingerprint that differs (both after the chain is
// printed), 2 for a malformed fingerprint or one given with several
// chains, 4 outside the search roots.
func TestLogsShow_ExitCodes(t *testing.T) {
	dir := chainFixture(t)
	_, ready, _ := showJSON(t, dir, "app.log")
	cases := []struct {
		label   string
		args    []string
		want    int
		says    string
		printed bool
	}{
		{"a lone file", []string{"notes.txt"}, clicommand.ExitFileNotFound, "Not a log chain", false},
		{"a missing directory", []string{filepath.Join(dir, "absent", "x.log")}, clicommand.ExitFileNotFound, "Directory not found", false},
		{"an invalid chain", []string{"bad.log"}, clicommand.ExitChainInvalid, "no_timestamps", true},
		{"a changed fingerprint", []string{"app.log", "--fingerprint=0000000000000000"}, clicommand.ExitChainChanged, "changed", true},
		{"the same fingerprint", []string{"app.log", "--fingerprint=" + ready.Fingerprint}, clicommand.ExitSuccess, "", true},
		{"a malformed fingerprint", []string{"app.log", "--fingerprint=xyz"}, clicommand.ExitUsageError, "fingerprint", false},
		{"a fingerprint for two chains", []string{"app.log", "bad.log", "--fingerprint=" + ready.Fingerprint}, clicommand.ExitUsageError, "one CHAIN", false},
		{"outside the search root", []string{"--search-root=" + t.TempDir(), filepath.Join(dir, "app.log")}, clicommand.ExitAccessDenied, "outside all search roots", false},
		{"a handle ending in a separator", []string{dir + "/"}, clicommand.ExitUsageError, "chain handle", false},
	}
	for _, tc := range cases {
		code, stdout, stderr := runRxIn(t, dir, nil, append([]string{"logs", "show"}, tc.args...)...)
		if code != tc.want || !strings.Contains(stderr, tc.says) || (stdout != "") != tc.printed {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want %d with %q", tc.label, code, stdout, stderr, tc.want, tc.says)
		}
	}
}

// `rx logs time-range` shows the chain's first and last time as
// `rx time-range` shows a file's, and --json gives them as instants.
func TestLogsTimeRange(t *testing.T) {
	dir := chainFixture(t)
	code, stdout, stderr := runRxIn(t, dir, nil, "logs", "time-range", "app.log")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	fields := strings.Split(strings.TrimSuffix(stdout, "\n"), "  ")
	if len(fields) != 5 || !strings.HasSuffix(fields[0], "app.log") || fields[1] != "iso" ||
		fields[2] != "2026-10-01 00:00:00.000 .. 2026-10-01 02:00:02.000" || fields[3] != "UTC" || fields[4] != "ready" {
		t.Fatalf("line %q", stdout)
	}
	code, stdout, stderr = runRxIn(t, dir, nil, "logs", "time-range", "app.log", "--json", "--file-tz=+02:00")
	var r rxtypes.ChainTimeRange
	if code != 0 || json.Unmarshal([]byte(stdout), &r) != nil {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("", 2*3600)).UnixMilli()
	if r.State != rxtypes.ChainStateReady || r.FirstMs == nil || *r.FirstMs != first || r.Format == nil || *r.Format != "iso" ||
		r.DisplayZone != "+02:00" || r.CLICommand != "rx logs time-range "+r.Path+" --file-tz=+02:00" {
		t.Fatalf("answer %+v", r)
	}
	code, stdout, _ = runRxIn(t, dir, nil, "logs", "time-range", "bad.log")
	if code != clicommand.ExitChainInvalid || !strings.Contains(stdout, "invalid") {
		t.Fatalf("an invalid chain: exit %d, stdout %q", code, stdout)
	}
}
