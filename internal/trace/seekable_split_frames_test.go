package trace

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// splitFramesLog is a log whose every line reads "LINE <n> ...", with
// line 20 far longer than the frames the test cuts, so that line spans
// frames that hold no line break.
func splitFramesLog() []byte {
	var b bytes.Buffer
	for n := 1; n <= 60; n++ {
		fmt.Fprintf(&b, "LINE %d %s", n, strings.Repeat("x", n%11))
		if n == 20 {
			b.WriteString(strings.Repeat(" long", 80))
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// A seekable file from another encoder can cut a frame anywhere,
// including inside a line longer than a frame, so some frames hold no
// line break at all. Such a frame still moves the count on by zero
// lines, not by one and not by "unknown": every match and context line
// a full scan reports carries the number of the line that holds its
// offset in the text.
func TestProcessSeekable_FramesThatSplitLinesAreNumberedAsTheText(t *testing.T) {
	requireRipgrep(t)
	text := splitFramesLog()
	firstLineEnd := bytes.IndexByte(text, '\n') + 1
	layouts := map[string][][]byte{
		// Frame 0 ends mid-line, so the scan reads one frame at a time.
		"cut every 37 bytes": seekablefile.SplitEvery(text, 37),
		// Frame 0 ends at a line break, so the scan batches frames.
		"first frame aligned": append(
			[][]byte{text[:firstLineEnd]},
			seekablefile.SplitEvery(text[firstLineEnd:], 37)...),
	}
	lineHolding := func(offset int64) int {
		return bytes.Count(text[:offset], []byte{'\n'}) + 1
	}

	for name, frames := range layouts {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "split.log.zst")
			seekablefile.Write(t, path, frames)

			matches, contexts, _, err := ProcessSeekable(
				context.Background(), path,
				map[string]string{"p1": "LINE"}, []string{"p1"},
				nil, 1, 1, nil,
			)
			if err != nil {
				t.Fatalf("ProcessSeekable: %v", err)
			}
			if len(matches) < 30 {
				t.Fatalf("got %d matches; the fixture has 60 lines", len(matches))
			}
			for _, m := range matches {
				if want := lineHolding(m.Offset); m.AbsoluteLine != want {
					t.Errorf("match at byte %d (%q): line %d, want %d", m.Offset, m.LineText, m.AbsoluteLine, want)
				}
			}
			for _, c := range contexts {
				if want := lineHolding(c.Offset); c.AbsoluteLine != want {
					t.Errorf("context at byte %d (%q): line %d, want %d", c.Offset, c.LineText, c.AbsoluteLine, want)
				}
			}
		})
	}
}
