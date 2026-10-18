package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A count of lines or matches below 0 has no meaning: rx trace refuses
// it with a usage error instead of reading it as 0 (the context flags)
// or as "no cap" (--max-results).
func TestExitCode_NegativeTraceCountIsTwo(t *testing.T) {
	_, path := writeLog(t)

	for flag, want := range map[string]string{
		"--context=-1":     "--context must be 0 or more, got -1",
		"--before=-1":      "--before must be 0 or more, got -1",
		"--after=-1":       "--after must be 0 or more, got -1",
		"--max-results=-5": "--max-results must be 0 or more, got -5",
	} {
		code, stdout, stderr := runRx(t, "trace", "line", path, flag, "--json")
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr: %s)", flag, code, stderr)
		}
		if !strings.Contains(stderr, want) {
			t.Errorf("%s: stderr %q, want %q", flag, stderr, want)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Errorf("%s: stdout should be empty, got %q", flag, stdout)
		}
	}
}

// 0 keeps its meanings: no context, and no cap on the matches; 1 caps.
func TestExitCode_ZeroTraceCountsAreAccepted(t *testing.T) {
	_, path := writeLog(t)

	matches := func(flags ...string) int {
		args := append([]string{"trace", "line", path, "--json"}, flags...)
		code, stdout, stderr := runRx(t, args...)
		if code != 0 {
			t.Fatalf("rx %v: exit %d (stderr: %s)", args, code, stderr)
		}
		var body struct {
			Matches []json.RawMessage `json:"matches"`
		}
		if err := json.Unmarshal([]byte(stdout), &body); err != nil {
			t.Fatalf("rx %v: %v in %q", args, err, stdout)
		}
		return len(body.Matches)
	}
	if got := matches("--context=0", "--max-results=0"); got != 2 {
		t.Errorf("--max-results=0 (unlimited): %d matches, want 2", got)
	}
	if got := matches("--max-results=1"); got != 1 {
		t.Errorf("--max-results=1: %d matches, want 1", got)
	}
}

// rx compress refuses a worker count below 1 and a frame size below one
// byte, each with a usage error, instead of clamping the workers to 1
// and letting the encoder replace the frame size with its default.
func TestExitCode_CompressBoundsAreTwo(t *testing.T) {
	dir, path := writeLog(t)

	// Each case names its own output, so a case that wrongly succeeds
	// cannot make the next one fail on an output that already exists.
	outputs := 0
	for flag, want := range map[string]string{
		"--workers=0":       "--workers must be 1 or more, got 0",
		"--workers=-3":      "--workers must be 1 or more, got -3",
		"--frame-size=0":    "--frame-size must be at least 1 byte",
		"--frame-size=-1":   "--frame-size must be at least 1 byte",
		"--frame-size=0B":   "--frame-size must be at least 1 byte",
		"--frame-size=0.1B": "--frame-size must be at least 1 byte",
	} {
		outputs++
		output := filepath.Join(dir, fmt.Sprintf("out%d.zst", outputs))
		code, _, stderr := runRx(t, "compress", path, flag, "--output="+output, "--no-index")
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr: %s)", flag, code, stderr)
		}
		if !strings.Contains(stderr, want) {
			t.Errorf("%s: stderr %q, want %q", flag, stderr, want)
		}
	}
}

// A usage error leaves no --output-dir behind; with valid flags the
// directory is created and the output lands in it.
func TestExitCode_CompressUsageErrorCreatesNoOutputDir(t *testing.T) {
	dir, path := writeLog(t)
	newdir := filepath.Join(dir, "newdir")

	code, _, stderr := runRx(t, "compress", path, "--output-dir="+newdir, "--level=99", "--no-index")
	if code != 2 {
		t.Fatalf("--level=99: exit %d, want 2 (stderr: %s)", code, stderr)
	}
	if _, err := os.Stat(newdir); !os.IsNotExist(err) {
		t.Errorf("--output-dir exists after a usage error (stat: %v)", err)
	}

	code, _, stderr = runRx(t, "compress", path, "--output-dir="+newdir, "--level=3", "--no-index")
	if code != 0 {
		t.Fatalf("valid flags: exit %d (stderr: %s)", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(newdir, "app.log.zst")); err != nil {
		t.Errorf("the output is not in --output-dir: %v", err)
	}
}
