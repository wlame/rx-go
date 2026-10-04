package clicommand

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
