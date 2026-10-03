package samples

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A byte offset in a compressed file is a position in its text, the
// decompressed stream, which is the coordinate a search of the file
// reports. `samples --offsets=B` answers it the way it answers B in the
// plain copy of the same text, and `--lines=N` and `--offsets=<offset
// of N>` are inverses on every format rx reads.

// variedLog builds a log whose lines differ in length, with an empty
// line now and then and no newline after the last line, so a line
// boundary off by one byte shows up somewhere. It returns the text and
// the byte offset each line starts at (starts[n-1] is line n).
func variedLog(lines int, seed uint64) (text []byte, starts []int64) {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	var buf bytes.Buffer
	for n := 1; n <= lines; n++ {
		starts = append(starts, int64(buf.Len()))
		if n%37 != 0 {
			fmt.Fprintf(&buf, "LINE %d %s", n, strings.Repeat("x", rng.IntN(160)))
		}
		if n < lines {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), starts
}

// compressedCopiesOf writes text to dir in every compressed format rx
// reads and returns their paths by name. bzip2 needs the bzip2 binary,
// since Go's standard library only decodes it.
func compressedCopiesOf(t *testing.T, text []byte, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	write := func(name string, body []byte) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		out[name] = path
	}

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(text)
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	write("app.log.gz", gz.Bytes())

	var xzBuf bytes.Buffer
	xw, err := xz.NewWriter(&xzBuf)
	if err != nil {
		t.Fatalf("xz writer: %v", err)
	}
	_, _ = xw.Write(text)
	if err := xw.Close(); err != nil {
		t.Fatalf("xz: %v", err)
	}
	write("app.log.xz", xzBuf.Bytes())

	zw, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	write("app.log.zst", zw.EncodeAll(text, nil))
	_ = zw.Close()

	var seekableBuf bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 2048, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &seekableBuf); err != nil {
		t.Fatalf("encode seekable: %v", err)
	}
	write("app.seekable.zst", seekableBuf.Bytes())

	if _, err := exec.LookPath("bzip2"); err == nil {
		cmd := exec.Command("bzip2", "-c")
		cmd.Stdin = bytes.NewReader(text)
		body, err := cmd.Output()
		if err != nil {
			t.Fatalf("bzip2: %v", err)
		}
		write("app.log.bz2", body)
	} else {
		t.Log("bzip2 binary not on PATH; the bz2 copy is not checked")
	}
	return out
}

// loadersFor returns the two ways a compressed file can be resolved:
// with no index, and with the index `rx index` builds for it. The
// answer must not depend on which one is used.
func loadersFor(t *testing.T, path string) map[string]IndexLoader {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatalf("index.Build(%s): %v", path, err)
	}
	return map[string]IndexLoader{
		"no index": NoIndex,
		"indexed":  func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil },
	}
}

// offsetsOfLines picks, for each line, the offset it starts at and one
// inside it, which must both resolve to that line.
func offsetsOfLines(rng *rand.Rand, text []byte, starts []int64, lines []int) (spec []OffsetOrRange, wantLine map[int64]int64) {
	wantLine = map[int64]int64{}
	for _, n := range lines {
		start := starts[n-1]
		end := int64(len(text))
		if n < len(starts) {
			end = starts[n] - 1 // the newline still belongs to line n
		}
		inside := start + rng.Int64N(end-start+1)
		for _, off := range []int64{start, inside} {
			if _, dup := wantLine[off]; dup {
				continue
			}
			wantLine[off] = int64(n)
			spec = append(spec, OffsetOrRange{Start: off})
		}
	}
	return spec, wantLine
}

// randomLines returns count distinct line numbers in 1..total, always
// including the first and the last line.
func randomLines(rng *rand.Rand, total, count int) []int {
	seen := map[int]bool{1: true, total: true}
	for len(seen) < count {
		seen[1+rng.IntN(total)] = true
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func TestOffsetsOnACompressedFileAnswerAsThePlainFile(t *testing.T) {
	text, starts := variedLog(3000, 67)
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plainPath, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	rng := rand.New(rand.NewPCG(3, 4))
	spec, wantLine := offsetsOfLines(rng, text, starts, randomLines(rng, len(starts), 40))
	// Counted back from the end, and one past the last byte.
	spec = append(spec, OffsetOrRange{Start: -1}, OffsetOrRange{Start: int64(len(text)) + 10})

	request := func(path string, loader IndexLoader) Request {
		return Request{Path: path, Offsets: spec, BeforeContext: 2, AfterContext: 2, IndexLoader: loader}
	}
	plain, err := Resolve(request(plainPath, NoIndex))
	if err != nil {
		t.Fatalf("resolve plain: %v", err)
	}
	for off, line := range wantLine {
		if got := plain.Offsets[strconv.FormatInt(off, 10)]; got != line {
			t.Fatalf("plain: offset %d is on line %d, want %d", off, got, line)
		}
	}

	for name, path := range compressedCopiesOf(t, text, dir) {
		for how, loader := range loadersFor(t, path) {
			t.Run(name+"/"+how, func(t *testing.T) {
				got, err := Resolve(request(path, loader))
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				if !got.IsCompressed {
					t.Error("the answer does not say the file is compressed")
				}
				if !reflect.DeepEqual(got.Offsets, plain.Offsets) {
					t.Errorf("offsets differ from the plain file's:\n got %v\nwant %v", got.Offsets, plain.Offsets)
				}
				if !reflect.DeepEqual(got.Samples, plain.Samples) {
					for key, want := range plain.Samples {
						if !reflect.DeepEqual(got.Samples[key], want) {
							t.Errorf("window %s: got %q, want %q", key, got.Samples[key], want)
						}
					}
					t.FailNow()
				}
			})
		}
	}
}

func TestLinesAndOffsetsAreInversesOnACompressedFile(t *testing.T) {
	text, starts := variedLog(2000, 11)
	dir := t.TempDir()
	rng := rand.New(rand.NewPCG(5, 6))
	lines := randomLines(rng, len(starts), 25)

	for name, path := range compressedCopiesOf(t, text, dir) {
		for how, loader := range loadersFor(t, path) {
			t.Run(name+"/"+how, func(t *testing.T) {
				for _, n := range lines {
					byLine, err := Resolve(Request{
						Path: path, Lines: []OffsetOrRange{{Start: int64(n)}}, IndexLoader: loader,
					})
					if err != nil {
						t.Fatalf("lines=%d: %v", n, err)
					}
					offset := byLine.Lines[strconv.Itoa(n)]
					if offset != starts[n-1] {
						t.Fatalf("lines=%d reports offset %d, want %d", n, offset, starts[n-1])
					}
					byOffset, err := Resolve(Request{
						Path: path, Offsets: []OffsetOrRange{{Start: offset}}, IndexLoader: loader,
					})
					if err != nil {
						t.Fatalf("offsets=%d: %v", offset, err)
					}
					if got := byOffset.Offsets[strconv.FormatInt(offset, 10)]; got != int64(n) {
						t.Fatalf("offsets=%d reports line %d, want %d", offset, got, n)
					}
					if !reflect.DeepEqual(byOffset.Samples, renameKey(byLine.Samples, strconv.Itoa(n), strconv.FormatInt(offset, 10))) {
						t.Fatalf("line %d: offsets window %v, lines window %v", n, byOffset.Samples, byLine.Samples)
					}
				}
			})
		}
	}
}

// renameKey returns windows with the one window under from moved to to.
func renameKey(windows map[string][]string, from, to string) map[string][]string {
	return map[string][]string{to: windows[from]}
}

// A frame table whose line numbers do not add up to the file's line
// count would number every later line wrongly, so it is not trusted:
// the answer comes from the stream, as without an index.
func TestSeekableOffsets_DistrustAFrameTableThatDoesNotAddUp(t *testing.T) {
	text, starts := variedLog(2000, 21)
	dir := t.TempDir()
	path := compressedCopiesOf(t, text, dir)["app.seekable.zst"]
	idx, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	frames := *idx.Frames
	if len(frames) < 4 {
		t.Fatalf("the fixture has %d frames; the test needs several", len(frames))
	}
	// Shift every frame after the first by one line, as a table that
	// counted a frame without a newline as holding a line would.
	for i := 1; i < len(frames); i++ {
		frames[i].FirstLine++
		frames[i].LastLine++
	}
	loader := func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }

	n := 1500
	got, err := Resolve(Request{Path: path, Offsets: []OffsetOrRange{{Start: starts[n-1]}}, IndexLoader: loader})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if line := got.Offsets[strconv.FormatInt(starts[n-1], 10)]; line != int64(n) {
		t.Fatalf("offset of line %d resolved to line %d", n, line)
	}
}

// One offset deep in a seekable file is answered from the frames around
// it, whatever the file's size: the frame before the one holding the
// offset (where the pass finds its first whole line) and that frame.
func TestSeekableOffsets_DecodeOnlyTheFramesAroundTheOffset(t *testing.T) {
	zstPath, loader, _ := makeIndexedSeekable(t, 50000, 64*1024)
	idx, err := loader(zstPath)
	if err != nil {
		t.Fatalf("loader: %v", err)
	}
	if len(*idx.Frames) < 20 {
		t.Fatalf("the fixture has %d frames; the budget needs many", len(*idx.Frames))
	}

	var offset int64
	const line = 25000
	for n := 1; n < line; n++ {
		offset += int64(len(fmt.Sprintf("log line number %d with padding to make frames\n", n)))
	}

	decoded := map[int]int{}
	orig := decodeSeekableFrame
	decodeSeekableFrame = func(d *seekable.Decoder, src paths.Pinned, frame int, table *seekable.SeekTable) ([]byte, error) {
		decoded[frame]++
		return orig(d, src, frame, table)
	}
	t.Cleanup(func() { decodeSeekableFrame = orig })

	got, err := Resolve(Request{
		Path: zstPath, Offsets: []OffsetOrRange{{Start: offset}},
		BeforeContext: 3, AfterContext: 3, IndexLoader: loader,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	key := strconv.FormatInt(offset, 10)
	if got.Offsets[key] != line {
		t.Fatalf("offset %d resolved to line %d, want %d", offset, got.Offsets[key], line)
	}
	if want := fmt.Sprintf("log line number %d with padding to make frames", line); got.Samples[key][3] != want {
		t.Fatalf("window %q does not hold %q in the middle", got.Samples[key], want)
	}
	if len(decoded) == 0 {
		t.Fatal("no frame was decoded through the frame table; the file was streamed")
	}
	if len(decoded) > 3 {
		t.Errorf("decoded %d frames (%v) for one offset; the frames around it are enough", len(decoded), decoded)
	}
}

// A stream that cannot seek still stops once every window is complete:
// an offset near the start of a gzip file does not decompress the rest.
func TestStreamedOffsets_StopAfterTheLastWindow(t *testing.T) {
	text, starts := variedLog(60000, 31)
	path := compressedCopiesOf(t, text, t.TempDir())["app.log.gz"]
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	counter := withCountingOpen(t)
	got, err := Resolve(Request{
		Path: path, Offsets: []OffsetOrRange{{Start: starts[499]}},
		BeforeContext: 3, AfterContext: 3, IndexLoader: NoIndex,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if line := got.Offsets[strconv.FormatInt(starts[499], 10)]; line != 500 {
		t.Fatalf("offset resolved to line %d, want 500", line)
	}
	if read := counter.Load(); read > info.Size()/4 {
		t.Errorf("read %d of %d compressed bytes for an offset near the start", read, info.Size())
	}
}

// failingFile is a file whose reads fail once limit bytes are read.
type failingFile struct {
	*os.File
	limit int64
	read  int64
}

var errDiskGone = errors.New("the disk went away")

func (f *failingFile) Read(p []byte) (int, error) {
	if f.read >= f.limit {
		return 0, errDiskGone
	}
	if room := f.limit - f.read; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := f.File.Read(p)
	f.read += int64(n)
	return n, err
}

// A read that fails part-way through is an error, not the end of the
// file: answering the offsets after it as past the end (-1) would say
// the file is shorter than it is.
func TestOffsets_AReadErrorIsReportedNotTakenForTheEnd(t *testing.T) {
	text, starts := variedLog(5000, 41)
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	orig := openFileForSamples
	openFileForSamples = func(src paths.Pinned) (readSeekCloser, error) {
		f, err := src.Open()
		if err != nil {
			return nil, err
		}
		return &failingFile{File: f, limit: int64(len(text)) / 2}, nil
	}
	t.Cleanup(func() { openFileForSamples = orig })

	_, err := Resolve(Request{Path: path, Offsets: []OffsetOrRange{{Start: starts[4900]}}, IndexLoader: NoIndex})
	if !errors.Is(err, errDiskGone) {
		t.Fatalf("err = %v, want the read error", err)
	}
}
