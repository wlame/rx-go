package filekind

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/testutil/xzfile"
)

// largeFrameBytes is the text of one frame in the large-frame fixtures:
// 256 MiB, which compresses to about 28 KB because every line is the
// same. A file this small that declares a frame this large is what a
// listing of a directory someone else filled can meet.
const largeFrameBytes = 256 << 20

// largeFrameLine is the one line the large-frame text repeats.
var largeFrameLine = []byte("2025-12-10 07:00:00.000 INFO same line again\n")

// largeFrameText is largeFrameBytes of text, as whole lines.
func largeFrameText() []byte {
	return bytes.Repeat(largeFrameLine, largeFrameBytes/len(largeFrameLine))
}

// largeFrameFiles are files whose text is one large frame, stored the
// ways a decoder meets one: by rx's own encoder (a frame header that
// declares an 8 MiB window), as a seekable file whose single-segment
// frame declares its whole content size as the window, and as a plain
// zstd stream with the same single-segment frame.
func largeFrameFiles(t *testing.T, text []byte) map[string][]byte {
	t.Helper()
	var rxEncoded bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: len(text), Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &rxEncoded); err != nil {
		t.Fatalf("encode: %v", err)
	}
	singleSegment, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithSingleSegment(true))
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer func() { _ = singleSegment.Close() }()
	return map[string][]byte{
		"seekable, rx's encoder":      rxEncoded.Bytes(),
		"seekable, single segment":    seekablefile.EncodeSingleSegment(t, [][]byte{text}),
		"zstd stream, single segment": singleSegment.EncodeAll(text, nil),
	}
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

// Classifying a file costs memory in proportion to the window the probe
// decoder allows, never to the frame size a file declares: a 28 KB file
// that declares a 256 MiB frame is classified for a few mebibytes, and
// one that declares more window than the probe allows is refused by
// the decoder before it reserves anything.
func TestOf_AFileDeclaringALargeFrameCostsAFewMebibytes(t *testing.T) {
	const budget = 20 << 20
	for name, body := range largeFrameFiles(t, largeFrameText()) {
		t.Run(name, func(t *testing.T) {
			r := bytes.NewReader(body)
			var kind Kind
			allocated := allocatedBy(func() { kind = Of(r, int64(len(body))) })
			t.Logf("Of allocated %d KiB for a %d-byte file", allocated>>10, len(body))
			if allocated > budget {
				t.Errorf("Of allocated %d MiB for a %d-byte file; budget %d MiB",
					allocated>>20, len(body), budget>>20)
			}
			if !kind.IsText() {
				t.Errorf("Of = %+v; want text", kind)
			}
		})
	}
}

// The detection head of a seekable file is read frame by frame through
// a streaming decoder, so it holds the frame's window and the head,
// never the whole frame. A frame that declares more window than a head
// read allows is refused with compression.ErrWindowTooLarge before the
// decoder reserves it.
func TestReadTextHead_ASeekableFrameIsNotDecodedWhole(t *testing.T) {
	const limit = 1 << 20
	text := largeFrameText()
	files := largeFrameFiles(t, text)

	t.Run("rx's encoder", func(t *testing.T) {
		body := files["seekable, rx's encoder"]
		r := bytes.NewReader(body)
		kind := FormatOf(r, int64(len(body)))
		var head []byte
		var err error
		allocated := allocatedBy(func() { head, err = ReadTextHead(r, int64(len(body)), kind, limit) })
		t.Logf("ReadTextHead allocated %d KiB", allocated>>10)
		if err != nil {
			t.Fatalf("ReadTextHead: %v", err)
		}
		if !bytes.Equal(head, text[:limit]) {
			t.Errorf("head is %d bytes; want the first %d bytes of the text", len(head), limit)
		}
		const budget = 16 << 20
		if allocated > budget {
			t.Errorf("ReadTextHead allocated %d MiB for a %d-byte head; budget %d MiB",
				allocated>>20, limit, budget>>20)
		}
	})

	t.Run("single segment", func(t *testing.T) {
		body := files["seekable, single segment"]
		r := bytes.NewReader(body)
		kind := FormatOf(r, int64(len(body)))
		if !kind.IsSeekable() {
			t.Fatalf("fixture is %+v; want a seekable file", kind)
		}
		var err error
		allocated := allocatedBy(func() { _, err = ReadTextHead(r, int64(len(body)), kind, limit) })
		t.Logf("ReadTextHead allocated %d KiB to refuse: %v", allocated>>10, err)
		if !errors.Is(err, compression.ErrWindowTooLarge) {
			t.Errorf("err = %v; want compression.ErrWindowTooLarge", err)
		}
		const budget = 4 << 20
		if allocated > budget {
			t.Errorf("ReadTextHead allocated %d MiB to refuse the frame; budget %d MiB",
				allocated>>20, budget>>20)
		}
	})
}

// A seekable file's head is the plain file's head byte for byte, also
// when frames end in the middle of a line and the head ends in the
// middle of a frame.
func TestReadTextHead_SeekableHeadEqualsThePlainHead(t *testing.T) {
	const limit = 1 << 20
	text := numberedLog(80_000) // about 3 MiB
	plain := bytes.NewReader(text)
	want, err := ReadTextHead(plain, int64(len(text)), Of(plain, int64(len(text))), limit)
	if err != nil {
		t.Fatalf("plain head: %v", err)
	}
	if len(want) != limit {
		t.Fatalf("plain head is %d bytes; want %d", len(want), limit)
	}
	stored := map[string][]byte{
		"frames cut mid-line": seekablefile.Encode(t, seekablefile.SplitEvery(text, 100_003)),
		"rx's encoder":        seekableOf(t, text),
	}
	for name, body := range stored {
		t.Run(name, func(t *testing.T) {
			r := bytes.NewReader(body)
			kind := Of(r, int64(len(body)))
			if !kind.IsSeekable() {
				t.Fatalf("fixture is %+v; want a seekable file", kind)
			}
			head, err := ReadTextHead(r, int64(len(body)), kind, limit)
			if err != nil {
				t.Fatalf("ReadTextHead: %v", err)
			}
			if !bytes.Equal(head, want) {
				t.Errorf("seekable head (%d bytes) differs from the plain head (%d bytes)", len(head), len(want))
			}
		})
	}
}

// An xz block header names the dictionary its decoder reserves before
// it decodes the block. Classifying a 64-byte xz file whose header names
// gigabytes costs a few mebibytes: the probe refuses a dictionary above
// its limit before reserving it, in any block, and takes the file for
// text, as it does a zstd frame whose window is above its limit.
func TestOf_AnXzFileDeclaringAHugeDictionaryCostsAFewMebibytes(t *testing.T) {
	small := xzfile.Encode(t, []byte("hello\n"), xz.WriterConfig{})
	twoBlocks := xzfile.Encode(t, numberedLog(2_000), xz.WriterConfig{BlockSize: 4 << 10}) // the probe reads into block 2
	files := map[string][]byte{
		"1 GiB":                     xzfile.WithDictionaryCode(t, small, 0, 36),
		"2 GiB":                     xzfile.WithDictionaryCode(t, small, 0, 38),
		"4 GiB":                     xzfile.WithDictionaryCode(t, small, 0, 40),
		"2 GiB in the second block": xzfile.WithDictionaryCode(t, twoBlocks, 1, 38),
	}
	for name, body := range files {
		t.Run(name, func(t *testing.T) {
			r := bytes.NewReader(body)
			var kind Kind
			allocated := allocatedBy(func() { kind = Of(r, int64(len(body))) })
			t.Logf("Of allocated %d KiB for a %d-byte file", allocated>>10, len(body))
			const budget = 12 << 20
			if allocated > budget {
				t.Errorf("Of allocated %d MiB for a %d-byte file; budget %d MiB",
					allocated>>20, len(body), budget>>20)
			}
			if kind.Format != compression.FormatXz || !kind.IsText() {
				t.Errorf("Of = %+v; want xz text", kind)
			}
		})
	}
}
