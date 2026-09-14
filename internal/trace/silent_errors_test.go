package trace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/seekable"
)

// captureDefaultLog sends slog's default logger to a buffer for the
// rest of the test.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// A trace cache that cannot be written only costs the next trace a
// scan, but an operator whose disk is full needs to hear about it once,
// not once per file.
func TestSaveScannedFile_FailureWarnsOnce(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	// The cache directory would be created under a regular file, which
	// fails on every write.
	t.Setenv("RX_CACHE_DIR", blocked)
	log := captureDefaultLog(t)
	cacheWriteWarned.Store(false)
	t.Cleanup(func() { cacheWriteWarned.Store(false) })

	path := mustWriteFile(t, []byte("one\ntwo\n"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	scan := ScannedFile{Path: path, Source: index.IdentityFromInfo(path, info), Chunks: 1}

	SaveScannedFile(scan, []string{"one"}, nil)
	SaveScannedFile(scan, []string{"two"}, nil)

	if got := strings.Count(log.String(), "trace_cache_write_failed"); got != 1 {
		t.Errorf("warnings logged: %d, want 1:\n%s", got, log.String())
	}
}

// ripgrep's output for a seekable batch is parsed after rg exits. A
// line the parser cannot read (one longer than its buffer) must fail
// the batch rather than silently drop every match after it.
func TestRemapBatchEvents_ReportsAnUnreadableStream(t *testing.T) {
	tooLong := strings.Repeat("x", 17*1024*1024)
	out := []byte(`{"type":"begin","data":{}}` + "\n" + tooLong + "\n")
	locs := []frameLoc{{frameIdx: 0, info: seekable.FrameInfo{DecompressedSize: 100}}}

	_, _, err := remapBatchEvents(context.Background(), out, locs, []string{"p1"})

	if err == nil {
		t.Error("remapBatchEvents returned no error for a stream it could not read")
	}
}
