package main

// The --search-root sandbox is a persistent flag: every subcommand that
// takes a path honors it, and RX_SEARCH_ROOTS is the environment form of
// the same switch. These tests run the real binary because the sandbox is
// installed by the root command's pre-run hook, which an in-process test
// of a single RunE function never reaches.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/clicommand"
)

// runRxEnv runs the built binary with extra environment variables and
// returns its exit code, stdout and stderr.
func runRxEnv(t *testing.T, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := rxCommand(t, env, args...)
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

// sandboxFixture builds a root directory holding one log file and a
// sibling directory outside it holding another.
func sandboxFixture(t *testing.T) (root, inside, outside, outsideFile string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	inside = filepath.Join(root, "in.log")
	outsideFile = filepath.Join(outside, "out.log")
	body := []byte("alpha\nerror happened\nomega\n")
	for _, f := range []string{inside, outsideFile} {
		if err := os.WriteFile(f, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return root, inside, outside, outsideFile
}

func TestSearchRoot_CompressOutputOutsideRootIsDenied(t *testing.T) {
	root, inside, outside, _ := sandboxFixture(t)
	target := filepath.Join(outside, "x.zst")

	code, _, stderr := runRx(t, "compress", inside, "--output="+target, "--search-root="+root)
	if code != clicommand.ExitAccessDenied {
		t.Errorf("exit code: got %d, want %d (stderr: %s)", code, clicommand.ExitAccessDenied, stderr)
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("%s was written outside the sandbox", target)
	}
}

func TestSearchRoot_TraceOutsideRootIsDenied(t *testing.T) {
	root, _, _, outsideFile := sandboxFixture(t)

	code, _, stderr := runRx(t, "trace", "error", outsideFile, "--search-root="+root)
	if code != clicommand.ExitAccessDenied {
		t.Errorf("exit code: got %d, want %d (stderr: %s)", code, clicommand.ExitAccessDenied, stderr)
	}
}

func TestSearchRoot_SamplesOutsideRootIsDenied(t *testing.T) {
	root, _, _, outsideFile := sandboxFixture(t)

	code, _, stderr := runRx(t, "samples", outsideFile, "--lines=1", "--search-root="+root)
	if code != clicommand.ExitAccessDenied {
		t.Errorf("exit code: got %d, want %d (stderr: %s)", code, clicommand.ExitAccessDenied, stderr)
	}
}

func TestSearchRoot_IndexHonorsTheEnvironmentVariable(t *testing.T) {
	root, _, _, outsideFile := sandboxFixture(t)

	code, _, stderr := runRxEnv(t, []string{"RX_SEARCH_ROOTS=" + root}, "index", outsideFile)
	if code != clicommand.ExitAccessDenied {
		t.Errorf("exit code: got %d, want %d (stderr: %s)", code, clicommand.ExitAccessDenied, stderr)
	}
}

// The flag repeats, and a path inside either root is accepted.
func TestSearchRoot_RepeatedFlagAcceptsEitherRoot(t *testing.T) {
	rootA, insideA, rootB, insideB := sandboxFixture(t)

	for _, target := range []string{insideA, insideB} {
		code, _, stderr := runRx(t, "trace", "error", target,
			"--search-root="+rootA, "--search-root="+rootB)
		if code != clicommand.ExitSuccess {
			t.Errorf("%s: exit code got %d, want 0 (stderr: %s)", target, code, stderr)
		}
	}
}

// Without the flag and without the variable there is no sandbox, which is
// what an ordinary CLI invocation relies on.
func TestSearchRoot_UnsetLeavesEveryPathReachable(t *testing.T) {
	_, _, _, outsideFile := sandboxFixture(t)

	code, stdout, stderr := runRx(t, "trace", "error", outsideFile, "--json")
	if code != clicommand.ExitSuccess {
		t.Fatalf("exit code: got %d, want 0 (stderr: %s)", code, stderr)
	}
	var resp struct {
		Matches []struct {
			LineText string `json:"line_text"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("decode json: %v (%s)", err, stdout)
	}
	if len(resp.Matches) != 1 {
		t.Fatalf("matches: got %d, want 1", len(resp.Matches))
	}
	if resp.Matches[0].LineText != "error happened" {
		t.Errorf("line_text: got %q", resp.Matches[0].LineText)
	}
}

// A root that does not exist is a usage error, not a silent no-sandbox.
func TestSearchRoot_MissingRootIsAUsageError(t *testing.T) {
	base := t.TempDir()
	code, _, stderr := runRx(t, "trace", "error", base, "--search-root="+filepath.Join(base, "nope"))
	if code != clicommand.ExitUsageError {
		t.Errorf("exit code: got %d, want %d (stderr: %s)", code, clicommand.ExitUsageError, stderr)
	}
}

// Every subcommand that takes a path advertises the flag.
func TestSearchRoot_AppearsInEverySubcommandHelp(t *testing.T) {
	for _, sub := range []string{"trace", "samples", "index", "compress", "serve"} {
		code, stdout, stderr := runRx(t, sub, "--help")
		if code != clicommand.ExitSuccess {
			t.Fatalf("%s --help: exit %d (stderr: %s)", sub, code, stderr)
		}
		if !strings.Contains(stdout, "--search-root") {
			t.Errorf("%s --help does not mention --search-root", sub)
		}
	}
}
