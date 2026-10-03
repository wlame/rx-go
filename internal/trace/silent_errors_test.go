package trace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
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

// A line of ripgrep's output for a seekable batch that the parser
// cannot read must fail the batch rather than silently drop every match
// after it.
func TestReadBatchEvents_ReportsAnUnreadableStream(t *testing.T) {
	out := []byte(`{"type":"begin","data":{}}` + "\n" + "this is not json" + "\n" +
		`{"type":"match","data":{"lines":{"text":"x\\n"},"line_number":1,"absolute_offset":0,"submatches":[]}}` + "\n")

	_, err := readBatchEvents(context.Background(), bytes.NewReader(out))

	if !errors.Is(err, ErrMalformedEvent) {
		t.Errorf("readBatchEvents returned %v, want ErrMalformedEvent", err)
	}
}
