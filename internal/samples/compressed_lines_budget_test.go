package samples

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// streamedCopies are the compressed copies read as one stream, which
// cannot be entered in the middle.
var streamedCopies = []string{"app.log.gz", "app.log.bz2", "app.log.xz", "app.log.zst"}

// decoderReadAhead is how far past the bytes it needs a decoder may
// read: the xz reader fills a 64 KiB buffer at a time.
const decoderReadAhead = 64 * 1024

// smallBlockBzip2 rewrites the bzip2 copy with 100 KB blocks (bzip2 -1).
// A bzip2 block is decoded whole, so with the default 900 KB blocks this
// fixture is about two blocks and its first block is most of the file.
func smallBlockBzip2(t *testing.T, text []byte, copies map[string]string) {
	t.Helper()
	path, ok := copies["app.log.bz2"]
	if !ok {
		return
	}
	cmd := exec.Command("bzip2", "-1", "-c")
	cmd.Stdin = bytes.NewReader(text)
	body, err := cmd.Output()
	if err != nil {
		t.Fatalf("bzip2 -1: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write bzip2: %v", err)
	}
}

// A line near the start of a compressed stream is answered from the
// start of the stream: the pass stops after the last wanted line instead
// of decompressing to the end, with or without an index.
func TestStreamedLines_StopAfterTheLastWantedLine(t *testing.T) {
	text, _ := variedLog(60000, 51)
	copies := compressedCopiesOf(t, text, t.TempDir())
	smallBlockBzip2(t, text, copies)
	for _, name := range streamedCopies {
		path, ok := copies[name]
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			for loaderName, loader := range loadersFor(t, path) {
				end := int64(220)
				counter := withCountingOpen(t)
				got, err := Resolve(Request{
					Path:  path,
					Lines: []OffsetOrRange{{Start: 500}, {Start: 200, End: &end}},
					// Line 518 is empty (every 37th is), the others read "LINE n".
					BeforeContext: 3, AfterContext: 3, IndexLoader: loader,
				})
				if err != nil {
					t.Fatalf("%s: resolve: %v", loaderName, err)
				}
				if window := got.Samples["500"]; len(window) != 7 || !strings.HasPrefix(window[3], "LINE 500 ") {
					t.Fatalf("%s: window of line 500 = %q", loaderName, window)
				}
				if read := counter.Load(); read == 0 || read > info.Size()/4+decoderReadAhead {
					t.Errorf("%s: read %d of %d compressed bytes for lines near the start",
						loaderName, read, info.Size())
				}
			}
		})
	}
}

// A line counted from the end needs the line count. An index has it, so
// the lookup decompresses the stream once; without one it counts the
// lines in a first pass and reads to the line in a second.
func TestStreamedLines_TakeTheLineCountFromTheIndex(t *testing.T) {
	text, _ := variedLog(20000, 53)
	copies := compressedCopiesOf(t, text, t.TempDir())
	for _, name := range streamedCopies {
		path, ok := copies[name]
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			loaders := loadersFor(t, path)
			read := map[string]int64{}
			for loaderName, loader := range loaders {
				counter := withCountingOpen(t)
				got, err := Resolve(Request{
					Path: path, Lines: []OffsetOrRange{{Start: -2}},
					BeforeContext: 0, AfterContext: 0, IndexLoader: loader,
				})
				if err != nil {
					t.Fatalf("%s: resolve: %v", loaderName, err)
				}
				if window := got.Samples["19999"]; len(window) != 1 || !strings.HasPrefix(window[0], "LINE 19999 ") {
					t.Fatalf("%s: line -2 answered %v", loaderName, got.Samples)
				}
				read[loaderName] = counter.Load()
			}
			if read["indexed"] > info.Size() {
				t.Errorf("indexed: read %d bytes of a %d-byte file, want one pass", read["indexed"], info.Size())
			}
			if read["no index"] <= info.Size() {
				t.Errorf("no index: read %d bytes of a %d-byte file, want the counting pass too", read["no index"], info.Size())
			}
		})
	}
}

// With an index the pass starts at the checkpoint before the first
// wanted line; the answer is the plain file's, windows and offsets
// included, for single lines, ranges, lines counted from the end, a
// line past the end and line 0.
func TestStreamedLines_AnswerAsThePlainFileWithAndWithoutAnIndex(t *testing.T) {
	text, _ := variedLog(3000, 57)
	dir := t.TempDir()
	copies := compressedCopiesOf(t, text, dir)
	plainPath := dir + "/app.log"
	if err := os.WriteFile(plainPath, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	rangeEnd, lateEnd := int64(1210), int64(2990)
	spec := []OffsetOrRange{
		{Start: 1}, {Start: 37}, {Start: 1500}, {Start: 1200, End: &rangeEnd},
		{Start: 2950, End: &lateEnd}, {Start: -1}, {Start: -40}, {Start: 9999}, {Start: 0},
	}
	request := func(path string, loader IndexLoader) *rxtypes.SamplesResponse {
		t.Helper()
		got, err := Resolve(Request{Path: path, Lines: spec, BeforeContext: 5, AfterContext: 4, IndexLoader: loader})
		if err != nil {
			t.Fatalf("resolve %s: %v", path, err)
		}
		return got
	}
	want := request(plainPath, NoIndex)
	for _, name := range streamedCopies {
		path, ok := copies[name]
		if !ok {
			continue
		}
		for loaderName, loader := range loadersFor(t, path) {
			got := request(path, loader)
			for key, lines := range want.Samples {
				if strings.Join(got.Samples[key], "\n") != strings.Join(lines, "\n") ||
					(got.Samples[key] == nil) != (lines == nil) {
					t.Errorf("%s %s: samples[%s] differ from the plain file's", name, loaderName, key)
				}
				if got.Lines[key] != want.Lines[key] {
					t.Errorf("%s %s: lines[%s] = %d, want %d", name, loaderName, key, got.Lines[key], want.Lines[key])
				}
			}
			if len(got.Samples) != len(want.Samples) {
				t.Errorf("%s %s: %d keys, want %d", name, loaderName, len(got.Samples), len(want.Samples))
			}
		}
	}
	if _, ok := want.Samples[strconv.Itoa(3000)]; !ok {
		t.Fatalf("plain answer has no key for -1: %v", want.Lines)
	}
}
