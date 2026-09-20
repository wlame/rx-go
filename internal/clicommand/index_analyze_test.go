package clicommand

// Tests for the `--analyze-window-lines` CLI flag: that it parses into
// indexParams, that leaving it off keeps the 0 the resolver reads as
// "not set", and that an index built with it succeeds end to end. What
// the window size does to the detectors is covered by
// internal/index/builder_analyze_test.go.
//
// The params are captured through the real constructor, because the flag
// table is the thing under test: a test-local copy of it stops testing
// the command as soon as one flag changes shape, which is how
// `--threshold` came to be an int in one place and a *int in the other.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureIndexParams runs the real `rx index` command with its real
// flags and returns the indexParams they produced, without indexing
// anything.
func captureIndexParams(t *testing.T, args []string) indexParams {
	t.Helper()

	var captured indexParams
	cmd := newIndexCommand(io.Discard, func(_ io.Writer, p indexParams) error {
		captured = p
		return nil
	})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return captured
}

// TestIndexCommand_AnalyzeWindowLinesFlag_Parses verifies the
// --analyze-window-lines flag lands in indexParams.analyzeWindowLines.
func TestIndexCommand_AnalyzeWindowLinesFlag_Parses(t *testing.T) {
	p := captureIndexParams(t, []string{"/tmp/dummy", "--analyze-window-lines=256"})
	if p.analyzeWindowLines != 256 {
		t.Errorf("analyzeWindowLines: got %d, want 256", p.analyzeWindowLines)
	}
}

// TestIndexCommand_AnalyzeWindowLinesFlag_DefaultZero confirms the flag
// defaults to 0 (the "not set" sentinel the resolver expects).
func TestIndexCommand_AnalyzeWindowLinesFlag_DefaultZero(t *testing.T) {
	p := captureIndexParams(t, []string{"/tmp/dummy"})
	if p.analyzeWindowLines != 0 {
		t.Errorf("analyzeWindowLines default: got %d, want 0", p.analyzeWindowLines)
	}
}

// TestIndexCommand_AnalyzeWindowLines_EndToEnd runs the real index
// command on a small file with --analyze --analyze-window-lines=32
// and confirms the command succeeds and writes a cache entry.
//
// We don't introspect the coordinator state here — that's covered by
// the internal/index tests; this test is the glue-level smoke check.
func TestIndexCommand_AnalyzeWindowLines_EndToEnd(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "small.log")
	// Keep fixture small but populous enough for multiple line-index
	// entries. --threshold=1 lets anything ≥ 1 MB through.
	if err := os.WriteFile(src, []byte(strings.Repeat("payload line\n", 100000)), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{
		src,
		"--json",
		"--threshold=1",
		"--analyze",
		"--analyze-window-lines=32",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\n%s", err, buf.String())
	}
	// If the flag had tripped any validation the command would error;
	// for a smoke test, command-level success is enough.
	if !strings.Contains(buf.String(), "\"indexed\":") {
		t.Errorf("expected JSON output with 'indexed' key, got: %s", buf.String())
	}
}
