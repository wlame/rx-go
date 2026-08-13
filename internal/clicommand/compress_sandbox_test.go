package clicommand

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// compressSandboxFixture builds a root/outside pair with one input file
// inside the root and installs root as the only search root.
func compressSandboxFixture(t *testing.T) (root, outside, input string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	input = filepath.Join(root, "a.log")
	if err := os.WriteFile(input, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return root, outside, input
}

// TestCompress_OutputOutsideSearchRootFails asserts `rx compress
// --output=<outside>` refuses to write when a sandbox is configured.
func TestCompress_OutputOutsideSearchRootFails(t *testing.T) {
	_, outside, input := compressSandboxFixture(t)
	out := filepath.Join(outside, "x.zst")

	var buf bytes.Buffer
	err := runCompress(&buf, compressParams{
		paths:      []string{input},
		output:     out,
		frameSize:  "4K",
		level:      3,
		workers:    1,
		jsonOutput: true,
	})
	if err == nil {
		t.Errorf("runCompress: got nil error, want failure")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("output file %s was written outside the sandbox", out)
	}

	var result compressResult
	if decodeErr := json.Unmarshal(buf.Bytes(), &result); decodeErr != nil {
		t.Fatalf("decode json: %v (%s)", decodeErr, buf.String())
	}
	if len(result.Files) != 1 {
		t.Fatalf("files: got %d entries, want 1", len(result.Files))
	}
	entry := result.Files[0]
	if ok, _ := entry["success"].(bool); ok {
		t.Errorf("success: got true, want false")
	}
	msg, _ := entry["error"].(string)
	if msg == "" {
		t.Errorf("error field is empty: %v", entry)
	}
}

// TestCompress_OutputDirOutsideSearchRootFails covers the --output-dir
// spelling of the same escape.
func TestCompress_OutputDirOutsideSearchRootFails(t *testing.T) {
	_, outside, input := compressSandboxFixture(t)

	var buf bytes.Buffer
	err := runCompress(&buf, compressParams{
		paths:      []string{input},
		outputDir:  outside,
		frameSize:  "4K",
		level:      3,
		workers:    1,
		jsonOutput: true,
	})
	if err == nil {
		t.Errorf("runCompress: got nil error, want failure")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "a.log.zst")); statErr == nil {
		t.Errorf("output file was written outside the sandbox")
	}
}

// TestCompress_OutputInsideSearchRootSucceeds is the positive control.
func TestCompress_OutputInsideSearchRootSucceeds(t *testing.T) {
	root, _, input := compressSandboxFixture(t)
	out := filepath.Join(root, "x.zst")

	var buf bytes.Buffer
	if err := runCompress(&buf, compressParams{
		paths:      []string{input},
		output:     out,
		frameSize:  "4K",
		level:      3,
		workers:    1,
		jsonOutput: true,
	}); err != nil {
		t.Fatalf("runCompress: %v (%s)", err, buf.String())
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Errorf("output file %s was not written: %v", out, statErr)
	}
}

// TestCompress_OutputUnrestrictedWithoutSearchRoot keeps the default
// CLI behaviour: with no sandbox configured, any output path is allowed.
func TestCompress_OutputUnrestrictedWithoutSearchRoot(t *testing.T) {
	paths.Reset()
	base := t.TempDir()
	input := filepath.Join(base, "a.log")
	if err := os.WriteFile(input, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	out := filepath.Join(base, "elsewhere.zst")

	var buf bytes.Buffer
	if err := runCompress(&buf, compressParams{
		paths:      []string{input},
		output:     out,
		frameSize:  "4K",
		level:      3,
		workers:    1,
		jsonOutput: true,
	}); err != nil {
		t.Fatalf("runCompress: %v (%s)", err, buf.String())
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Errorf("output file %s was not written: %v", out, statErr)
	}
}
