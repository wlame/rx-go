package trace

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// A seekable file is searched frame by frame whichever footer layout
// ends its seek table: the zstd seekable format specification's (with
// or without per-frame checksums) or the one rx wrote before. Trace
// scans it as frames (file_chunks is the frame count) and answers the plain text's
// matches, samples reads a line through the table, and an index records
// the file's frames.
func TestTraceSamplesAndIndexReadASeekableFileOfEveryFooterLayout(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	text := numberedLog(3000, 37)
	lines := linesOf(text)
	frames := seekablefile.SplitEvery(text, 16<<10)
	var want []textLine
	for _, line := range lines {
		if strings.Contains(line.text, "NEEDLE") {
			want = append(want, line)
		}
	}
	layouts := map[string]seekablefile.Footer{
		"spec layout":                seekablefile.SpecFooter,
		"spec layout with checksums": seekablefile.SpecFooterWithChecksums,
		"legacy rx layout":           seekablefile.LegacyRxFooter,
	}
	for name, footer := range layouts {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.log.zst")
			seekablefile.WriteWithFooter(t, path, frames, footer)

			resp := traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
			if chunks := resp.FileChunks["f1"]; chunks != len(frames) {
				t.Errorf("file_chunks = %d, want one per frame (%d)", chunks, len(frames))
			}
			if len(resp.SkippedFiles) != 0 {
				t.Errorf("skipped_files = %v, want none", resp.SkippedFiles)
			}
			if len(resp.Matches) != len(want) {
				t.Fatalf("got %d matches, want %d", len(resp.Matches), len(want))
			}
			for i, m := range resp.Matches {
				if m.Offset != want[i].offset || m.AbsoluteLineNumber != want[i].number || derefText(m.LineText) != want[i].text {
					t.Fatalf("match %d: byte %d line %d %q, want byte %d line %d %q", i,
						m.Offset, m.AbsoluteLineNumber, derefText(m.LineText), want[i].offset, want[i].number, want[i].text)
				}
			}

			late := lines[len(lines)-10]
			if got, err := samplesLine(t, path, int64(late.number)); err != nil || got != late.text {
				t.Errorf("samples line %d = %q, %v; want %q", late.number, got, err, late.text)
			}

			idx, err := index.Build(path, index.BuildOptions{})
			if err != nil {
				t.Fatalf("index: %v", err)
			}
			if idx.Frames == nil || len(*idx.Frames) != len(frames) {
				t.Errorf("index frames = %v, want %d frames", idx.Frames, len(frames))
			}
			if idx.LineCount == nil || *idx.LineCount != int64(len(lines)) {
				t.Errorf("index line_count = %v, want %d", idx.LineCount, len(lines))
			}
		})
	}
}
