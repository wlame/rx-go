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
	cmd := rxCommand(t, nil, args...)
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

// index and compress go through every named path and report each
// failure in their output, so a missing file there is one failure among
// possibly several. When every failure is a missing file, the exit code
// is the one the contract gives a missing file, as in trace and samples.
func TestExitCode_IndexAndCompressMissingFileIsThree(t *testing.T) {
	for _, subcommand := range []string{"index", "compress"} {
		t.Run(subcommand, func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "nope.log")

			code, _, stderr := runRx(t, subcommand, missing)

			if code != 3 {
				t.Errorf("exit code: got %d, want 3 (stderr: %s)", code, stderr)
			}
		})
	}
}

func TestExitCode_IndexWithOneMissingPathIsThreeAndIndexesTheOther(t *testing.T) {
	_, present := writeLog(t)
	missing := filepath.Join(t.TempDir(), "nope.log")
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	code, stdout, stderr := runRx(t, "index", "--threshold=0", "--json", present, missing)

	if code != 3 {
		t.Errorf("exit code: got %d, want 3 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, `"path": "`+present+`"`) {
		t.Errorf("the present file should still be indexed: %s", stdout)
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
			// The message names the pattern as it was typed, and
			// keeps ripgrep's reason without the alternation ripgrep
			// wraps the patterns in.
			if !strings.Contains(stderr, `"`+pattern+`"`) {
				t.Errorf("stderr should name the pattern: %s", stderr)
			}
			if strings.Contains(stderr, "(?:") {
				t.Errorf("stderr leaks ripgrep's wrapper: %s", stderr)
			}
			if strings.Contains(stdout, "No matches") {
				t.Errorf("a bad pattern must not report success: %s", stdout)
			}
		})
	}
}

// A pattern PCRE2 cannot compile is the same usage error as one the
// default engine cannot: exit 2 with PCRE2's reason, never "no matches"
// with the file skipped. A ripgrep without PCRE2 refuses -P with exit 2
// as well, and says so.
func TestExitCode_InvalidPCRE2PatternIsTwo(t *testing.T) {
	_, path := writeLog(t)

	code, stdout, stderr := runRx(t, "trace", "-P", "-e", "(", path, "--json")

	if code != 2 {
		t.Errorf("exit code: got %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "PCRE2") {
		t.Errorf("stderr should carry PCRE2's reason: %s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should be empty on a fatal error, got: %s", stdout)
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

// A flag cobra rejects never reaches a command's RunE, so nothing inside
// the command can print it; the process has to, or the user gets exit 2
// and a blank screen.
func TestExitCode_UnknownFlagIsTwoAndNamed(t *testing.T) {
	_, path := writeLog(t)
	argLists := [][]string{
		{"samples", path, "--lines=1", "--definitely-not-a-flag"},
		{"index", path, "--definitely-not-a-flag"},
		{"compress", path, "--definitely-not-a-flag"},
		{"serve", "--definitely-not-a-flag"},
	}
	for _, args := range argLists {
		t.Run(args[0], func(t *testing.T) {
			code, _, stderr := runRx(t, args...)

			if code != 2 {
				t.Errorf("exit code: got %d, want 2 (stderr: %s)", code, stderr)
			}
			if !strings.Contains(stderr, "--definitely-not-a-flag") {
				t.Errorf("stderr does not name the flag: %q", stderr)
			}
		})
	}
}

func TestExitCode_MissingSearchRootIsTwoAndExplained(t *testing.T) {
	_, path := writeLog(t)
	missing := filepath.Join(t.TempDir(), "no-such-root")

	code, _, stderr := runRx(t, "samples", path, "--lines=1", "--search-root="+missing)

	if code != 2 {
		t.Errorf("exit code: got %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.HasPrefix(stderr, "Error: ") || !strings.Contains(stderr, "no-such-root") {
		t.Errorf("stderr should be an Error: line naming the root, got %q", stderr)
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

	cmd := rxCommand(t, nil, "trace", "line", path, "--json")
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

// A time query that cannot be answered is a usage error, exit 2: modes
// mixed, a value that is not a time, a file without timestamps. A
// timestamp with a comma is one value, and a time no line reaches
// answers -1 with exit 0.
func TestExitCode_SamplesByTime(t *testing.T) {
	dir := t.TempDir()
	timed := filepath.Join(dir, "timed.log")
	if err := os.WriteFile(timed, []byte("2025-12-10 12:34:56,123 one\n2025-12-10 12:34:57,000 two\n"+
		"2025-12-10 12:34:58,000 three\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	_, plain := writeLog(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"a timestamp with a comma", []string{"samples", timed, "--timestamps=2025-12-10 12:34:56,123"}, 0},
		{"short flag, repeated", []string{"samples", timed, "-t", "12:34:57", "-t", "12:34:58"}, 0},
		{"no line at the time", []string{"samples", timed, "--timestamps=2026-01-01"}, 0},
		{"with --lines", []string{"samples", timed, "--timestamps=12:34:57", "--lines=1"}, 2},
		{"with --offsets", []string{"samples", timed, "--timestamps=12:34:57", "--offsets=0"}, 2},
		{"not a time", []string{"samples", timed, "--timestamps=soon"}, 2},
		{"a file without timestamps", []string{"samples", plain, "--timestamps=12:34:57"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runRx(t, tc.args...)
			if code != tc.want {
				t.Errorf("exit code %d, want %d (stderr: %s)", code, tc.want, stderr)
			}
		})
	}
}
