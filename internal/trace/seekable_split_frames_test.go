package trace

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/pkg/rxtypes"
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
		// Frame 0 ends mid-line.
		"cut every 37 bytes": seekablefile.SplitEvery(text, 37),
		// Frame 0 ends at a line break.
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

// longLineNumber is the line of splitLinesLog that is longer than any
// frame the tests below cut, with NEEDLE in its middle.
const longLineNumber = 700

// splitLinesLog is a log of 2000 lines reading "LINE <n> <word> <hex>",
// where the word is NEEDLE on every 40th line. Line longLineNumber
// holds about 20 KB of padding on both sides of a NEEDLE.
func splitLinesLog() []byte {
	var b bytes.Buffer
	for n := 1; n <= 2000; n++ {
		word := "filler"
		if n%40 == 0 {
			word = "NEEDLE"
		}
		fmt.Fprintf(&b, "LINE %d %s %08x", n, word, uint32(n)*2654435761)
		if n == longLineNumber {
			fmt.Fprintf(&b, " %s NEEDLE %s", strings.Repeat("a", 20<<10), strings.Repeat("z", 20<<10))
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// textLine is one line of a text as a scan of the plain file sees it.
type textLine struct {
	number int
	offset int64
	text   string
}

// linesOf splits text into its lines, without their line breaks.
func linesOf(text []byte) []textLine {
	var out []textLine
	var offset int64
	for n, line := range strings.SplitAfter(string(text), "\n") {
		if line == "" {
			break
		}
		out = append(out, textLine{number: n + 1, offset: offset, text: strings.TrimSuffix(line, "\n")})
		offset += int64(len(line))
	}
	return out
}

// plainMatches is what a scan of the plain text reports for re: every
// line that re matches, whole, at the line's own offset and number,
// with each match in the line as a submatch.
func plainMatches(text []byte, re *regexp.Regexp) []MatchRaw {
	var out []MatchRaw
	for _, line := range linesOf(text) {
		found := re.FindAllStringIndex(line.text, -1)
		if found == nil {
			continue
		}
		subs := make([]rxtypes.Submatch, len(found))
		for i, f := range found {
			subs[i] = rxtypes.Submatch{Text: line.text[f[0]:f[1]], Start: f[0], End: f[1]}
		}
		out = append(out, MatchRaw{Offset: line.offset, AbsoluteLine: line.number, LineText: line.text, Submatches: subs})
	}
	return out
}

// cutsThroughEveryMatch cuts text in the middle of every occurrence of
// word, and also every step bytes, so frames are small enough that
// batch boundaries fall at such cuts too.
func cutsThroughEveryMatch(text []byte, word string, step int) []int {
	cuts := map[int]bool{}
	for at := 0; ; {
		i := bytes.Index(text[at:], []byte(word))
		if i < 0 {
			break
		}
		cuts[at+i+len(word)/2] = true
		at += i + len(word)
	}
	for cut := step; cut < len(text); cut += step {
		cuts[cut] = true
	}
	return slices.Sorted(maps.Keys(cuts))
}

// batchBoundaryInside cuts text every step bytes, except that the cut
// starting the second batch of frames lands at position at.
func batchBoundaryInside(text []byte, at, step int) [][]byte {
	var cuts []int
	for i := framesPerBatch - 1; i >= 0; i-- {
		cuts = append(cuts, at-i*step)
	}
	for cut := at + step; cut < len(text); cut += step {
		cuts = append(cuts, cut)
	}
	return seekablefile.SplitAt(text, cuts...)
}

// A frame boundary of a seekable file from another encoder can fall
// anywhere in a line, and a line can be longer than a frame. The scan
// still reports each matching line once, whole, at the offset and line
// number the plain text gives it, with its submatches counted from the
// start of the line: a match that a frame boundary cuts in two is
// found, and a fragment of a line never matches on its own.
func TestProcessSeekable_LinesThatFramesSplitAreScannedWhole(t *testing.T) {
	requireRipgrep(t)
	text := splitLinesLog()
	lines := linesOf(text)
	needle := lines[119] // line 120, far enough in for a hundred small frames before it
	needleAt := int(needle.offset) + strings.Index(needle.text, "NEEDLE") + 3
	lineAt := func(n int) textLine { return lines[n-1] }

	layouts := map[string][][]byte{
		"cut every 37 bytes":  seekablefile.SplitEvery(text, 37),
		"cut every 4 KB":      seekablefile.SplitEvery(text, 4<<10),
		"cut through matches": seekablefile.SplitAt(text, cutsThroughEveryMatch(text, "NEEDLE", 101)...),
		"first frame aligned": append(
			[][]byte{[]byte(lineAt(1).text + "\n")},
			seekablefile.SplitEvery(text[lineAt(2).offset:], 37)...),
		"one line per frame": seekablefile.SplitAt(text, func() []int {
			var cuts []int
			for _, l := range lines[1:] {
				cuts = append(cuts, int(l.offset))
			}
			return cuts
		}()...),
		"batch boundary inside a match": batchBoundaryInside(text, needleAt, 13),
	}
	patterns := map[string]string{
		"a word":                `NEEDLE`,
		"every whole line":      `LINE \d+ \w+ [0-9a-f]{8}`,
		"the long line's end":   `z{5}$`,
		"a line start fragment": `^[a-z0-9]`,
	}

	for layoutName, frames := range layouts {
		path := filepath.Join(t.TempDir(), "split.log.zst")
		seekablefile.Write(t, path, frames)
		tbl, err := readSeekTable(path)
		if err != nil {
			t.Fatalf("readSeekTable: %v", err)
		}
		for patternName, pattern := range patterns {
			t.Run(layoutName+"/"+patternName, func(t *testing.T) {
				matches, contexts, _, err := ProcessSeekable(
					context.Background(), path,
					map[string]string{"p1": pattern}, []string{"p1"},
					nil, 1, 1, nil,
				)
				if err != nil {
					t.Fatalf("ProcessSeekable: %v", err)
				}
				want := plainMatches(text, regexp.MustCompile(pattern))
				if len(matches) != len(want) {
					t.Errorf("got %d matches, want %d", len(matches), len(want))
				}
				for i := range min(len(matches), len(want)) {
					got, w := matches[i], want[i]
					if got.Offset != w.Offset || got.AbsoluteLine != w.AbsoluteLine ||
						got.LineText != w.LineText || !reflect.DeepEqual(got.Submatches, w.Submatches) {
						t.Errorf("match %d: got byte %d line %d %.60q %v\nwant byte %d line %d %.60q %v",
							i, got.Offset, got.AbsoluteLine, got.LineText, got.Submatches,
							w.Offset, w.AbsoluteLine, w.LineText, w.Submatches)
						break
					}
					if frame := tbl.Frames[got.FrameIndex]; got.Offset < frame.DecompressedOffset ||
						got.Offset >= frame.DecompressedOffset+frame.DecompressedSize {
						t.Errorf("match at byte %d names frame %d, which does not hold it", got.Offset, got.FrameIndex)
					}
				}
				for _, c := range contexts {
					if c.AbsoluteLine < 1 || c.AbsoluteLine > len(lines) {
						t.Errorf("context at byte %d: line %d is not a line of the text", c.Offset, c.AbsoluteLine)
						continue
					}
					if l := lineAt(c.AbsoluteLine); c.Offset != l.offset || c.LineText != l.text {
						t.Errorf("context line %d: got byte %d %.60q, want byte %d %.60q",
							c.AbsoluteLine, c.Offset, c.LineText, l.offset, l.text)
					}
				}
			})
		}
	}
}
