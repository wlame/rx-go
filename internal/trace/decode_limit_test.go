package trace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/testutil/xzfile"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// overLimitLine is the line the over-limit text repeats, and lastLine
// the one line that differs, at its end, so a search must read all of
// the text to find it.
var (
	overLimitLine = []byte("2025-12-10 07:00:00.000 INFO same line again\n")
	lastLine      = []byte("2025-12-10 07:00:01.000 WARN NEEDLE at the very end\n")
)

// overLimitText is a little more text than compression.WindowLimit,
// whole lines that compress to a few kilobytes: what one frame of a
// small crafted file can hold.
func overLimitText() []byte {
	n := (compression.WindowLimit + 1<<20) / len(overLimitLine)
	return append(bytes.Repeat(overLimitLine, n), lastLine...)
}

// allocatedBy returns how many bytes of heap f allocates. The tests
// that call it do not run in parallel, so the count is f's own.
func allocatedBy(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// writeFixture writes body to a file named name in a new directory.
func writeFixture(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// zstdStream compresses text as a zstd stream whose frame declares a
// window of window bytes. The writer declares the window it is given
// only for an input longer than its first block.
func zstdStream(t *testing.T, text []byte, window int) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(window))
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	if _, err := w.Write(text); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return out.Bytes()
}

// A seekable file whose seek table gives one frame more text than rx
// decodes whole is read as the zstd stream it also is: through a
// decoder that holds the frame's window, never the frame. trace, samples
// and index all answer its text, as for its plain copy, for a few
// mebibytes, where each used to hold the whole frame.
func TestTraceSamplesAndIndexReadAFrameAboveTheLimitAsAStream(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	text := overLimitText()
	lineCount := int64(bytes.Count(text, []byte{'\n'}))
	var encoded bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: len(text), Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &encoded); err != nil {
		t.Fatalf("encode: %v", err)
	}
	path := writeFixture(t, "one-large-frame.log.zst", encoded.Bytes())
	t.Logf("%d bytes on disk hold %d MiB of text in one frame", encoded.Len(), len(text)>>20)

	const budget = 48 << 20 // the frame's 8 MiB window and buffers; the frame is 129 MiB
	var resp = traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
	allocated := allocatedBy(func() { resp = traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true}) })
	t.Logf("trace allocated %d MiB", allocated>>20)
	if allocated > budget {
		t.Errorf("trace allocated %d MiB; budget %d MiB", allocated>>20, budget>>20)
	}
	if len(resp.SkippedFiles) != 0 || len(resp.Matches) != 1 || int64(resp.Matches[0].AbsoluteLineNumber) != lineCount {
		t.Errorf("trace: skipped %v, %d matches (line %v); want the one match on line %d",
			resp.SkippedFiles, len(resp.Matches), lineNumbers(resp.Matches), lineCount)
	}

	var got string
	var err error
	allocated = allocatedBy(func() { got, err = samplesLine(t, path, lineCount) })
	t.Logf("samples allocated %d MiB", allocated>>20)
	if err != nil || got+"\n" != string(lastLine) {
		t.Errorf("samples line %d = %q, %v; want %q", lineCount, got, err, lastLine)
	}
	if allocated > budget {
		t.Errorf("samples allocated %d MiB; budget %d MiB", allocated>>20, budget>>20)
	}

	var idx *indexResult
	allocated = allocatedBy(func() { idx = buildIndex(path) })
	t.Logf("index allocated %d MiB", allocated>>20)
	if idx.err != nil || idx.lines != lineCount {
		t.Errorf("index: %d lines, %v; want %d lines", idx.lines, idx.err, lineCount)
	}
	if allocated > budget {
		t.Errorf("index allocated %d MiB; budget %d MiB", allocated>>20, budget>>20)
	}
}

// Reading a seekable file whose frame is above the limit as a stream
// changes how fast samples answers, never what: the answer is the same
// with an index and without one, and the same as the plain copy's.
func TestSamplesOfAFrameAboveTheLimitAgreeColdIndexedAndWithThePlainCopy(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	text := overLimitText()
	lineCount := int64(bytes.Count(text, []byte{'\n'}))
	var encoded bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: len(text), Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &encoded); err != nil {
		t.Fatalf("encode: %v", err)
	}
	stored := writeFixture(t, "one-large-frame.log.zst", encoded.Bytes())
	plain := writeFixture(t, "one-large-frame.log", text)
	// Each answer is a lines request and an offsets request, one mode
	// each, as a samples request takes.
	ask := func(path string) func(t testing.TB) any {
		return func(t testing.TB) any {
			resolve := func(req samples.Request) *rxtypes.SamplesResponse {
				req.Path, req.Source = path, pinForTest(t, path)
				req.BeforeContext, req.AfterContext = 1, 1
				req.IndexLoader = samples.StoredIndex
				resp, err := samples.Resolve(context.Background(), req)
				if err != nil {
					t.Fatalf("samples %s: %v", path, err)
				}
				return resp
			}
			lastButOne := lineCount - 1
			return []*rxtypes.SamplesResponse{
				resolve(samples.Request{Lines: []samples.OffsetOrRange{
					{Start: 2}, {Start: lineCount / 2}, {Start: lastButOne, End: &lineCount},
				}}),
				resolve(samples.Request{Offsets: []samples.OffsetOrRange{{Start: int64(len(text)) - 10}}}),
			}
		}
	}
	got := samplesanswer.ColdAndIndexed(t, stored, 0, ask(stored)).([]*rxtypes.SamplesResponse)
	want := ask(plain)(t).([]*rxtypes.SamplesResponse)
	for i := range want {
		samplesanswer.RequireAgree(t, "plain copy samples", got[i].Samples, want[i].Samples)
		samplesanswer.RequireAgree(t, "plain copy lines", got[i].Lines, want[i].Lines)
		samplesanswer.RequireAgree(t, "plain copy offsets", got[i].Offsets, want[i].Offsets)
	}
}

// indexResult is what buildIndex reports of an index build.
type indexResult struct {
	lines int64
	err   error
}

// buildIndex builds the line index of path and returns its line count.
func buildIndex(path string) *indexResult {
	idx, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		return &indexResult{err: err}
	}
	if idx.LineCount == nil {
		return &indexResult{err: errors.New("the index has no line count")}
	}
	return &indexResult{lines: *idx.LineCount}
}

// lineNumbers lists the line number of each match.
func lineNumbers(matches []rxtypes.Match) []int64 {
	numbers := make([]int64, len(matches))
	for i, m := range matches {
		numbers[i] = int64(m.AbsoluteLineNumber)
	}
	return numbers
}

// A file that cannot be decoded without more than compression.WindowLimit
// at once — a zstd frame that declares a larger window, or a
// single-segment frame whose content size, which is its window, is
// larger, or an xz block that declares a larger dictionary — is refused
// before the memory is reserved, the same way by every command: trace
// skips it with the reason, samples and index fail with an error
// wrapping compression.ErrTooLargeToDecode. None of them answers part of
// it as if it were the whole.
func TestTraceSamplesAndIndexRefuseAFileThatNeedsMoreThanTheLimit(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	small := bytes.Repeat([]byte("2025-12-10 07:00:00.000 INFO NEEDLE in a small file\n"), 8_000) // 400 KiB
	over := overLimitText()
	files := map[string][]byte{
		"seekable, single segment":    seekablefile.EncodeSingleSegment(t, [][]byte{over}),
		"zstd stream, single segment": singleSegmentStream(t, over),
		"zstd stream, 256 MiB window": zstdStream(t, small, 256<<20),
		"xz, 256 MiB dictionary":      xzfile.WithDictionaryCode(t, xzfile.Encode(t, small, xz.WriterConfig{}), 0, 32),
	}
	for name, body := range files {
		t.Run(name, func(t *testing.T) {
			path := writeFixture(t, "big.log.compressed", body)
			const budget = 16 << 20
			var resp = traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
			allocated := allocatedBy(func() { resp = traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true}) })
			t.Logf("trace allocated %d KiB to refuse it", allocated>>10)
			if allocated > budget {
				t.Errorf("trace allocated %d MiB; budget %d MiB", allocated>>20, budget>>20)
			}
			requireSkipReasons(t, resp, map[string]string{path: compression.TooLargeToDecodeReason})
			if len(resp.Matches) != 0 {
				t.Errorf("trace kept %d matches of a file it could not read", len(resp.Matches))
			}

			if _, err := samplesLine(t, path, 1); !errors.Is(err, compression.ErrTooLargeToDecode) {
				t.Errorf("samples: err = %v; want compression.ErrTooLargeToDecode", err)
			}
			if idx := buildIndex(path); !errors.Is(idx.err, compression.ErrTooLargeToDecode) {
				t.Errorf("index: err = %v; want compression.ErrTooLargeToDecode", idx.err)
			}
		})
	}
}

// singleSegmentStream compresses text as one single-segment zstd frame,
// whose window is its whole content size.
func singleSegmentStream(t *testing.T, text []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithSingleSegment(true))
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer func() { _ = enc.Close() }()
	return enc.EncodeAll(text, nil)
}

// The limit is the window `zstd -d` decodes without `--long=N`: a file
// written with `zstd --long=27` (128 MiB) is searched; one written with
// `--long=28` (256 MiB), which `zstd -d` itself refuses without the
// flag, is skipped with the reason.
func TestTraceReadsZstdLong27AndSkipsZstdLong28(t *testing.T) {
	requireRipgrep(t)
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd is not installed")
	}
	text := bytes.Repeat([]byte("2025-12-10 07:00:00.000 INFO NEEDLE in a long-window file\n"), 20_000)
	for _, tc := range []struct {
		flag    string
		skipped bool
	}{{"--long=27", false}, {"--long=28", true}} {
		t.Run(tc.flag, func(t *testing.T) {
			// From standard input the size is unknown, so zstd keeps the
			// window the flag asks for.
			cmd := exec.Command("zstd", "-q", "-c", tc.flag)
			cmd.Stdin = bytes.NewReader(text)
			body, err := cmd.Output()
			if err != nil {
				t.Fatalf("zstd %s: %v", tc.flag, err)
			}
			path := writeFixture(t, "long.log.zst", body)
			resp := traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
			if tc.skipped {
				requireSkipReasons(t, resp, map[string]string{path: compression.TooLargeToDecodeReason})
				return
			}
			if len(resp.SkippedFiles) != 0 || len(resp.Matches) != 20_000 {
				t.Errorf("skipped %v, %d matches; want none skipped and 20000 matches", resp.SkippedFiles, len(resp.Matches))
			}
		})
	}
}

// A file refused for its window is refused whole, also when the frame
// that needs too much comes after frames that do not: the matches of
// the frames before it are not kept, since keeping them would answer
// part of the file as a smaller file.
func TestTraceKeepsNoMatchOfAStreamWhoseLaterFrameNeedsMoreThanTheLimit(t *testing.T) {
	requireRipgrep(t)
	small := bytes.Repeat([]byte("2025-12-10 07:00:00.000 INFO NEEDLE before the large frame\n"), 4_000)
	body := slices.Concat(zstdStream(t, small, 1<<20), zstdStream(t, small, 256<<20))
	path := writeFixture(t, "two-frames.log.zst", body)
	resp := traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
	requireSkipReasons(t, resp, map[string]string{path: compression.TooLargeToDecodeReason})
	if len(resp.Matches) != 0 {
		t.Errorf("trace kept %d matches of the frame before the refused one", len(resp.Matches))
	}
}
