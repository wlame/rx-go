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

// requireUnprivileged skips when the test runs as a user that can read
// a file with no permission bits, which makes the case untestable.
func requireUnprivileged(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	if f, err := os.Open(path); err == nil {
		_ = f.Close()
		t.Skip("the unreadable fixture is readable here")
	}
}

// TestTraceNamedFileThatCannotBeReadFailsWithAccessDenied pins the exit
// code for a file the caller named and nobody can read.
//
// The engine lists such a file as skipped, which is right inside a
// directory scan. Named on the command line it used to produce "Files
// skipped: 1", zero matches and exit 0 — a search that reported success
// without reading anything.
func TestTraceNamedFileThatCannotBeReadFailsWithAccessDenied(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.log")
	if err := os.WriteFile(path, []byte("error here\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod fixture: %v", err)
	}
	requireUnprivileged(t, path)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", path})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected an error, got output:\n%s", buf.String())
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error %v is not an ExitError", err)
	}
	if exitErr.Code != ExitAccessDenied {
		t.Errorf("exit code %d, want %d (access denied)", exitErr.Code, ExitAccessDenied)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the message does not name the file: %v", err)
	}
}

// TestTraceUnreadableFileInsideADirectoryIsSkipped keeps the other half
// of the rule: one unreadable file must not fail a directory scan.
func TestTraceUnreadableFileInsideADirectoryIsSkipped(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	readable := filepath.Join(dir, "open.log")
	if err := os.WriteFile(readable, []byte("error here\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	locked := filepath.Join(dir, "locked.log")
	if err := os.WriteFile(locked, []byte("error there\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod fixture: %v", err)
	}
	requireUnprivileged(t, locked)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("directory scan failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Matches: 1") {
		t.Errorf("expected the readable file's match, got:\n%s", out)
	}
	if !strings.Contains(out, "Files skipped: 1\n  "+locked+": permission denied\n") {
		t.Errorf("expected the unreadable file to be reported as skipped with its reason, got:\n%s", out)
	}
}
