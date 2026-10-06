package compression

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// readAllThroughHeadDecoder decodes stream with a HeadDecoder that
// allows windowLimit and returns the text and the error.
func readAllThroughHeadDecoder(t *testing.T, stream []byte, windowLimit uint64) ([]byte, error) {
	t.Helper()
	dec, err := NewHeadDecoder(windowLimit)
	if err != nil {
		t.Fatalf("NewHeadDecoder: %v", err)
	}
	defer func() { _ = dec.Close() }()
	dec.Reset(bytes.NewReader(stream))
	return io.ReadAll(dec)
}

// zstdFrame compresses text as one frame with the given encoder options.
func zstdFrame(t *testing.T, text []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, append([]zstd.EOption{zstd.WithEncoderConcurrency(1)}, opts...)...)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer func() { _ = enc.Close() }()
	return enc.EncodeAll(text, nil)
}

// A HeadDecoder reads a stream of frames as any zstd decoder does,
// skipping a skippable frame between them.
func TestHeadDecoder_ReadsAStreamOfFrames(t *testing.T) {
	first := bytes.Repeat([]byte("first frame line\n"), 4000)
	second := bytes.Repeat([]byte("second frame line\n"), 4000)
	skippable := []byte{0x50, 0x2A, 0x4D, 0x18, 3, 0, 0, 0, 'a', 'b', 'c'}
	var stream []byte
	stream = append(stream, zstdFrame(t, first)...)
	stream = append(stream, skippable...)
	stream = append(stream, zstdFrame(t, second)...)

	got, err := readAllThroughHeadDecoder(t, stream, 1<<20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := append(append([]byte{}, first...), second...); !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, want the %d bytes of both frames", len(got), len(want))
	}
}

// The limit is on the window a frame needs: the window its header
// declares, or a single-segment frame's content size. A frame at the
// limit is read; one above it is refused with ErrWindowTooLarge.
func TestHeadDecoder_RefusesAWindowAboveTheLimit(t *testing.T) {
	text := bytes.Repeat([]byte("a line of the text\n"), 13_000) // about 240 KiB
	cases := []struct {
		name    string
		frame   []byte
		limit   uint64
		refused bool
	}{
		{"declared window at the limit", zstdFrame(t, text, zstd.WithWindowSize(64<<10)), 64 << 10, false},
		{"declared window above the limit", zstdFrame(t, text, zstd.WithWindowSize(128<<10)), 64 << 10, true},
		{"single segment within the limit", zstdFrame(t, text, zstd.WithSingleSegment(true)), 256 << 10, false},
		{"single segment above the limit", zstdFrame(t, text, zstd.WithSingleSegment(true)), 128 << 10, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readAllThroughHeadDecoder(t, tc.frame, tc.limit)
			if tc.refused {
				if !errors.Is(err, ErrWindowTooLarge) {
					t.Errorf("err = %v, want ErrWindowTooLarge", err)
				}
				return
			}
			if err != nil || !bytes.Equal(got, text) {
				t.Errorf("read %d bytes, %v; want the %d bytes of the text", len(got), err, len(text))
			}
		})
	}
}

// Reset only takes the stream; it reads none of it. The first frame's
// header is read by the first Read, so a frame whose window is above
// the limit is refused there, before the window is reserved.
func TestHeadDecoder_RefusesAFirstFrameAboveTheLimitOnTheFirstRead(t *testing.T) {
	text := bytes.Repeat([]byte("a line of the text\n"), 13_000) // about 240 KiB
	frame := zstdFrame(t, text, zstd.WithWindowSize(128<<10))
	dec, err := NewHeadDecoder(64 << 10)
	if err != nil {
		t.Fatalf("NewHeadDecoder: %v", err)
	}
	defer func() { _ = dec.Close() }()
	src := &countingReader{r: bytes.NewReader(frame)}

	dec.Reset(src)
	if src.n != 0 {
		t.Errorf("Reset read %d bytes; want none", src.n)
	}
	n, err := dec.Read(make([]byte, 4096))
	if n != 0 || !errors.Is(err, ErrWindowTooLarge) {
		t.Errorf("first Read = %d, %v; want 0, ErrWindowTooLarge", n, err)
	}
}
