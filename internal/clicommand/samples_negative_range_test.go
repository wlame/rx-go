package clicommand

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --lines=-3--1 prints the last three lines, like tail -n 3; a range
// whose ends are in the wrong order is a usage error that names a line
// range, and a negative offset range stays refused with its own wording.
func TestSamplesCommand_NegativeLineRange(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(f, []byte("one\ntwo\nthree\nfour\nfive\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var buf bytes.Buffer
	if err := runSamples(&buf, samplesParams{path: f, lines: []string{"-3--1"}, ctxLines: 0, jsonOutput: true}); err != nil {
		t.Fatalf("runSamples: %v", err)
	}
	got := decodeSamples(t, buf.Bytes())
	sample, ok := got["samples"].(map[string]any)["3-5"].([]any)
	if !ok || len(sample) != 3 || sample[0] != "three" || sample[2] != "five" {
		t.Errorf("samples[3-5] = %v, want [three four five]; answer %s", sample, buf.String())
	}

	for _, tc := range []struct {
		params samplesParams
		want   string
	}{
		{samplesParams{path: f, lines: []string{"-1--3"}, ctxLines: 0}, "invalid line range: -1--3"},
		{samplesParams{path: f, offsets: []string{"-3--1"}, ctxLines: 0}, "invalid offset range: -3--1"},
	} {
		var stderr bytes.Buffer
		err := runSamples(&stderr, tc.params)
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
			t.Fatalf("%v: got %v, want exit code %d", tc.params.lines, err, ExitUsageError)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error %q, want it to hold %q", err.Error(), tc.want)
		}
	}
}
