package clicommand

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func thirtyLineFixture(t *testing.T) string {
	t.Helper()
	var text strings.Builder
	for i := 1; i <= 30; i++ {
		text.WriteString("LINE " + strings.Repeat("x", i%3) + " payload\n")
	}
	f := filepath.Join(t.TempDir(), "l30.log")
	if err := os.WriteFile(f, []byte(text.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return f
}

// A line counted from the end that reaches before the first line is
// answered as missing, under the key it was asked by, with the same
// warning a line past the end gets; the command exits 0.
func TestSamplesCommand_LineBeforeTheStartIsMissing(t *testing.T) {
	f := thirtyLineFixture(t)
	var out bytes.Buffer
	stderr := captureStderr(t)
	if err := runSamples(&out, samplesParams{path: f, lines: []string{"-31"}, ctxLines: 0, jsonOutput: true}); err != nil {
		t.Fatalf("runSamples: %v", err)
	}
	got := decodeSamples(t, out.Bytes())
	if lines := got["lines"].(map[string]any); lines["-31"] != float64(-1) || len(lines) != 1 {
		t.Errorf("lines = %v, want {-31: -1}", lines)
	}
	if !strings.Contains(stderr(), "Warning: line -31 is not in the file.") {
		t.Errorf("stderr = %q, want the missing-line warning", stderr())
	}
}

// --context=-1 means the default, as context=-1 does over HTTP, and
// --before=-1 or --after=-1 leaves that side to --context.
func TestSamplesCommand_MinusOneContextIsTheDefault(t *testing.T) {
	f := thirtyLineFixture(t)
	answer := func(p samplesParams) map[string]any {
		var out bytes.Buffer
		p.path, p.lines, p.jsonOutput = f, []string{"10"}, true
		if err := runSamples(&out, p); err != nil {
			t.Fatalf("runSamples(%+v): %v", p, err)
		}
		return decodeSamples(t, out.Bytes())
	}
	want := answer(samplesParams{ctxLines: 3})
	for name, p := range map[string]samplesParams{
		"context -1": {ctxLines: -1},
		"before -1":  {ctxLines: 3, beforeCtx: -1, beforeSet: true},
		"after -1":   {ctxLines: 3, afterCtx: -1, afterSet: true},
		"both -1":    {ctxLines: -1, beforeCtx: -1, beforeSet: true, afterCtx: -1, afterSet: true},
	} {
		got := answer(p)
		if len(got["samples"].(map[string]any)["10"].([]any)) != 7 || got["before_context"] != float64(3) || got["after_context"] != float64(3) {
			t.Errorf("%s: answered %v, want the default context of 3 (%v)", name, got, want)
		}
	}
}
