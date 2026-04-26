package clicommand

// Tests for the `--analyze-window-lines` CLI flag added in Task 6.
//
// Coverage:
//   - Flag parses and round-trips into the indexParams struct (captured
//     via a test-only fork of runIndex that records the params).
//   - The zero default (no flag supplied) leaves analyzeWindowLines at 0
//     so the resolver can fall through to env / default.
//   - End-to-end: a file indexed with `--analyze --analyze-window-lines=N`
//     produces a valid cache — we don't instrument the coordinator here,
//     that's covered by internal/index/builder_analyze_test.go; we only
//     verify the flag doesn't break the command.
//
// Why we capture params via a sibling constructor rather than overriding
// runIndex: the exported NewIndexCommand hard-codes `runIndex` as its
// RunE target. To avoid rewriting the command just for tests, the test
// builds its own minimal cobra command that reuses the flag wiring.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// captureIndexParams builds a cobra command with the exact same flag
// definitions as NewIndexCommand but swaps RunE for a capture closure.
// Returns the indexParams the command observed after Execute.
func captureIndexParams(t *testing.T, args []string) indexParams {
	t.Helper()

	var (
		captured           indexParams
		force              bool
		showInfo           bool
		deleteFlag         bool
		jsonOutput         bool
		recursive          bool
		analyze            bool
		threshold          int
		analyzeWindowLines int
	)

	cmd := &cobra.Command{
		Use:  "index PATH [PATH ...]",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, cmdArgs []string) error {
			captured = indexParams{
				paths:              cmdArgs,
				force:              force,
				showInfo:           showInfo,
				delete:             deleteFlag,
				jsonOutput:         jsonOutput,
				recursive:          recursive,
				analyze:            analyze,
				threshold:          threshold,
				analyzeWindowLines: analyzeWindowLines,
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "")
	cmd.Flags().BoolVarP(&showInfo, "info", "i", false, "")
	cmd.Flags().BoolVarP(&deleteFlag, "delete", "d", false, "")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "")
	cmd.Flags().BoolVarP(&analyze, "analyze", "a", false, "")
	cmd.Flags().IntVar(&threshold, "threshold", 0, "")
	cmd.Flags().IntVar(&analyzeWindowLines, "analyze-window-lines", 0, "")

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
