package samples

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
)

// The window around a byte offset holds the same lines with an index as
// without one, and the same lines as the window around the line that
// offset is on. An index checkpoint is a line start every few kilobytes,
// so on a log of long lines a checkpoint gap holds only a few lines, and
// the leading context of an offset can reach back across several gaps.

// wideLineLog builds a log of `lines` lines, each `width` bytes long
// with its newline, starting "LINE <n> " so a wrong window shows which
// lines it holds. It returns the text and the byte offset each line
// starts at (starts[n-1] is line n).
func wideLineLog(lines, width int) (text []byte, starts []int64) {
	var buf bytes.Buffer
	for n := 1; n <= lines; n++ {
		starts = append(starts, int64(buf.Len()))
		head := fmt.Sprintf("LINE %d ", n)
		buf.WriteString(head + strings.Repeat("x", width-len(head)-1) + "\n")
	}
	return buf.Bytes(), starts
}

// lineOf returns the 1-based line holding offset in a text whose lines
// start at starts.
func lineOf(starts []int64, offset int64) int64 {
	n := int64(0)
	for _, s := range starts {
		if s > offset {
			break
		}
		n++
	}
	return n
}

func TestOffsetsWindowReachesBackAcrossCheckpointGaps(t *testing.T) {
	const (
		lineCount = 400
		lineWidth = 1000
		context   = 10 // the index below puts about 4 lines in a gap
	)
	text, starts := wideLineLog(lineCount, lineWidth)
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plainPath, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}

	// Offsets at a checkpoint, one byte after it and one byte before it
	// (the last byte of the line before), for checkpoints far enough
	// from both ends of the file that the whole window exists.
	plainIndex, err := index.Build(plainPath, index.BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	var offsets []int64
	for _, cp := range plainIndex.LineIndex {
		if cp.LineNumber > 3*context && cp.LineNumber < lineCount-2*context && len(offsets) < 12 {
			offsets = append(offsets, cp.ByteOffset, cp.ByteOffset+1, cp.ByteOffset-1)
		}
	}
	if len(offsets) < 12 {
		t.Fatalf("only %d offsets near checkpoints; the index has %d checkpoints", len(offsets), len(plainIndex.LineIndex))
	}

	request := func(path string, loader IndexLoader, at ...int64) Request {
		spec := make([]OffsetOrRange, 0, len(at))
		for _, off := range at {
			spec = append(spec, OffsetOrRange{Start: off})
		}
		return Request{Path: path, Offsets: spec, BeforeContext: context, AfterContext: context, IndexLoader: loader}
	}

	// The oracle: the plain file read from its first byte, checked
	// against the text itself.
	want, err := Resolve(t.Context(), request(plainPath, NoIndex, offsets...))
	if err != nil {
		t.Fatalf("resolve plain without an index: %v", err)
	}
	for _, off := range offsets {
		key := strconv.FormatInt(off, 10)
		line := lineOf(starts, off)
		if want.Offsets[key] != line {
			t.Fatalf("offset %d: line %d, want %d", off, want.Offsets[key], line)
		}
		window := want.Samples[key]
		if len(window) != 2*context+1 || !strings.HasPrefix(window[0], fmt.Sprintf("LINE %d ", line-context)) {
			t.Fatalf("offset %d: window of %d lines from %.12q, want %d from line %d",
				off, len(window), window[0], 2*context+1, line-context)
		}
	}

	copies := compressedCopiesOf(t, text, dir)
	copies["app.log"] = plainPath
	for name, path := range copies {
		for how, loader := range loadersFor(t, path) {
			t.Run(name+"/"+how, func(t *testing.T) {
				// All the offsets in one request, then each on its own:
				// a lone offset is the first window of its pass.
				got, err := Resolve(t.Context(), request(path, loader, offsets...))
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				requireSameWindows(t, "one request", got.Offsets, want.Offsets, got.Samples, want.Samples)
				for _, off := range offsets {
					one, err := Resolve(t.Context(), request(path, loader, off))
					if err != nil {
						t.Fatalf("resolve %d: %v", off, err)
					}
					key := strconv.FormatInt(off, 10)
					if one.Offsets[key] != want.Offsets[key] || !reflect.DeepEqual(one.Samples[key], want.Samples[key]) {
						t.Fatalf("offset %d alone: line %d with %d lines from %.12q; want line %d with %d lines from %.12q",
							off, one.Offsets[key], len(one.Samples[key]), first(one.Samples[key]),
							want.Offsets[key], len(want.Samples[key]), first(want.Samples[key]))
					}
				}

				// --lines is the inverse of --offsets: the window around
				// the line holding an offset is the window around it.
				line := want.Offsets[strconv.FormatInt(offsets[0], 10)]
				byLine, err := Resolve(t.Context(), Request{
					Path: path, Lines: []OffsetOrRange{{Start: line}},
					BeforeContext: context, AfterContext: context, IndexLoader: loader,
				})
				if err != nil {
					t.Fatalf("resolve line %d: %v", line, err)
				}
				if !reflect.DeepEqual(byLine.Samples[strconv.FormatInt(line, 10)], want.Samples[strconv.FormatInt(offsets[0], 10)]) {
					t.Fatalf("line %d: the --lines window differs from the --offsets window", line)
				}
			})
		}
	}
}

// requireSameWindows fails the test at the first offset whose line or
// window differs, naming the lines each window starts with.
func requireSameWindows(t *testing.T, what string, gotLines, wantLines map[string]int64, got, want map[string][]string) {
	t.Helper()
	for key, w := range want {
		if gotLines[key] != wantLines[key] || !reflect.DeepEqual(got[key], w) {
			t.Fatalf("%s, offset %s: line %d with %d lines from %.12q; want line %d with %d lines from %.12q",
				what, key, gotLines[key], len(got[key]), first(got[key]), wantLines[key], len(w), first(w))
		}
	}
}

// first returns a window's first line, or "" for an empty window.
func first(window []string) string {
	if len(window) == 0 {
		return ""
	}
	return window[0]
}
