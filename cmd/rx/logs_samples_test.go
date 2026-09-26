package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/clicommand"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// logsSamplesJSON runs `rx logs samples <args> --json` in dir and
// decodes it.
func logsSamplesJSON(t *testing.T, dir string, args ...string) (int, rxtypes.ChainSamplesResponse, string) {
	t.Helper()
	code, stdout, stderr := runRxIn(t, dir, nil, append(append([]string{"logs", "samples"}, args...), "--json")...)
	var resp rxtypes.ChainSamplesResponse
	if stdout != "" {
		if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
			t.Fatalf("decode %q: %v", stdout, err)
		}
	}
	return code, resp, stderr
}

// The human output: the chain, its times and the context, then a block
// per key whose lines carry their global number and their part and
// local number, with a `-- NAME --` line where a part's lines start.
func TestLogsSamples_HumanOutput(t *testing.T) {
	dir := chainFixture(t)
	code, stdout, stderr := runRxIn(t, dir, nil, "logs", "samples", "app.log", "--lines=10", "--context=1")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 10 || !strings.HasPrefix(lines[0], "Chain: ") || !strings.Contains(lines[0], "app.log  ready  3 parts  fingerprint ") {
		t.Fatalf("stdout\n%s", stdout)
	}
	want := []string{
		"Times: 2026-10-01 00:00:00.000 .. 2026-10-01 02:00:02.000  UTC",
		"Context: 1 before, 1 after",
		"",
		"-- app.log.2.gz --",
		" 9  app.log.2.gz:9  2026-10-01 00:00:08.000 LINE 9 app.log.2.gz",
		"10  app.log.2.gz:10  2026-10-01 00:00:09.000 LINE 10 app.log.2.gz",
		"-- app.log.1 --",
		"11  app.log.1:1  2026-10-01 01:00:00.000 LINE 11 app.log.1",
	}
	got := append(append([]string{}, lines[1:4]...), lines[5:]...)
	if !slices.Equal(got, want) || !strings.HasPrefix(lines[4], "=== ") || !strings.HasSuffix(lines[4], "app.log:10 ===") {
		t.Fatalf("stdout\n%s\nwant\n%s", stdout, strings.Join(want, "\n"))
	}
}

// Each piece's cli_command, run as it stands, gives the piece's lines:
// the part's own numbers of the piece, from the part alone.
func TestLogsSamples_PieceCommandsGiveThePiecesLines(t *testing.T) {
	dir := chainFixture(t)
	code, resp, stderr := logsSamplesJSON(t, dir, "app.log", "--lines=8-17,-1", "--context=2")
	if code != clicommand.ExitSuccess || len(resp.Samples["8-17"]) != 3 {
		t.Fatalf("exit %d: %+v %s", code, resp, stderr)
	}
	for key, pieces := range resp.Samples {
		for _, p := range pieces {
			words := shellWords(t, p.CLICommand)
			code, stdout, stderr := runRxIn(t, dir, nil, append(words[1:], "--json")...)
			var single rxtypes.SamplesResponse
			if code != clicommand.ExitSuccess || json.Unmarshal([]byte(stdout), &single) != nil {
				t.Fatalf("%s: exit %d: %s", p.CLICommand, code, stderr)
			}
			rangeKey := fmt.Sprintf("%d-%d", p.FirstLocalLine, p.FirstLocalLine+int64(len(p.Lines))-1)
			if !slices.Equal(single.Samples[rangeKey], p.Lines) {
				t.Fatalf("key %s: %q gives %q, the piece holds %q", key, p.CLICommand, single.Samples[rangeKey], p.Lines)
			}
		}
	}
	if resp.CLICommand != "rx logs samples "+resp.Path+" --lines=8-17,-1 --context=2" {
		t.Fatalf("cli_command %q", resp.CLICommand)
	}
}

// Times: the line at T across the parts, and a range across three parts.
func TestLogsSamples_Times(t *testing.T) {
	dir := chainFixture(t)
	code, resp, stderr := logsSamplesJSON(t, dir, "app.log", "--timestamps=2026-10-01 00:30",
		"--timestamps=2026-10-01 00:00:08..2026-10-01 02:00:00", "--context=0")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if resp.Timestamps["2026-10-01 00:30"] != 11 || resp.Timestamps["2026-10-01 00:00:08..2026-10-01 02:00:00"] != 9 {
		t.Fatalf("timestamps %v", resp.Timestamps)
	}
	if pieces := resp.Samples["2026-10-01 00:00:08..2026-10-01 02:00:00"]; len(pieces) != 3 || len(pieces[2].Lines) != 1 {
		t.Fatalf("the range's pieces %+v", pieces)
	}
}

// The exit codes: 3 when the handle names no chain, 6 for an invalid
// chain, 7 for a fingerprint that differs, 2 for a part that is not a
// member and for the usage errors.
func TestLogsSamples_ExitCodes(t *testing.T) {
	dir := chainFixture(t)
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"notes.txt", "--lines=1"}, clicommand.ExitFileNotFound},
		{[]string{"bad.log", "--lines=1"}, clicommand.ExitChainInvalid},
		{[]string{"app.log", "--lines=1", "--fingerprint=0000000000000000"}, clicommand.ExitChainChanged},
		{[]string{"app.log", "--lines=1", "--part=app.log.9"}, clicommand.ExitUsageError},
		{[]string{"app.log", "--lines=1", "--timestamps=2026-10-01"}, clicommand.ExitUsageError},
		{[]string{"app.log", "--part=app.log.1"}, clicommand.ExitUsageError},
		{[]string{"app.log"}, clicommand.ExitUsageError},
		{[]string{"app.log", "--timestamps=soon"}, clicommand.ExitUsageError},
		{[]string{"app.log", "--lines=1", "--fingerprint=xyz"}, clicommand.ExitUsageError},
	}
	for _, tc := range cases {
		code, stdout, stderr := runRxIn(t, dir, nil, append([]string{"logs", "samples"}, tc.args...)...)
		if code != tc.want {
			t.Fatalf("%v: exit %d, want %d; stdout %q stderr %q", tc.args, code, tc.want, stdout, stderr)
		}
	}
}
