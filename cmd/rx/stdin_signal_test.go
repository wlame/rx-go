package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// While rx trace reads piped standard input, an interrupt ends it at
// once: exit 5, no answer printed, and the spool file removed. The
// pipe is held open and never written, so without the fix rx would wait
// on it for ever.
func TestExitCode_InterruptWhileSpoolingStdinIsFive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal delivery differs on Windows")
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			tmp := t.TempDir()
			cmd := rxCommand(t, []string{"TMPDIR=" + tmp}, "trace", "-e", "x", "-", "--json")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatalf("stdin pipe: %v", err)
			}
			defer func() { _ = stdin.Close() }()
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitSpoolFile(t, tmp)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatalf("signal: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				var exitErr *exec.ExitError
				if !asExitError(err, &exitErr) || exitErr.ExitCode() != 5 {
					t.Errorf("exit: got %v, want exit code 5 (stderr %q)", err, stderr.String())
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("rx did not stop within 10 s of the signal: it kept reading standard input")
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want no answer after an interrupt", stdout.String())
			}
			if left, _ := filepath.Glob(filepath.Join(tmp, "rx_stdin_*")); len(left) != 0 {
				t.Errorf("spool files left behind: %v", left)
			}
		})
	}
}

// awaitSpoolFile waits until rx has created its spool file in dir, so
// the signal lands while the copy from standard input is under way.
func awaitSpoolFile(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if found, _ := filepath.Glob(filepath.Join(dir, "rx_stdin_*")); len(found) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("rx created no spool file within 10 s")
}
