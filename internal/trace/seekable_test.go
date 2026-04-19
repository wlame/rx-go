package trace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
)

// writeSeekableZstdFile encodes content as a seekable-zstd file via
// M2's encoder and returns the path. Test helper — lets the trace
// layer exercise its seekable path without external `t2sz` binary.
func writeSeekableZstdFile(t *testing.T, content []byte, frameSize int) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "log.zst")

	var buf bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{
		FrameSize: frameSize,
		Level:     1,
	})
	src := bytes.NewReader(content)
	if _, err := enc.Encode(context.Background(), src, int64(len(content)), &buf); err != nil {
		t.Fatalf("encode seekable zstd: %v", err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestProcessSeekable_BasicMatches scans a seekable-zstd file we
// produced ourselves. Each frame is line-aligned, batching applies,
// and offsets/lines come back translated into file coordinates.
func TestProcessSeekable_BasicMatches(t *testing.T) {
	requireRipgrep(t)

	// Build content with known error-line positions across 3 frames.
	var b strings.Builder
	for i := 0; i < 200; i++ {
		if i%50 == 49 {
			b.WriteString("line error ")
			b.WriteString("\n")
		} else {
			b.WriteString("ordinary line text here\n")
		}
	}
	content := []byte(b.String())
	p := writeSeekableZstdFile(t, content, 64*1024) // small frames → multiple frames

	matches, _, _, err := ProcessSeekable(
		context.Background(),
		p,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, nil,
	)
	if err != nil {
		t.Fatalf("ProcessSeekable: %v", err)
	}
	// Expected: matches on lines 50, 100, 150, 200 (4 total).
	if len(matches) != 4 {
		t.Errorf("got %d matches, want 4", len(matches))
	}
	for _, m := range matches {
		if !m.IsCompressed {
			t.Errorf("IsCompressed false on seekable match")
		}
		if m.LineText == "" {
			t.Errorf("empty line_text in seekable match: %+v", m)
		}
	}
}

// TestProcessSeekable_MaxResultsTruncates checks the cap after the
// per-frame post-sort pass.
func TestProcessSeekable_MaxResultsTruncates(t *testing.T) {
	requireRipgrep(t)
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("error here\n")
	}
	p := writeSeekableZstdFile(t, []byte(b.String()), 32*1024)

	cap := 5
	matches, _, _, err := ProcessSeekable(
		context.Background(),
		p,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, &cap,
	)
	if err != nil {
		t.Fatalf("ProcessSeekable: %v", err)
	}
	if len(matches) != 5 {
		t.Errorf("got %d matches, want 5", len(matches))
	}
}

// TestReadSeekTable_RoundTripsOurEncoder smoke-tests the wrapper.
func TestReadSeekTable_RoundTripsOurEncoder(t *testing.T) {
	content := []byte("a\nb\nc\nd\n")
	p := writeSeekableZstdFile(t, content, 4)
	tbl, err := readSeekTable(p)
	if err != nil {
		t.Fatalf("readSeekTable: %v", err)
	}
	if tbl.NumFrames == 0 {
		t.Errorf("NumFrames = 0, want > 0")
	}
}
