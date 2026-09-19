package clicommand

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// A symbolic link at the output name holds it even when it leads
// nowhere. Without --force the run is refused and nothing is created
// where the link leads; with --force the link itself is replaced by the
// output, and still nothing is written where it led.
func TestCompress_LinkAtTheOutputNameIsNeverWrittenThrough(t *testing.T) {
	text := defaultNameText()
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", filepath.Join(dir, "cache"))
	input := writeInput(t, dir, "app.log", text)
	output := filepath.Join(dir, "app.log.zst")
	target := filepath.Join(dir, "elsewhere.zst")
	if err := os.Symlink(target, output); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	entry, err := compressOne(t, compressParams{paths: []string{input}, frameSize: "16K", level: 1, workers: 1})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitGenericError {
		t.Fatalf("error: got %v, want exit code %d", err, ExitGenericError)
	}
	if want := "output file already exists: " + output + " (use --force to overwrite)"; entry["error"] != want {
		t.Errorf("error = %v, want %q", entry["error"], want)
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		t.Errorf("the refused run created %s through the link", target)
	}

	entry, err = compressOne(t, compressParams{paths: []string{input}, frameSize: "16K", level: 1, workers: 1, force: true})
	if err != nil {
		t.Fatalf("runCompress --force: %v (%v)", err, entry["error"])
	}
	info, err := os.Lstat(output)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file after --force: %v", output, err)
	}
	if got := seekableFileText(t, output); !bytes.Equal(got, text) {
		t.Errorf("%s holds %d bytes of text, want %d", output, len(got), len(text))
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		t.Errorf("--force created %s through the link", target)
	}
}

// Two inputs of one command that would write the same output are
// refused before anything is written, with or without --force: the
// second would silently replace the first one's output.
func TestCompress_TwoInputsWithOneOutputAreRefusedBeforeAnythingIsWritten(t *testing.T) {
	text := defaultNameText()
	cases := []struct {
		name      string
		inputs    []string // names in the fixture directory
		outputDir string   // below the fixture directory, "" for none
		force     bool
	}{
		{"default names", []string{"app.log", "app.log.gz"}, "", false},
		{"default names with --force", []string{"app.log", "app.log.gz"}, "", true},
		{"one input twice", []string{"app.log", "app.log"}, "", true},
		{"--output-dir", []string{"app.log", "app.log.gz"}, "out", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("RX_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
			writeInput(t, dir, "app.log", text)
			writeInput(t, dir, "app.log.gz", compressedcopy.Encode(t, compressedcopy.Gzip, text))
			var inputs []string
			for _, name := range tc.inputs {
				inputs = append(inputs, filepath.Join(dir, name))
			}
			outputDir := ""
			if tc.outputDir != "" {
				outputDir = filepath.Join(dir, tc.outputDir)
			}

			var out bytes.Buffer
			err := runCompress(context.Background(), &out, compressParams{
				paths: inputs, outputDir: outputDir, frameSize: "16K", level: 1, workers: 1, force: tc.force,
			})

			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
				t.Fatalf("error: got %v, want exit code %d", err, ExitUsageError)
			}
			if !strings.Contains(err.Error(), "app.log.zst") {
				t.Errorf("error %q does not name the shared output", err)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q, want nothing", out.String())
			}
			requireNoFile(t, filepath.Join(dir, "app.log.zst"))
			if outputDir != "" {
				requireNoFile(t, outputDir)
			}
		})
	}
}

// A command whose context ends (Ctrl-C, SIGTERM) stops, and leaves
// nothing under the output name.
func TestCompress_CanceledCommandWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	input := writeInput(t, dir, "app.log", defaultNameText())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	_ = runCompress(ctx, &out, compressParams{paths: []string{input}, frameSize: "16K", level: 1, workers: 1})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("%s holds %d entries, want only the input", dir, len(entries))
	}
}

// requireNoFile fails when anything exists at path.
func requireNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("%s was created", path)
	}
}
