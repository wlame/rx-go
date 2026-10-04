package journal

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zeroReader gives zero bytes without end.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestDecodeLZ4_RefusesADeclaredSizeAbove128MiBBeforeMakingItsBuffer(t *testing.T) {
	for _, size := range []uint64{maxValue + 1, 1 << 40, ^uint64(0)} {
		stored := make([]byte, lz4SizePrefix+16)
		le.PutUint64(stored, size)
		var err error
		allocated := allocatedBytes(func() { _, err = decodeLZ4(stored) })
		if !errors.Is(err, errValueTooLarge) {
			t.Errorf("a declared size of %d gives %v, want errValueTooLarge", size, err)
		}
		if allocated > 1<<20 {
			t.Errorf("a declared size of %d allocated %d bytes before the refusal", size, allocated)
		}
	}
}

func TestDecodeZstd_RefusesAStreamedFrameThatDecodesToMoreThan128MiB(t *testing.T) {
	// A frame written in a stream does not declare its size, so the
	// decoder finds out only as it decodes.
	var frame bytes.Buffer
	w, err := zstd.NewWriter(&frame, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for range 129 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeZstd(frame.Bytes()); !errors.Is(err, errValueTooLarge) {
		t.Errorf("a %d-byte frame of 129 MiB gives %v, want errValueTooLarge", frame.Len(), err)
	}
}

func TestReadValue_AcceptsExactly128MiBAndRefusesOneByteMore(t *testing.T) {
	// The value lives only inside the if, so it can be freed before the
	// second read.
	if value, err := readValue(io.LimitReader(zeroReader{}, maxValue)); err != nil || len(value) != maxValue {
		t.Errorf("a value of exactly 128 MiB gives %d bytes, %v", len(value), err)
	}
	if _, err := readValue(io.LimitReader(zeroReader{}, maxValue+1)); !errors.Is(err, errValueTooLarge) {
		t.Errorf("a value of 128 MiB and one byte gives %v, want errValueTooLarge", err)
	}
}
