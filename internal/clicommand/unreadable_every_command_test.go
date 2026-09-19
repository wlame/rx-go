package clicommand

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lockedFile writes a log in a new directory and takes every permission
// away from it, skipping the test where that does not stop a read.
func lockedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "locked.log")
	if err := os.WriteFile(path, []byte("LINE 1 NEEDLE\nLINE 2\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	requireUnprivileged(t, path)
	return path
}

// A file the user names and the process may not read fails every
// command the same way: exit code 4 and a message that says "permission
// denied" and names the file. `rx index` used to skip it as "not a text
// file" and exit 0, and `rx samples` exited 1.
func TestNamedUnreadableFileFailsEveryCommandAlike(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := lockedFile(t)
	zero := 0
	commands := map[string]func(*bytes.Buffer) error{
		"trace": func(out *bytes.Buffer) error {
			cmd := NewTraceCommand(out)
			cmd.SetArgs([]string{"NEEDLE", path})
			return cmd.Execute()
		},
		"samples": func(out *bytes.Buffer) error {
			return runSamples(out, samplesParams{path: path, lines: []string{"1"}})
		},
		"index": func(out *bytes.Buffer) error {
			return runIndex(out, indexParams{paths: []string{path}, threshold: &zero, jsonOutput: true})
		},
	}
	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(&out)
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != ExitAccessDenied {
				t.Fatalf("got %v, want exit code %d", err, ExitAccessDenied)
			}
			// trace and samples say it in their error; index in the
			// errors list of its answer.
			said := strings.ToLower(exitErr.Error() + out.String())
			if !strings.Contains(said, "permission denied") || !strings.Contains(said, strings.ToLower(path)) {
				t.Errorf("the output does not say permission denied for %s:\n%s", path, said)
			}
		})
	}
}

// A file or subdirectory a directory walk meets and may not read is
// skipped with the reason "permission denied", and the rest of the tree
// is indexed: `rx index -r` agrees with `rx trace` of the same tree
// rather than fail the whole directory.
func TestIndexDirectorySkipsWhatItMayNotRead(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	writeInput(t, dir, "app.log", []byte("LINE 1\n"))
	locked := writeInput(t, dir, "locked.log", []byte("LINE 1\n"))
	closed := filepath.Join(dir, "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeInput(t, closed, "inner.log", []byte("LINE 1\n"))
	for _, p := range []string{locked, closed} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
	}
	requireUnprivileged(t, locked)

	zero := 0
	result := runIndexJSON(t, indexParams{paths: []string{dir}, recursive: true, threshold: &zero})

	indexed, _ := result["indexed"].([]any)
	if len(indexed) != 1 {
		t.Errorf("indexed: got %v, want app.log alone", indexed)
	}
	reasons, _ := result["skip_reasons"].([]any)
	assertSkipReasons(t, reasons, map[string]string{locked: "permission denied", closed: "permission denied"})
	if errs, _ := result["errors"].([]any); len(errs) != 0 {
		t.Errorf("errors: %v, want none", errs)
	}
}
