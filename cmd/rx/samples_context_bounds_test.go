package main

import (
	"strings"
	"testing"
)

// rx samples takes -1 as the default on each context flag and refuses
// anything below it with a usage error.
func TestExitCode_SamplesContextBelowMinusOneIsTwo(t *testing.T) {
	_, path := writeLog(t)

	for flag, want := range map[string]string{
		"--context=-2": "--context must be -1 or more, got -2",
		"--before=-2":  "--before must be -1 or more, got -2",
		"--after=-2":   "--after must be -1 or more, got -2",
	} {
		code, stdout, stderr := runRx(t, "samples", path, "--lines=1", flag, "--json")
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
	if code, _, stderr := runRx(t, "samples", path, "--lines=1", "--context=-1", "--json"); code != 0 {
		t.Errorf("--context=-1 (the default): exit %d, want 0 (stderr: %s)", code, stderr)
	}
}
