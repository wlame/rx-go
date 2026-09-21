package samples

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/timestamps"
)

// lastLinesOf returns n single positions and n/10 short ranges among
// the last lines of a file of total lines, in no particular order.
func lastLinesOf(total, n int) []OffsetOrRange {
	var out []OffsetOrRange
	for i := range n {
		out = append(out, OffsetOrRange{Start: int64(total - i*3)})
		if i%10 == 0 {
			end := int64(total - i*3 + 2)
			out = append(out, OffsetOrRange{Start: int64(total - i*3 - 4), End: &end})
		}
	}
	return out
}

// Without an index, a thousand windows near the end of a plain file
// cost one pass over the text up to the last window, not one pass per
// window: asked by line and asked by time.
func TestBudget_ManyWindowsWithoutIndexReadThePlainTextOnce(t *testing.T) {
	const total = 35_000
	path, lines, size := largeTimedLog(t, total)
	var values []string
	for i := range 1000 {
		values = append(values, iso(lines[total-1-i*3].ms))
	}
	// The detection head, and one buffer past the end.
	head := int64(timestamps.SampleBytes) + 64*1024
	cases := []struct {
		name   string
		req    Request
		budget int64
	}{
		{"lines", Request{Lines: lastLinesOf(total, 1000)}, size + head},
		// The search for the times is one pass and the lines are a
		// second.
		{"timestamps", Request{Timestamps: values}, 2*size + head},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Path, req.IndexLoader, req.BeforeContext, req.AfterContext = path, NoIndex, 3, 3
			counter := withCountingOpen(t)
			resp, err := Resolve(t.Context(), req)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if len(resp.Samples) < 1000 {
				t.Fatalf("%d samples", len(resp.Samples))
			}
			if read := counter.Load(); read > tc.budget {
				t.Errorf("read %d bytes of a %d-byte file; budget %d", read, size, tc.budget)
			}
		})
	}
}

// A gzip file's windows are one pass over its decompressed text too.
func TestBudget_ManyWindowsWithoutIndexReadTheGzipTextOnce(t *testing.T) {
	const total = 35_000
	plainPath, _, _ := largeTimedLog(t, total)
	text, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	path := filepath.Join(t.TempDir(), "big.log.gz")
	compressed := compressedcopy.Encode(t, compressedcopy.Gzip, text)
	writeFile(t, path, compressed)
	counter := withCountingOpen(t)
	resp, err := Resolve(t.Context(), Request{Path: path, Lines: lastLinesOf(total, 1000), BeforeContext: 3, AfterContext: 3, IndexLoader: NoIndex})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resp.Samples) < 1000 {
		t.Fatalf("%d samples", len(resp.Samples))
	}
	// The detection head (a mebibyte of text decompressed from the
	// start), and the pass.
	if read, budget := counter.Load(), 2*int64(len(compressed))+decoderReadAhead; read > budget {
		t.Errorf("read %d compressed bytes of %d; budget %d", read, len(compressed), budget)
	}
}

// With a frame table, windows in the last frames of a seekable file
// decode those frames once: the pass reads the windows in order and
// keeps going through a frame the next window also needs.
func TestBudget_ManyWindowsDecodeEachSeekableFrameOnce(t *testing.T) {
	const total = 50_000
	path, loader, _ := makeIndexedSeekable(t, total, 64*1024)
	idx, err := loader(path)
	if err != nil {
		t.Fatalf("loader: %v", err)
	}
	wants := lastLinesOf(total, 300)
	first := int64(total - 299*3 - 4 - 3)
	// The frames holding the windows' lines, and the one before them.
	touched := 1
	for _, frame := range *idx.Frames {
		if frame.LastLine >= first {
			touched++
		}
	}
	var decoded atomic.Int64
	orig := decodeSeekableFrame
	decodeSeekableFrame = func(d *seekable.Decoder, src paths.Pinned, frame int, table *seekable.SeekTable) ([]byte, error) {
		decoded.Add(1)
		return orig(d, src, frame, table)
	}
	t.Cleanup(func() { decodeSeekableFrame = orig })
	resp, err := Resolve(t.Context(), Request{Path: path, Lines: wants, BeforeContext: 3, AfterContext: 3, IndexLoader: loader})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := resp.Samples[strconv.Itoa(total)]; len(got) != 4 {
		t.Fatalf("last line's sample: %q", got)
	}
	if n := decoded.Load(); n < 1 || n > int64(touched) {
		t.Errorf("decoded %d frames; the windows lie in %d", n, touched)
	}
}

// A canceled context stops the read: Resolve returns its error after
// at most the detection head and one buffer, in every mode.
func TestResolve_CancelledContextStopsTheRead(t *testing.T) {
	path, lines, size := largeTimedLog(t, 35_000)
	end := int64(size - 1)
	cases := map[string]Request{
		"lines":      {Lines: []OffsetOrRange{{Start: 34_000}}},
		"offsets":    {Offsets: []OffsetOrRange{{Start: 0, End: &end}}},
		"timestamps": {Timestamps: []string{iso(lines[34_000].ms)}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			req.Path, req.IndexLoader = path, NoIndex
			counter := withCountingOpen(t)
			_, err := Resolve(ctx, req)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err %v, want context.Canceled", err)
			}
			if read, budget := counter.Load(), int64(timestamps.SampleBytes)+64*1024; read > budget {
				t.Errorf("read %d bytes of %d after the cancel; budget %d", read, size, budget)
			}
		})
	}
}
