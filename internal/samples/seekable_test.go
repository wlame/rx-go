package samples

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/seekableindex"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A seekable .zst carries a seek table so any frame can be decompressed
// on its own. Reading it like an ordinary archive throws that away: the
// whole file is decompressed from byte zero until the line turns up,
// which on a multi-gigabyte log is the cost the format exists to avoid.

// makeIndexedSeekable writes a .zst and the frame index for it, and
// returns the path plus a loader the resolver can use.
func makeIndexedSeekable(t *testing.T, lines, frameSize int) (string, IndexLoader, int64) {
	t.Helper()
	dir := t.TempDir()
	textPath := filepath.Join(dir, "app.log")

	var body bytes.Buffer
	for n := 1; n <= lines; n++ {
		fmt.Fprintf(&body, "log line number %d with padding to make frames\n", n)
	}
	if err := os.WriteFile(textPath, body.Bytes(), 0o600); err != nil {
		t.Fatalf("write text: %v", err)
	}

	zstPath := textPath + ".zst"
	src, err := os.Open(textPath)
	if err != nil {
		t.Fatalf("open text: %v", err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.Create(zstPath)
	if err != nil {
		t.Fatalf("create zst: %v", err)
	}
	defer func() { _ = dst.Close() }()
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: frameSize, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), src, int64(body.Len()), dst); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := dst.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	built, err := seekableindex.Build(zstPath)
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	frames := built.Frames
	lineCount := built.LineCount
	idx := &rxtypes.UnifiedFileIndex{
		FileType:  rxtypes.FileTypeSeekableZstd,
		LineIndex: built.LineIndex,
		Frames:    &frames,
		LineCount: &lineCount,
	}
	return zstPath, func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }, built.DecompressedSizeBytes
}

// The index only changes how fast the answer is reached: with it and
// without it, the same request answers the same way.
func TestSeekable_IndexDoesNotChangeTheAnswer(t *testing.T) {
	zstPath, loader, _ := makeIndexedSeekable(t, 50000, 64*1024)

	cases := []struct {
		name          string
		lines         []OffsetOrRange
		before, after int
	}{
		{"single line with context", []OffsetOrRange{{Start: 25000}}, 2, 2},
		{"first line", []OffsetOrRange{{Start: 1}}, 3, 3},
		{"last line", []OffsetOrRange{{Start: 50000}}, 3, 3},
		{"a frame boundary", []OffsetOrRange{{Start: 1362}}, 1, 1},
		{"negative line", []OffsetOrRange{{Start: -1}}, 0, 0},
		{"a range", []OffsetOrRange{{Start: 100, End: int64Ptr(105)}}, 0, 0},
		{"several lines", []OffsetOrRange{{Start: 10}, {Start: 30000}, {Start: 49999}}, 1, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			indexed, err := Resolve(Request{
				Path: zstPath, Lines: tc.lines,
				BeforeContext: tc.before, AfterContext: tc.after,
				IndexLoader: loader,
			})
			if err != nil {
				t.Fatalf("indexed: %v", err)
			}
			streamed, err := Resolve(Request{
				Path: zstPath, Lines: tc.lines,
				BeforeContext: tc.before, AfterContext: tc.after,
				IndexLoader: NoIndex,
			})
			if err != nil {
				t.Fatalf("streamed: %v", err)
			}

			if len(indexed.Samples) != len(streamed.Samples) {
				t.Fatalf("keys: indexed %v, streamed %v", indexed.Samples, streamed.Samples)
			}
			for key, want := range streamed.Samples {
				got := indexed.Samples[key]
				if len(got) != len(want) {
					t.Fatalf("%s: indexed %d lines, streamed %d", key, len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("%s line %d: indexed %q, streamed %q", key, i, got[i], want[i])
					}
				}
				if indexed.Lines[key] != streamed.Lines[key] {
					t.Errorf("%s offset: indexed %d, streamed %d", key, indexed.Lines[key], streamed.Lines[key])
				}
			}
		})
	}
}

// The answer also matches the plain-text original, which is the rule the
// whole line-numbering contract rests on: plain, gzipped and
// seekable-zstd copies of one log answer identically.
func TestSeekable_MatchesThePlainFile(t *testing.T) {
	zstPath, loader, _ := makeIndexedSeekable(t, 20000, 64*1024)
	plainPath := zstPath[:len(zstPath)-len(".zst")]

	for _, line := range []int64{1, 2, 1362, 9999, 20000} {
		fromZst, err := Resolve(Request{
			Path: zstPath, Lines: []OffsetOrRange{{Start: line}},
			BeforeContext: 2, AfterContext: 2, IndexLoader: loader,
		})
		if err != nil {
			t.Fatalf("zst line %d: %v", line, err)
		}
		fromPlain, err := Resolve(Request{
			Path: plainPath, Lines: []OffsetOrRange{{Start: line}},
			BeforeContext: 2, AfterContext: 2, IndexLoader: NoIndex,
		})
		if err != nil {
			t.Fatalf("plain line %d: %v", line, err)
		}
		key := strconv.FormatInt(line, 10)
		if fromZst.Lines[key] != fromPlain.Lines[key] {
			t.Errorf("line %d offset: zst %d, plain %d", line, fromZst.Lines[key], fromPlain.Lines[key])
		}
		if len(fromZst.Samples[key]) != len(fromPlain.Samples[key]) {
			t.Fatalf("line %d: zst %v, plain %v", line, fromZst.Samples[key], fromPlain.Samples[key])
		}
		for i := range fromPlain.Samples[key] {
			if fromZst.Samples[key][i] != fromPlain.Samples[key][i] {
				t.Errorf("line %d, context %d: zst %q, plain %q",
					line, i, fromZst.Samples[key][i], fromPlain.Samples[key][i])
			}
		}
	}
}

// The budget: one line is answered from at most two frames, whatever
// the file's size: the frame holding it, and the one before when the
// line is the first the frame holds, since its head may be there.
func TestSeekable_OneLineCostsAtMostTwoFrames(t *testing.T) {
	zstPath, loader, totalBytes := makeIndexedSeekable(t, 50000, 64*1024)

	idx, err := loader(zstPath)
	if err != nil {
		t.Fatalf("loader: %v", err)
	}
	frames := *idx.Frames
	if len(frames) < 8 {
		t.Fatalf("fixture produced %d frames; the budget needs several", len(frames))
	}

	start, end, ok := frameRunFor(frames, 25000, 25000)
	if !ok {
		t.Fatal("no run for a line that exists")
	}
	if got := end - start + 1; got < 1 || got > 2 {
		t.Errorf("frames decompressed for one line: got %d, want 1 or 2", got)
	}

	read := frames[end].DecompressedOffset + frames[end].DecompressedSize - frames[start].DecompressedOffset
	if read >= totalBytes/4 {
		t.Errorf("reading line 25000 decompresses %d of %d bytes; the frame table should make it a fraction",
			read, totalBytes)
	}
}

func TestSeekable_FrameRunCoversTheWholeRequest(t *testing.T) {
	_, loader, _ := makeIndexedSeekable(t, 50000, 64*1024)
	idx, _ := loader("")
	frames := *idx.Frames

	cases := []struct{ first, last int64 }{
		{1, 1}, {1, 5000}, {25000, 25000}, {1361, 1363}, {49000, 50000},
	}
	for _, tc := range cases {
		start, end, ok := frameRunFor(frames, tc.first, tc.last)
		if !ok {
			t.Errorf("lines %d..%d: no run", tc.first, tc.last)
			continue
		}
		if frames[start].FirstLine > tc.first && start > 0 {
			t.Errorf("lines %d..%d: run starts at line %d, too late", tc.first, tc.last, frames[start].FirstLine)
		}
		if frames[end].LastLine < tc.last && end < len(frames)-1 {
			t.Errorf("lines %d..%d: run ends at line %d, too early", tc.first, tc.last, frames[end].LastLine)
		}
	}

	// A line past the end of the file has no run at all.
	if _, _, ok := frameRunFor(frames, 900000, 900000); ok {
		t.Error("a line past the end of the file produced a frame run")
	}
}

func int64Ptr(v int64) *int64 { return &v }

// A file that ends with a newline has no empty line after it. The final
// zero-length read is the end of the file, and appending it invented a
// line the file does not have — which the compressed paths and
// rx-python have always known, so the three storage forms of one log
// disagreed about their own last line.
func TestPlainFile_TrailingNewlineIsNotAnExtraLine(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		content string
		want    []string
	}{
		"ends with a newline":    {"a\nb\nc\n", []string{"a", "b", "c"}},
		"ends without a newline": {"a\nb\nc", []string{"a", "b", "c"}},
		"has a real empty line":  {"a\n\nc\n", []string{"a", "", "c"}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".log")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			resp, err := Resolve(Request{
				Path:          path,
				Lines:         []OffsetOrRange{{Start: 3}},
				BeforeContext: 2,
				AfterContext:  2,
				IndexLoader:   NoIndex,
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			got := resp.Samples["3"]
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("line %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
