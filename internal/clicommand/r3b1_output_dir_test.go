package clicommand

// Test for R3-B1: rx compress --output-dir=DIR parity with Python.
//
// Python's rx compress accepts --output-dir; output file is written as
// {output-dir}/{basename}.zst (with any compression suffix stripped).
// The directory is auto-created with os.makedirs(exist_ok=True).
//
// Precedence (matches Python):
//   --output + --output-dir  → usage error (mutually exclusive)
//   --output                 → exact single-file output
//   --output-dir             → {dir}/{basename}.zst
//   neither                  → {sourceDir}/{basename}.zst

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompress_OutputDir_WritesFileToRequestedDir verifies the happy path:
// --output-dir=DIR writes to DIR/basename.zst rather than next to the source.
func TestCompress_OutputDir_WritesFileToRequestedDir(t *testing.T) {
	srcDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "compressed")
	// Intentionally do NOT pre-create outDir — the fix must auto-create it
	// (Python's os.makedirs(exist_ok=True) parity).

	src := filepath.Join(srcDir, "data.log")
	if err := os.WriteFile(src, []byte(strings.Repeat("hello\n", 500)), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewCompressCommand(&buf)
	cmd.SetArgs([]string{src, "--output-dir=" + outDir, "--json", "--level=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("compress --output-dir: %v\nstdout: %s", err, buf.String())
	}

	// Parse JSON envelope, check output path reflects outDir.
	var resp map[string]any
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("json parse: %v\nbody: %s", err, buf.String())
	}
	files := resp["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files: got %d, want 1", len(files))
	}
	entry := files[0].(map[string]any)
	if entry["success"] != true {
		t.Fatalf("compression failed: %+v", entry)
	}

	got := entry["output"].(string)
	want := filepath.Join(outDir, "data.log.zst")
	if got != want {
		t.Errorf("output path: got %q, want %q", got, want)
	}

	// File must actually exist on disk.
	if _, err := os.Stat(want); err != nil {
		t.Errorf("output file not on disk: %v", err)
	}

	// Source-dir sibling must NOT exist (the --output-dir flag redirected it).
	siblingPath := src + ".zst"
	if _, err := os.Stat(siblingPath); err == nil {
		t.Errorf("unexpected sibling file %q — --output-dir should redirect", siblingPath)
	}
}

// TestCompress_OutputDir_CreatesMissingDirectory verifies the auto-create
// behavior (Python's os.makedirs(exist_ok=True)) — nested new dirs OK.
func TestCompress_OutputDir_CreatesMissingDirectory(t *testing.T) {
	srcDir := t.TempDir()
	// Nested, none of the path components exist yet.
	outDir := filepath.Join(t.TempDir(), "a", "b", "c")

	src := filepath.Join(srcDir, "data.log")
	if err := os.WriteFile(src, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewCompressCommand(&buf)
	cmd.SetArgs([]string{src, "--output-dir=" + outDir, "--json", "--level=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("compress: %v\nout: %s", err, buf.String())
	}

	info, err := os.Stat(outDir)
	if err != nil {
		t.Fatalf("outDir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("outDir is not a directory")
	}
	if _, err := os.Stat(filepath.Join(outDir, "data.log.zst")); err != nil {
		t.Errorf("compressed file missing: %v", err)
	}
}

// TestCompress_OutputDir_ConflictsWithOutput verifies Python's precedence
// rule: passing both --output and --output-dir is a usage error.
func TestCompress_OutputDir_ConflictsWithOutput(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	src := filepath.Join(srcDir, "data.log")
	if err := os.WriteFile(src, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewCompressCommand(&buf)
	cmd.SetErr(&buf) // capture stderr too
	cmd.SetArgs([]string{
		src,
		"--output=" + filepath.Join(outDir, "explicit.zst"),
		"--output-dir=" + outDir,
		"--level=1",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected usage error when both --output and --output-dir given")
	}
}

// TestCompress_OutputDir_MultiInput verifies each file lands in outDir
// with its own basename (this is exactly the use case --output-dir enables
// that --output cannot: multiple inputs).
func TestCompress_OutputDir_MultiInput(t *testing.T) {
	srcDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "out")
	src1 := filepath.Join(srcDir, "first.log")
	src2 := filepath.Join(srcDir, "second.log")
	if err := os.WriteFile(src1, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src2, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := NewCompressCommand(&buf)
	cmd.SetArgs([]string{src1, src2, "--output-dir=" + outDir, "--json", "--level=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("compress multi: %v\nout: %s", err, buf.String())
	}

	for _, base := range []string{"first.log.zst", "second.log.zst"} {
		if _, err := os.Stat(filepath.Join(outDir, base)); err != nil {
			t.Errorf("missing %s: %v", base, err)
		}
	}
}
