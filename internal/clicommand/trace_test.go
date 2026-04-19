package clicommand

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreprocessArgs mirrors the logic in cmd/rx/main.go via direct
// function calls. We re-implement the small helper here to keep the
// test package-local — main.preprocessArgs is intentionally not
// exported (it's a main-package implementation detail).
//
// The test lives in clicommand to enforce one package per test binary,
// but the behavior asserted applies to main; see main_test.go for the
// integration-level coverage.
func TestResolveTracePositionals(t *testing.T) {
	cases := []struct {
		name       string
		input      traceParams
		wantPatts  []string
		wantPathsN int
	}{
		{
			name: "single pattern, single path",
			input: traceParams{
				args: []string{"error", "/tmp/a.log"},
			},
			wantPatts:  []string{"error"},
			wantPathsN: 1,
		},
		{
			name: "pattern plus multiple paths",
			input: traceParams{
				args: []string{"error", "a.log", "b.log", "c.log"},
			},
			wantPatts:  []string{"error"},
			wantPathsN: 3,
		},
		{
			name: "--regexp flag means args are all paths",
			input: traceParams{
				args:    []string{"/tmp/a.log"},
				regexps: []string{"err"},
			},
			wantPatts:  []string{"err"},
			wantPathsN: 1,
		},
		{
			name: "multiple --regexp flags plus positional pattern",
			input: traceParams{
				args:    []string{"a.log"},
				regexps: []string{"err", "warn"},
			},
			wantPatts:  []string{"err", "warn"},
			wantPathsN: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patts, paths, err := resolveTracePositionals(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(patts) != len(tc.wantPatts) {
				t.Errorf("patterns count: got %d, want %d (%v)", len(patts), len(tc.wantPatts), patts)
			}
			for i, want := range tc.wantPatts {
				if i >= len(patts) || patts[i] != want {
					t.Errorf("pattern[%d]: got %v, want %s", i, patts, want)
				}
			}
			if len(paths) != tc.wantPathsN {
				t.Errorf("paths count: got %d, want %d", len(paths), tc.wantPathsN)
			}
		})
	}
}

// TestParseFrameSize covers compress.go's suffix matrix.
func TestParseFrameSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"4M", 4 << 20},
		{"4MB", 4 << 20},
		{"4m", 4 << 20},
		{"1G", 1 << 30},
		{"512", 512},
		{"2K", 2 << 10},
		{"2KB", 2 << 10},
	}
	for _, tc := range cases {
		got, err := ParseFrameSize(tc.in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestTraceCommand_JSONOutput runs the cobra command end-to-end,
// capturing stdout and parsing the JSON.
func TestTraceCommand_JSONOutput(t *testing.T) {
	if _, err := os.Stat("/usr/bin/rg"); err != nil {
		// Some CI images don't have rg; the trace invocation exits 1
		// early with a clear message.
		t.Skip("rg not installed")
	}
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("hello\nerror one\nok\nerror two\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", f, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"matches"`) {
		t.Errorf("output missing 'matches' key: %s", out)
	}
	if !strings.Contains(out, "error one") {
		t.Errorf("output missing match line: %s", out)
	}
}

// TestSamplesCommand_LineMode covers basic invocation.
func TestSamplesCommand_LineMode(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "--lines=3", "--context=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "two") || !strings.Contains(out, "three") || !strings.Contains(out, "four") {
		t.Errorf("samples output missing context: %s", out)
	}
}

// TestSamplesCommand_MutexError asserts --offsets + --lines is a usage
// error with non-zero exit (via returned error from RunE).
func TestSamplesCommand_MutexError(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("x\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "--offsets=1", "--lines=1"})
	if err := cmd.Execute(); err == nil {
		t.Errorf("expected mutex error, got none")
	}
}

// TestIndexCommand_InfoMissing asserts --info on an unknown path returns
// an error.
func TestIndexCommand_InfoMissing(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "missing.log")
	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{f, "--info"})
	err := cmd.Execute()
	if err == nil {
		t.Errorf("expected error for missing index")
	}
}

// TestCompressCommand_BadLevel.
func TestCompressCommand_BadLevel(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("content"), 0o644)
	var buf bytes.Buffer
	cmd := NewCompressCommand(&buf)
	cmd.SetArgs([]string{f, "--level=99"})
	err := cmd.Execute()
	if err == nil {
		t.Errorf("expected error for bad level")
	}
}

// TestCompressCommand_HappyPath creates a small fixture, compresses it,
// and verifies the output exists.
func TestCompressCommand_HappyPath(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	content := strings.Repeat("a line of log\n", 200)
	_ = os.WriteFile(f, []byte(content), 0o644)

	out := f + ".zst"
	var buf bytes.Buffer
	cmd := NewCompressCommand(&buf)
	cmd.SetArgs([]string{f, "--frame-size=4K", "--level=1", "--output=" + out})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output not created: %v", err)
	}
}

// TestCompressCommand_OutputExists returns error without --force.
func TestCompressCommand_OutputExists(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	out := f + ".zst"
	_ = os.WriteFile(out, []byte("existing"), 0o644)

	var buf bytes.Buffer
	cmd := NewCompressCommand(&buf)
	cmd.SetArgs([]string{f, "--output=" + out})
	err := cmd.Execute()
	if err == nil {
		t.Errorf("expected error on existing output without --force")
	}
}

// TestColorDecision covers the env + flag interaction.
func TestColorDecision(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("RX_NO_COLOR", "")
	var buf bytes.Buffer
	// With a non-*os.File writer, colorDecision falls through to true
	// (enabled). Verify that behavior holds.
	if got := colorDecision(false, &buf); !got {
		t.Errorf("bytes.Buffer writer should default to color-enabled")
	}
	if colorDecision(true, &buf) {
		t.Errorf("--no-color flag should disable")
	}
	t.Setenv("NO_COLOR", "1")
	if colorDecision(false, &buf) {
		t.Errorf("NO_COLOR env should disable")
	}
}
