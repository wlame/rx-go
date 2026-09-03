package main

// Exit codes are part of the CLI contract: scripts branch on $?. These
// tests run the real binary, because the codes only exist once
// root.Execute's error reaches os.Exit — an in-process test of a RunE
// function cannot see them.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	buildOnce   sync.Once
	builtBinary string
	buildErr    error
)

// rxBinary builds cmd/rx once per test run and returns its path.
func rxBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rx-exitcode")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "rx")
		cmd := exec.Command("go", "build", "-o", out, ".")
		if output, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("build output: %s", output)
			return
		}
		builtBinary = out
	})
	if buildErr != nil {
		t.Fatalf("build rx: %v", buildErr)
	}
	return builtBinary
}

// runRx runs the built binary and returns its exit code, stdout and stderr.
func runRx(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(rxBinary(t), args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); !ok {
			t.Fatalf("run rx %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// writeLog puts a small searchable file in a fresh temp dir.
func writeLog(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("first line\nerror happened here\nlast line\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return dir, path
}

func TestExitCode_MissingFileIsThree(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.log")

	code, _, stderr := runRx(t, "samples", missing, "--lines=1")

	if code != 3 {
		t.Errorf("exit code: got %d, want 3 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "nope.log") {
		t.Errorf("stderr should name the file: %s", stderr)
	}
}

func TestExitCode_MissingTracePathIsThree(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.log")

	code, _, stderr := runRx(t, "trace", "error", missing)

	if code != 3 {
		t.Errorf("exit code: got %d, want 3 (stderr: %s)", code, stderr)
	}
}

func TestExitCode_InvalidRegexIsTwo(t *testing.T) {
	_, path := writeLog(t)

	for _, pattern := range []string{"a(", "[", "(?P<x"} {
		t.Run(pattern, func(t *testing.T) {
			code, stdout, stderr := runRx(t, "trace", pattern, path)

			if code != 2 {
				t.Errorf("exit code: got %d, want 2 (stderr: %s)", code, stderr)
			}
			if !strings.Contains(stderr, "regex parse error") {
				t.Errorf("stderr should carry ripgrep's message: %s", stderr)
			}
			if strings.Contains(stdout, "No matches") {
				t.Errorf("a bad pattern must not report success: %s", stdout)
			}
		})
	}
}

func TestExitCode_InvalidRegexWithJSONPrintsNothingOnStdout(t *testing.T) {
	_, path := writeLog(t)

	code, stdout, stderr := runRx(t, "trace", "a(", path, "--json")

	if code != 2 {
		t.Errorf("exit code: got %d, want 2 (stderr: %s)", code, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should be empty on a fatal error, got: %s", stdout)
	}
}

// TestExitCode_UnknownFlagIsTwo uses `samples`, not `trace`: trace sets
// FParseErrWhitelist.UnknownFlags so that -i, -w and friends pass
// through to ripgrep, which is deliberate click parity.
func TestExitCode_UnknownFlagIsTwo(t *testing.T) {
	_, path := writeLog(t)

	code, _, stderr := runRx(t, "samples", path, "--lines=1", "--definitely-not-a-flag")

	if code != 2 {
		t.Errorf("exit code: got %d, want 2 (stderr: %s)", code, stderr)
	}
}

func TestExitCode_MissingPatternIsTwo(t *testing.T) {
	code, _, stderr := runRx(t, "trace")

	if code != 2 {
		t.Errorf("exit code: got %d, want 2 (stderr: %s)", code, stderr)
	}
}

func TestExitCode_SuccessWithMatchesIsZero(t *testing.T) {
	_, path := writeLog(t)

	code, stdout, stderr := runRx(t, "trace", "error", path)

	if code != 0 {
		t.Errorf("exit code: got %d, want 0 (stderr: %s)", code, stderr)
	}
	// The match list prints positions, not the matched text; the text
	// appears in the context section when a context window is asked for.
	if !strings.Contains(stdout, "app.log:2:11 [error]") {
		t.Errorf("stdout should show the match position: %s", stdout)
	}
}

func TestExitCode_SuccessWithNoMatchesIsZero(t *testing.T) {
	_, path := writeLog(t)

	code, _, stderr := runRx(t, "trace", "no-such-text-anywhere", path)

	if code != 0 {
		t.Errorf("exit code: got %d, want 0 (stderr: %s)", code, stderr)
	}
}

func TestExitCode_InterruptedIsFive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT delivery differs on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	// Large enough that the scan is still running when the signal lands.
	var content strings.Builder
	for i := 0; i < 400000; i++ {
		content.WriteString("some line of log text that we will search through\n")
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	cmd := exec.Command(rxBinary(t), "trace", "line", path, "--json")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	err := cmd.Wait()

	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Skipf("process finished before the signal landed (err=%v)", err)
	}
	if exitErr.ExitCode() != 5 {
		t.Errorf("exit code: got %d, want 5", exitErr.ExitCode())
	}
}
