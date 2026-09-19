package clicommand

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

// `rx samples` has no lines to give from a file whose text is not text:
// it refuses a .tar.gz the way it refuses a directory, with exit code 2
// and the reason, and stores no index for it.
func TestSamples_RefusesAFileThatIsNotText(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cache)
	archive := archiveFixture(t)

	var out bytes.Buffer
	err := runSamples(&out, samplesParams{path: archive, lines: []string{"5"}})

	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitUsageError {
		t.Fatalf("got %v, want exit code %d", err, ExitUsageError)
	}
	if want := archiveReason + ": " + archive; exitErr.Error() != want {
		t.Errorf("message: got %q, want %q", exitErr.Error(), want)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q for a refused file", out.String())
	}
	stored, _ := filepath.Glob(filepath.Join(cache, "indexes", "*"))
	if len(stored) != 0 {
		t.Errorf("indexes stored for a refused file: %v", stored)
	}
}
