package seekableindex

import (
	"bytes"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// splitCheckpoints is the reference for interiorCheckpoints: it splits
// the frame into one slice per line and walks the slices, which states
// the rule plainly but holds a slice header for every line of the
// frame. A checkpoint goes on every CheckpointLineInterval-th line
// after the frame's first, at the line's first byte, and never at the
// frame's end, where the empty element after a final line break starts.
func splitCheckpoints(data []byte, frame seekable.FrameInfo, firstLine int64) []rxtypes.LineIndexEntry {
	var out []rxtypes.LineIndexEntry
	byteOffset := int64(0)
	lineNumber := firstLine
	frameIndex := frame.Index
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if byteOffset >= int64(len(data)) {
			break
		}
		if lineNumber > firstLine && (lineNumber-firstLine)%CheckpointLineInterval == 0 {
			out = append(out, rxtypes.LineIndexEntry{
				LineNumber: lineNumber,
				ByteOffset: frame.DecompressedOffset + byteOffset,
				FrameIndex: &frameIndex,
			})
		}
		byteOffset += int64(len(line)) + 1
		lineNumber++
	}
	return out
}

// requireSameCheckpoints fails the test when interiorCheckpoints and
// the reference disagree on data. FrameIndex is a pointer, so the
// entries are compared by the values they point at.
func requireSameCheckpoints(t *testing.T, data []byte, frame seekable.FrameInfo, firstLine int64) {
	t.Helper()
	got := interiorCheckpoints(data, frame, firstLine)
	want := splitCheckpoints(data, frame, firstLine)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%d bytes, first line %d: got %d checkpoints %v, want %d %v",
			len(data), firstLine, len(got), got, len(want), want)
	}
}

// lines returns n lines of the given text, each ended by a line break.
func lines(n int, text string) []byte {
	return []byte(strings.Repeat(text+"\n", n))
}

// The interior checkpoints of a frame are the ones splitting the frame
// into lines gives: the same lines at the same bytes, with none at the
// frame's end.
func TestInteriorCheckpoints_AgreeWithSplittingTheFrame(t *testing.T) {
	const n = CheckpointLineInterval
	cases := []struct {
		name string
		data []byte
	}{
		{"empty frame", nil},
		{"one line break", []byte("\n")},
		{"exactly one interval of lines", lines(n, "x")},
		{"one line more than an interval", lines(n+1, "x")},
		{"two intervals ending with a line break", lines(2*n, "LINE")},
		{"two intervals and an unterminated line", append(lines(2*n, "LINE"), "tail"...)},
		{"two intervals of empty lines", lines(2*n, "")},
		{"CRLF lines", lines(3*n+7, "row\r")},
		{"a frame that starts mid-line", append([]byte("rest of a line"), lines(2*n+3, "y")...)},
	}
	frames := []struct {
		name      string
		frame     seekable.FrameInfo
		firstLine int64
	}{
		{"first frame", seekable.FrameInfo{Index: 0}, 1},
		{"later frame", seekable.FrameInfo{Index: 7, DecompressedOffset: 123456789}, 98765},
	}
	for _, tc := range cases {
		for _, fr := range frames {
			t.Run(tc.name+"/"+fr.name, func(t *testing.T) {
				requireSameCheckpoints(t, tc.data, fr.frame, fr.firstLine)
			})
		}
	}
}

// FuzzInteriorCheckpoints compares interiorCheckpoints with the
// reference on frames made of a repeated unit, so that a short input
// still reaches the thousands of lines a checkpoint needs.
func FuzzInteriorCheckpoints(f *testing.F) {
	f.Add([]byte("x\n"), uint16(CheckpointLineInterval), int64(1))
	f.Add([]byte("\n"), uint16(2*CheckpointLineInterval+1), int64(5))
	f.Add([]byte("ab\ncd"), uint16(30000), int64(1))
	f.Add([]byte("\r\n\n"), uint16(12345), int64(77))
	f.Fuzz(func(t *testing.T, unit []byte, repeats uint16, firstLine int64) {
		// The frame stays under 1 MiB so that each run is quick.
		const maxFrame = 1 << 20
		if len(unit)*int(repeats) > maxFrame || firstLine < 1 || firstLine > 1<<40 {
			t.Skip()
		}
		data := bytes.Repeat(unit, int(repeats))
		frame := seekable.FrameInfo{Index: 3, DecompressedOffset: 4096}
		requireSameCheckpoints(t, data, frame, firstLine)
	})
}

// Building the index of a seekable file whose frames are all line
// breaks allocates about the frames' text, not a slice header for each
// of their lines: a frame of the largest size rx decodes holds 134
// million lines, and a slice per line cost about 3 GiB per frame.
func TestBuild_AllocationFollowsTheFrameSizeNotTheLineCount(t *testing.T) {
	const frameSize = 4 << 20
	frame := bytes.Repeat([]byte{'\n'}, frameSize)
	zstPath := filepath.Join(t.TempDir(), "newlines.log.zst")
	seekablefile.Write(t, zstPath, [][]byte{frame, frame})

	f, size, err := openForBuild(zstPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := Build(f, size)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if want := int64(2 * frameSize); got.LineCount != want {
		t.Errorf("line_count = %d, want %d", got.LineCount, want)
	}
	// Each frame is decoded into a buffer of its size; anything per line
	// would add 24 bytes for each of the 8 million lines (192 MiB).
	const budget = 4 * 2 * frameSize
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > budget {
		t.Errorf("Build allocated %d MiB for %d MiB of text, want at most %d MiB",
			allocated>>20, 2*frameSize>>20, budget>>20)
	}
}
