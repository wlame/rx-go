package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// `rx trace --debug` never did anything. It stays accepted, so a script
// that passes it keeps working, but help no longer offers it and using
// it says on stderr that it is deprecated. stdout keeps the answer only.
func TestTraceDebugFlag_IsAcceptedHiddenAndDeprecated(t *testing.T) {
	_, path := writeLog(t)

	code, stdout, stderr := runRx(t, "trace", "error", path, "--debug", "--json")

	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (stderr: %s)", code, stderr)
	}
	var answer struct {
		Matches []json.RawMessage `json:"matches"`
	}
	if err := json.Unmarshal([]byte(stdout), &answer); err != nil || len(answer.Matches) != 1 {
		t.Errorf("stdout should be the JSON answer with one match (err %v): %s", err, stdout)
	}
	if !strings.Contains(stderr, "--debug") || !strings.Contains(stderr, "deprecated") {
		t.Errorf("stderr should say --debug is deprecated: %q", stderr)
	}

	_, help, _ := runRx(t, "trace", "--help")
	if strings.Contains(help, "--debug") {
		t.Errorf("rx trace --help still lists --debug:\n%s", help)
	}
}
