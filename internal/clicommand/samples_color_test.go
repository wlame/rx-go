package clicommand

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --color takes the same three values in both backends, and an
// unrecognized one is a usage error rather than a silent fall back to
// auto — a typo that quietly does the opposite of what was asked is
// worse than a refusal.

func colorFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	var body strings.Builder
	for n := 1; n <= 20; n++ {
		body.WriteString("line with error\n")
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestSamplesColor_AcceptedValues(t *testing.T) {
	path := colorFixture(t)
	const escape = "\x1b["

	cases := []struct {
		flag        string
		wantEscapes bool
	}{
		{"always", true},
		{"never", false},
		{"auto", false}, // the test writer is not a terminal
		{"", false},     // the historical spelling of auto
	}

	for _, tc := range cases {
		name := tc.flag
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			err := runSamples(&buf, samplesParams{
				path:      path,
				lines:     "5",
				ctxLines:  3,
				colorFlag: tc.flag,
			})
			if err != nil {
				t.Fatalf("runSamples: %v", err)
			}
			if got := strings.Contains(buf.String(), escape); got != tc.wantEscapes {
				t.Errorf("escapes present: got %v, want %v", got, tc.wantEscapes)
			}
		})
	}
}

func TestSamplesColor_UnknownValueIsAUsageError(t *testing.T) {
	path := colorFixture(t)

	var buf bytes.Buffer
	err := runSamples(&buf, samplesParams{
		path:      path,
		lines:     "5",
		ctxLines:  3,
		colorFlag: "sometimes",
	})
	if err == nil {
		t.Fatalf("runSamples: got nil error, want a usage error")
	}
	var exitErr *ExitError
	if !asExit(err, &exitErr) {
		t.Fatalf("error type: got %T, want *ExitError", err)
	}
	if exitErr.Code != ExitUsageError {
		t.Errorf("exit code: got %d, want %d", exitErr.Code, ExitUsageError)
	}
}
