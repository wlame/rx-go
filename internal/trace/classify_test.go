package trace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// A search reads each file by what its bytes are, the rule every command
// applies: text named .gz and gzip named .log are both searched as the
// text they hold, a seekable zstd file is searched frame by frame
// whatever its name, and a file whose text is not text (a .tar.gz, UTF-16,
// a NUL byte in the first 8 KiB of a compressed copy's text, as of a
// plain file's) is skipped.
func TestTraceClassifiesEachFileByItsBytes(t *testing.T) {
	requireRipgrep(t)
	text := []byte("LINE 1 NEEDLE one\nLINE 2 other\n")
	nulText := append([]byte("LINE 1 NEEDLE\x00\n"), text...)
	var seekableBody bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 16, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &seekableBody); err != nil {
		t.Fatalf("encode: %v", err)
	}
	gzip := func(body []byte) []byte { return compressedcopy.Encode(t, compressedcopy.Gzip, body) }
	cases := []struct {
		name        string
		body        []byte
		wantMatches int
		wantSkipped bool
		wantChunks  int
	}{
		{"text-named.log.gz", text, 1, false, 1},
		{"gzip-named.log", gzip(text), 1, false, 1},
		{"seekable-named.log", seekableBody.Bytes(), 1, false, 2},
		{"logs.tar.gz", gzip(append([]byte("NEEDLE"), make([]byte, 506)...)), 0, true, 0},
		{"utf16.log", []byte{0xFF, 0xFE, 'N', 0, 'E', 0, 'E', 0, 'D', 0, 'L', 0, 'E', 0, '\n', 0}, 0, true, 0},
		{"nul.log", nulText, 0, true, 0},
		{"nul.log.gz", gzip(nulText), 0, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, tc.body, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
			if len(resp.Matches) != tc.wantMatches {
				t.Errorf("matches: got %d, want %d", len(resp.Matches), tc.wantMatches)
			}
			if skipped := slices.Contains(resp.SkippedFiles, path); skipped != tc.wantSkipped {
				t.Errorf("skipped: got %v (%v), want %v", skipped, resp.SkippedFiles, tc.wantSkipped)
			}
			if tc.wantChunks > 0 && resp.FileChunks["f1"] != tc.wantChunks {
				t.Errorf("file_chunks: got %v, want %d", resp.FileChunks, tc.wantChunks)
			}
		})
	}
}
