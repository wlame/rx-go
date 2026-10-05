package clicommand

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// unreadableJournalReason is how every command names a journal whose
// header holds an incompatible flag rx does not know.
const unreadableJournalReason = "not a text file: a systemd journal rx cannot read: incompatible flags"

// unreadableJournal writes a journal with an incompatible flag rx does
// not know, named system.journal in a new directory, and returns its
// path.
func unreadableJournal(t *testing.T) string {
	t.Helper()
	path := journalfixture.Journal(t, "basic-none-regular")
	journalfixture.AddUnknownIncompatibleFlag(t, path)
	return path
}

// A journal rx cannot read is refused by every command with the
// journal's reason: searched, sampled, indexed, scanned for signatures,
// asked for its time range or compressed, it gives no text and says why.
func TestAJournalRxCannotReadIsRefusedByEveryCommandWithItsReason(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := unreadableJournal(t)
	zero := 0
	commands := map[string]func(*bytes.Buffer) error{
		"trace": func(out *bytes.Buffer) error {
			cmd := NewTraceCommand(out)
			cmd.SetArgs([]string{"service", path, "--json"})
			return cmd.Execute()
		},
		"trace --signatures": func(out *bytes.Buffer) error {
			cmd := NewTraceCommand(out)
			cmd.SetArgs([]string{"--signatures=default", path, "--json"})
			return cmd.Execute()
		},
		"samples": func(out *bytes.Buffer) error {
			return runSamples(out, samplesParams{path: path, lines: []string{"1"}})
		},
		"index": func(out *bytes.Buffer) error {
			return runIndex(out, indexParams{paths: []string{path}, threshold: &zero, jsonOutput: true})
		},
		"time-range": func(out *bytes.Buffer) error {
			return runTimeRange(out, []string{path}, true, config.Zone{})
		},
		"compress": func(out *bytes.Buffer) error {
			return runCompress(context.Background(), out, compressParams{
				paths: []string{path}, output: filepath.Join(filepath.Dir(path), "out.zst"),
				frameSize: "64K", level: 3, workers: 1, jsonOutput: true,
			})
		},
		"scan": func(out *bytes.Buffer) error {
			return runScan(context.Background(), out, scanParams{paths: []string{path}, jsonOutput: true})
		},
	}
	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			// A command reports a refused path in its answer, in its error
			// or on stderr (time-range answers the other paths first).
			stderr := captureStderr(t)
			var out bytes.Buffer
			err := run(&out)
			said := out.String() + "\n" + stderr()
			if err != nil {
				said += "\n" + err.Error()
			}
			if !strings.Contains(said, unreadableJournalReason) {
				t.Fatalf("%s does not refuse the journal with its reason; it said:\n%s", name, said)
			}
		})
	}
}
