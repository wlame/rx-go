package clicommand

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A named pipe is never opened: a read of one waits for a writer that
// may never come. Named on the command line it fails every command at
// once with exit code 2 and "not a regular file"; met by a walk it is
// skipped with that reason and the rest of the tree is searched.
func TestNamedPipeIsRefusedWithoutBlocking(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	writeInput(t, dir, "app.log", []byte("LINE 1 NEEDLE\n"))
	pipe := filepath.Join(dir, "pipe.log")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	zero := 0
	named := map[string]func(*bytes.Buffer) error{
		"trace": func(out *bytes.Buffer) error {
			cmd := NewTraceCommand(out)
			cmd.SetArgs([]string{"NEEDLE", pipe})
			return cmd.Execute()
		},
		"samples": func(out *bytes.Buffer) error {
			return runSamples(out, samplesParams{path: pipe, lines: []string{"1"}})
		},
		"index": func(out *bytes.Buffer) error {
			return runIndex(out, indexParams{paths: []string{pipe}, threshold: &zero, jsonOutput: true})
		},
	}
	for name, run := range named {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := withinBudget(t, func() error { return run(&out) })
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
				t.Fatalf("got %v, want exit code %d", err, ExitUsageError)
			}
			if said := exitErr.Error() + out.String(); !strings.Contains(said, "not a regular file") {
				t.Errorf("the output does not say why:\n%s", said)
			}
		})
	}

	var out bytes.Buffer
	err := withinBudget(t, func() error {
		cmd := NewTraceCommand(&out)
		cmd.SetArgs([]string{"NEEDLE", dir, "--json"})
		return cmd.Execute()
	})
	if err != nil {
		t.Fatalf("trace of the directory: %v", err)
	}
	if !strings.Contains(out.String(), `"reason": "not a regular file"`) || !strings.Contains(out.String(), "LINE 1 NEEDLE") {
		t.Errorf("the walk did not skip the pipe with its reason and search the rest:\n%s", out.String())
	}
}

// withinBudget runs f and fails the test when it has not returned after
// a few seconds, the sign of a read waiting on a named pipe.
func withinBudget(t *testing.T, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("blocked on a named pipe")
		return nil
	}
}
