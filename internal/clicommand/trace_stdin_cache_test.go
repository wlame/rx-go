package clicommand

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// largePipedText is a little over 1 MiB of numbered lines, some holding
// NEEDLE: large enough for a trace cache entry under RX_LARGE_FILE_MB=1.
func largePipedText() string {
	var b strings.Builder
	for i := 1; b.Len() < 1<<20+4096; i++ {
		b.WriteString("LINE " + strconv.Itoa(i))
		if i%1000 == 0 {
			b.WriteString(" NEEDLE")
		}
		b.WriteString(" padding to give the line some width\n")
	}
	return b.String()
}

// traceCacheEntries lists the trace cache files under cacheDir.
func traceCacheEntries(t *testing.T, cacheDir string) []string {
	t.Helper()
	var entries []string
	root := filepath.Join(cacheDir, "rx", "trace_cache")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".json") {
			entries = append(entries, path)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk %s: %v", root, err)
	}
	return entries
}

// Piped input is spooled to a temporary file that is deleted when the
// command ends. A trace-cache entry for it could never be read again,
// so none is written.
func TestTraceCommand_PipedInputWritesNoTraceCacheEntry(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	restore := withStdin(t, largePipedText())
	defer restore()

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"-e", "NEEDLE", "-"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trace from stdin: %v", err)
	}
	if !strings.Contains(buf.String(), "NEEDLE") {
		t.Fatalf("expected matches from the piped input, got:\n%s", buf.String())
	}
	if entries := traceCacheEntries(t, cacheDir); len(entries) != 0 {
		t.Errorf("trace cache entries written for piped input: %v", entries)
	}
}

// Beside piped input, a named file is cached as usual.
func TestTraceCommand_PipedInputBesideAFileCachesOnlyTheFile(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	text := largePipedText()
	named := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(named, []byte(text), 0o600); err != nil {
		t.Fatalf("write %s: %v", named, err)
	}
	restore := withStdin(t, text)
	defer restore()

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"-e", "NEEDLE", named, "-"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trace: %v", err)
	}
	entries := traceCacheEntries(t, cacheDir)
	if len(entries) != 1 || !strings.HasSuffix(entries[0], "_app.log.json") {
		t.Errorf("trace cache entries = %v, want one for app.log", entries)
	}
}
