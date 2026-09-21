package samples

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// A seekable file from another encoder can cut its frames anywhere: in
// the middle of a line, inside a line longer than a frame, or into an
// empty frame. `samples` answers on such a file exactly as on the plain
// copy of its text, with and without an index.

// longLineLog is a log whose every line reads "LINE <n> ...", with an
// empty line now and then, line 30 far longer than the frames the tests
// cut, and no line break after the last line. It returns the text and
// the byte offset each line starts at (starts[n-1] is line n).
func longLineLog(lines int) (text []byte, starts []int64) {
	var buf bytes.Buffer
	for n := 1; n <= lines; n++ {
		starts = append(starts, int64(buf.Len()))
		if n%17 != 0 {
			fmt.Fprintf(&buf, "LINE %d %s", n, strings.Repeat("x", (n*7)%50))
		}
		if n == 30 {
			buf.WriteString(strings.Repeat(" long", 120))
		}
		if n < lines {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), starts
}

// splitFrameLayouts cuts text into frames the ways a foreign encoder
// could: every 97 bytes (so the long line spans several frames that
// hold no line break), around an empty frame, with an empty last frame,
// and into frames of 30 bytes that all lie inside the long line, which
// starts at byte longLine.
func splitFrameLayouts(text []byte, longLine int) map[string][][]byte {
	return map[string][][]byte{
		"cut every 97 bytes":  seekablefile.SplitEvery(text, 97),
		"an empty frame":      seekablefile.SplitAt(text, 150, 260, 260, 700),
		"an empty last frame": seekablefile.SplitAt(text, 400, len(text)-5, len(text)),
		"frames inside a line": seekablefile.SplitAt(text,
			longLine+20, longLine+50, longLine+80, longLine+110, longLine+140),
	}
}

// writeSplitCopies writes text as a plain file and as one seekable file
// per frame layout, and returns the plain path and the seekable paths
// by layout name. longLine is the byte the long line starts at.
func writeSplitCopies(t *testing.T, text []byte, longLine int64) (plain string, copies map[string]string) {
	t.Helper()
	dir := t.TempDir()
	plain = filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	copies = map[string]string{}
	i := 0
	for name, frames := range splitFrameLayouts(text, int(longLine)) {
		path := filepath.Join(dir, fmt.Sprintf("app-%d.log.zst", i))
		seekablefile.Write(t, path, frames)
		copies[name] = path
		i++
	}
	return plain, copies
}

func TestLinesOnASeekableFileWithSplitFramesAnswerAsThePlainFile(t *testing.T) {
	text, starts := longLineLog(80)
	plainPath, copies := writeSplitCopies(t, text, starts[29])

	// Every line but the last, which -1 asks for below: a line asked
	// for twice is a different question.
	var spec []OffsetOrRange
	for n := 1; n < len(starts); n++ {
		spec = append(spec, OffsetOrRange{Start: int64(n)})
	}
	end := int64(45)
	spec = append(spec, OffsetOrRange{Start: 25, End: &end}, OffsetOrRange{Start: -1}, OffsetOrRange{Start: 500})

	request := func(path string, loader IndexLoader) Request {
		return Request{Path: path, Lines: spec, BeforeContext: 2, AfterContext: 2, IndexLoader: loader}
	}
	plain, err := Resolve(t.Context(), request(plainPath, NoIndex))
	if err != nil {
		t.Fatalf("resolve plain: %v", err)
	}
	if plain.Lines["31"] != starts[30] {
		t.Fatalf("plain: line 31 starts at %d, want %d", plain.Lines["31"], starts[30])
	}

	for layout, path := range copies {
		for how, loader := range loadersFor(t, path) {
			t.Run(layout+"/"+how, func(t *testing.T) {
				got, err := Resolve(t.Context(), request(path, loader))
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				if !reflect.DeepEqual(got.Lines, plain.Lines) {
					t.Errorf("line offsets differ from the plain file's:\n got %v\nwant %v", got.Lines, plain.Lines)
				}
				for key, want := range plain.Samples {
					if !reflect.DeepEqual(got.Samples[key], want) {
						t.Errorf("window %s: got %q, want %q", key, got.Samples[key], want)
					}
				}
			})
		}
	}
}

func TestOffsetsOnASeekableFileWithSplitFramesAnswerAsThePlainFile(t *testing.T) {
	text, starts := longLineLog(80)
	plainPath, copies := writeSplitCopies(t, text, starts[29])

	// The start of every line and a byte inside every line that has one.
	var spec []OffsetOrRange
	for n, start := range starts {
		spec = append(spec, OffsetOrRange{Start: start})
		next := int64(len(text))
		if n+1 < len(starts) {
			next = starts[n+1] - 1
		}
		if inside := (start + next) / 2; inside > start {
			spec = append(spec, OffsetOrRange{Start: inside})
		}
	}

	request := func(path string, loader IndexLoader) Request {
		return Request{Path: path, Offsets: spec, BeforeContext: 2, AfterContext: 2, IndexLoader: loader}
	}
	plain, err := Resolve(t.Context(), request(plainPath, NoIndex))
	if err != nil {
		t.Fatalf("resolve plain: %v", err)
	}

	for layout, path := range copies {
		for how, loader := range loadersFor(t, path) {
			t.Run(layout+"/"+how, func(t *testing.T) {
				got, err := Resolve(t.Context(), request(path, loader))
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				if !reflect.DeepEqual(got.Offsets, plain.Offsets) {
					t.Errorf("offsets differ from the plain file's:\n got %v\nwant %v", got.Offsets, plain.Offsets)
				}
				for key, want := range plain.Samples {
					if !reflect.DeepEqual(got.Samples[key], want) {
						t.Errorf("window %s: got %q, want %q", key, got.Samples[key], want)
					}
				}
			})
		}
	}
}
