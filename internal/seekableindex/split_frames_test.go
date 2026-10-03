package seekableindex

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// splitLog is a log whose every line reads "LINE <n> ...", with line 40
// far longer than the small frames the tests cut, so that line spans
// several frames none of which holds a line break.
func splitLog(lines int, finalNewline bool) []byte {
	var b strings.Builder
	for n := 1; n <= lines; n++ {
		fmt.Fprintf(&b, "LINE %d %s", n, strings.Repeat("x", n%13))
		if n == 40 {
			b.WriteString(strings.Repeat("long ", 60))
		}
		if n < lines || finalNewline {
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// lineHolding returns the 1-based number of the line that holds byte
// offset of text: one more than the line breaks before it.
func lineHolding(text []byte, offset int64) int64 {
	return int64(bytes.Count(text[:offset], []byte{'\n'})) + 1
}

// lineCountOf is the number of lines in text: its line breaks, plus one
// for a last line that no break ends.
func lineCountOf(text []byte) int64 {
	count := int64(bytes.Count(text, []byte{'\n'}))
	if len(text) > 0 && text[len(text)-1] != '\n' {
		count++
	}
	return count
}

// A seekable file written by another encoder can cut a frame anywhere:
// in the middle of a line, inside a line longer than a frame, or into
// an empty frame. Every number the index records must still be the
// text's own: a frame's first line is the line holding its first byte,
// its last line the last one it ends, and each checkpoint names the
// line holding its byte.
func TestBuild_FramesThatSplitLinesAreNumberedAsTheText(t *testing.T) {
	terminated := splitLog(60, true)
	unterminated := splitLog(60, false)
	cases := []struct {
		name   string
		text   []byte
		frames [][]byte
	}{
		{"cut every 37 bytes", terminated, seekablefile.SplitEvery(terminated, 37)},
		{"a line longer than a frame", terminated, seekablefile.SplitEvery(terminated, 64)},
		{"an empty frame mid-line", terminated, seekablefile.SplitAt(terminated, 100, 205, 205, 400)},
		{"no final line break", unterminated, seekablefile.SplitEvery(unterminated, 37)},
		{"an empty last frame after an unterminated line", unterminated,
			seekablefile.SplitAt(unterminated, 500, len(unterminated)-3, len(unterminated))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zstPath := filepath.Join(t.TempDir(), "split.log.zst")
			seekablefile.Write(t, zstPath, tc.frames)

			got, err := buildPath(zstPath)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if want := lineCountOf(tc.text); got.LineCount != want {
				t.Errorf("line_count = %d, want %d", got.LineCount, want)
			}
			for i, frame := range got.Frames {
				start := frame.DecompressedOffset
				end := start + frame.DecompressedSize
				wantFirst := lineHolding(tc.text, start)
				wantLast := int64(bytes.Count(tc.text[:end], []byte{'\n'}))
				if i == len(got.Frames)-1 {
					wantLast = lineCountOf(tc.text)
				}
				if frame.FirstLine != wantFirst || frame.LastLine != wantLast {
					t.Errorf("frame %d (bytes %d-%d): lines %d-%d, want %d-%d",
						i, start, end, frame.FirstLine, frame.LastLine, wantFirst, wantLast)
				}
				if frame.LineCount != frame.LastLine-frame.FirstLine+1 {
					t.Errorf("frame %d: line_count %d does not match lines %d-%d",
						i, frame.LineCount, frame.FirstLine, frame.LastLine)
				}
			}
			for _, entry := range got.LineIndex {
				if want := lineHolding(tc.text, entry.ByteOffset); entry.LineNumber != want {
					t.Errorf("checkpoint at byte %d names line %d, want %d",
						entry.ByteOffset, entry.LineNumber, want)
				}
			}
		})
	}
}
