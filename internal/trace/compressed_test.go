package trace

import (
	"bytes"
	"compress/gzip"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
)

// writeGzipFile writes content gzip-compressed into a fresh tmp file
// and returns its path. Uses pure-Go compress/gzip — no external
// `gzip` binary required on the test host. This matches the production
// path (Stage 8 Finding 4) which also no longer shells out.
func writeGzipFile(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "log.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(content); err != nil {
		_ = gz.Close()
		_ = f.Close()
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		t.Fatalf("gzip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}
	return p
}

// TestProcessCompressed_Gzip runs the full decompress → rg pipeline
// on a gzipped file. Post-consolidation (Stage 8 Finding 4) this does
// NOT require the external `gzip` binary — the pipeline uses
// compress/gzip from the stdlib.
func TestProcessCompressed_Gzip(t *testing.T) {
	requireRipgrep(t)
	content := []byte("alpha error\nbeta\ngamma error\ndelta\n")
	p := writeGzipFile(t, content)

	matches, _, _, err := ProcessCompressed(
		context.Background(),
		p, compression.FormatGzip,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if err != nil {
		t.Fatalf("ProcessCompressed: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(matches), matches)
	}
	// IsCompressed flag must be set for downstream cache handling.
	for _, m := range matches {
		if !m.IsCompressed {
			t.Errorf("IsCompressed = false, want true")
		}
	}
}

// TestProcessCompressed_MaxResultsTruncates verifies the maxResults
// early-exit path. No external decompressor binary required.
func TestProcessCompressed_MaxResultsTruncates(t *testing.T) {
	requireRipgrep(t)
	// 20 matching lines; maxResults=3 should stop after 3.
	content := []byte{}
	for i := 0; i < 20; i++ {
		content = append(content, []byte("error line\n")...)
	}
	p := writeGzipFile(t, content)
	cap := 3
	matches, _, _, err := ProcessCompressed(
		context.Background(),
		p, compression.FormatGzip,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, &cap,
	)
	if err != nil {
		t.Fatalf("ProcessCompressed: %v", err)
	}
	if len(matches) != 3 {
		t.Errorf("got %d matches, want 3", len(matches))
	}
}

// TestProcessCompressed_NonCompressedError rejects plain text inputs.
func TestProcessCompressed_NonCompressedError(t *testing.T) {
	p := mustWriteFile(t, []byte("plain text\n"))
	_, _, _, err := ProcessCompressed(
		context.Background(),
		p, compression.FormatNone,
		map[string]string{"p1": "text"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if err == nil {
		t.Errorf("want error when format is None, got nil")
	}
}

// writeCorruptGzipFile produces a gzip file whose HEADER is valid
// (gzip.NewReader succeeds) but whose DEFLATE stream is corrupted
// mid-stream. Reading far enough surfaces "flate: corrupt input"
// from the decompressor via io.Copy's Read path — exercising
// R2M3's silent-error-swallowing bug.
func writeCorruptGzipFile(t *testing.T, body []byte) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	raw := buf.Bytes()
	if len(raw) < 20 {
		t.Fatalf("compressed payload too short to corrupt: %d", len(raw))
	}
	// Overwrite a block of deflate bytes near the middle of the stream
	// with 0xFF. Keep the 10-byte gzip header intact so NewReader
	// succeeds; keep the 8-byte trailer intact so the trailer-parse
	// step doesn't fail for a different reason. The middle corruption
	// triggers the "flate: corrupt input" branch during Read.
	start := 12
	end := len(raw) - 10
	if end <= start {
		t.Fatalf("not enough room to corrupt middle bytes")
	}
	for i := start; i < end; i++ {
		raw[i] = 0xff
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "corrupt.gz")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// slogCapture collects structured-log records emitted during a test.
// Concurrency-safe so it can be read from both the scan goroutine
// and the test body.
type slogCapture struct {
	mu      sync.Mutex
	records []slogRecord
}

// slogRecord snapshots the fields we care about from a slog.Record.
// Stored as plain strings so test assertions stay readable.
type slogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

// Handler returns a slog.Handler that appends every record into the
// capture. The Handler is safe for concurrent use.
func (c *slogCapture) Handler() slog.Handler {
	return &captureHandler{c: c}
}

// captureHandler is the minimal slog.Handler that feeds records into
// the capture. We implement only what's needed for this test —
// there's no WithAttrs/WithGroup composition in the trace call site.
type captureHandler struct {
	c *slogCapture
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := slogRecord{
		Level: r.Level,
		Msg:   r.Message,
		Attrs: map[string]string{},
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.String()
		return true
	})
	h.c.mu.Lock()
	h.c.records = append(h.c.records, rec)
	h.c.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

// recordsWithLevel returns the subset of captured records at the
// given level. Used for focused assertions.
func (c *slogCapture) recordsWithLevel(l slog.Level) []slogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []slogRecord{}
	for _, r := range c.records {
		if r.Level == l {
			out = append(out, r)
		}
	}
	return out
}

// installSlogCapture redirects slog.Default() to a fresh capture for
// the duration of the test. The caller receives the capture for
// assertion. Previous default is restored on test cleanup.
func installSlogCapture(t *testing.T) *slogCapture {
	t.Helper()
	prev := slog.Default()
	cap := &slogCapture{}
	slog.SetDefault(slog.New(cap.Handler()))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return cap
}

// TestProcessCompressed_CorruptStreamLogsWarning is the R2M3 regression
// guard. Before the fix, an io.Copy error (e.g. "flate: corrupt input"
// from a mid-stream-corrupted gzip file) was captured into copyErr but
// silently discarded via `_ = copyErr`. Operators saw zero indication
// that a file was truncated / corrupted — only an empty result set.
//
// After the fix, the error surfaces as a slog.Warn record including
// the file path, compression format, and the underlying Read error.
// The call itself still returns nil error with empty results, matching
// Python's "log warning and move on" behavior.
func TestProcessCompressed_CorruptStreamLogsWarning(t *testing.T) {
	requireRipgrep(t)
	cap := installSlogCapture(t)

	// Build a body big enough that rg sees "error" matches in the
	// uncorrupted prefix, but the middle-corruption guarantees the
	// decoder surfaces an error before EOF.
	body := bytes.Repeat([]byte("alpha error line\nbeta\ngamma\n"), 200)
	p := writeCorruptGzipFile(t, body)

	matches, _, _, err := ProcessCompressed(
		context.Background(),
		p, compression.FormatGzip,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	// The call itself must not fail — we're matching Python's
	// degrade-on-corruption contract.
	if err != nil {
		t.Fatalf("ProcessCompressed returned error, want nil (degrade-gracefully): %v", err)
	}
	// Matches may be empty or partial; what we assert is ONLY that
	// the corruption was logged.
	_ = matches

	warns := cap.recordsWithLevel(slog.LevelWarn)
	if len(warns) == 0 {
		t.Fatalf("expected at least one Warn-level slog record for corrupt stream; got 0")
	}
	// At least one Warn record must mention the path and the format.
	var found bool
	for _, w := range warns {
		pathOK := strings.Contains(w.Attrs["path"], "corrupt.gz")
		fmtOK := w.Attrs["format"] == "gzip"
		errOK := w.Attrs["error"] != ""
		if pathOK && fmtOK && errOK {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no Warn record includes path+format+error; got records: %+v", warns)
	}
}

// TestProcessCompressed_CleanStreamEmitsNoWarning is the complement:
// a valid gzip stream must NOT trigger the corruption Warn path.
// Prevents a future change from accidentally logging on every file.
func TestProcessCompressed_CleanStreamEmitsNoWarning(t *testing.T) {
	requireRipgrep(t)
	cap := installSlogCapture(t)

	p := writeGzipFile(t, []byte("alpha error\nbeta\ngamma error\n"))
	_, _, _, err := ProcessCompressed(
		context.Background(),
		p, compression.FormatGzip,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if err != nil {
		t.Fatalf("ProcessCompressed: %v", err)
	}
	// Filter to records that look like OUR corruption warn specifically.
	// Other unrelated Warn records elsewhere in the pipeline should
	// not make this test fail if this function doesn't emit one.
	warns := cap.recordsWithLevel(slog.LevelWarn)
	for _, w := range warns {
		if strings.Contains(w.Msg, "compressed_stream_copy_error") ||
			strings.Contains(w.Msg, "decompression_corruption") {
			t.Fatalf("clean stream emitted corruption-warn record: %+v", w)
		}
	}
}

// TestEngine_Run_Gzip hits the compressed bucket end-to-end through
// the engine dispatcher. No external `gzip` binary required — the
// decompressor is pure-Go compress/gzip (Stage 8 Finding 4).
func TestEngine_Run_Gzip(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	p := writeGzipFile(t, []byte("alpha error\nbeta\n"))
	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) != 1 {
		t.Errorf("got %d matches, want 1", len(resp.Matches))
	}
	// Files map should map f1 to the gzip path.
	if resp.Files["f1"] != p {
		t.Errorf("Files[f1] = %q, want %q", resp.Files["f1"], p)
	}
}
