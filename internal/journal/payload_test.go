package journal

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
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

// lz4Stored returns text as systemd stores an lz4 value: its size as a
// little-endian uint64, then one block.
func lz4Stored(t *testing.T, text []byte) []byte {
	t.Helper()
	out := make([]byte, lz4SizePrefix+lz4.CompressBlockBound(len(text)))
	le.PutUint64(out, uint64(len(text)))
	n, err := lz4.CompressBlock(text, out[lz4SizePrefix:], nil)
	if err != nil || n == 0 {
		t.Fatalf("compress %d bytes: %d, %v", len(text), n, err)
	}
	return out[:lz4SizePrefix+n]
}

func TestDecodeLZ4Prefix_GivesTheStartOfWhatTheBlockHolds(t *testing.T) {
	texts := map[string][]byte{
		"repeats":        bytes.Repeat([]byte("MESSAGE=abc"), 200),
		"words":          randomWords(4<<10, 3),
		"one long match": append([]byte("COREDUMP="), bytes.Repeat([]byte{0}, 10_000)...),
		"short":          []byte("SHORT=value of fewer than 65 bytes, but compressed anyway, aaaaaaaaaaaaaa"),
	}
	for name, text := range texts {
		t.Run(name, func(t *testing.T) {
			stored := lz4Stored(t, text)
			for _, want := range []int{1, 9, namePrefixBytes, len(text)} {
				dst := make([]byte, want)
				n, _, err := decodeLZ4Prefix(stored, dst)
				if err != nil || !bytes.Equal(dst[:n], text[:min(want, len(text))]) {
					t.Errorf("a %d-byte prefix gives %q, %v; want %q", want, dst[:n], err, text[:min(want, len(text))])
				}
			}
		})
	}
}

func TestDecodeLZ4Prefix_NeedsOnlyTheStoredBytesItDecodes(t *testing.T) {
	// A long first run of literals, cut after the bytes asked for: the
	// rest of the run is not needed.
	text := randomWords(64<<10, 5)
	stored := lz4Stored(t, text)
	dst := make([]byte, namePrefixBytes)
	n, atEnd, err := decodeLZ4Prefix(stored[:lz4SizePrefix+200], dst)
	if err != nil || atEnd || !bytes.Equal(dst[:n], text[:namePrefixBytes]) {
		t.Errorf("from 200 stored bytes: %q, at end %v, %v", dst[:n], atEnd, err)
	}
}

func TestDecodeLZ4Prefix_GivesTheBlocksTextWhateverSizeTheValueDeclares(t *testing.T) {
	text := bytes.Repeat([]byte("MESSAGE=abc"), 200)
	stored := lz4Stored(t, text)
	for _, size := range []uint64{0, 3, 17, 1 << 40} {
		le.PutUint64(stored, size)
		dst := make([]byte, namePrefixBytes)
		n, _, err := decodeLZ4Prefix(stored, dst)
		if err != nil || !bytes.Equal(dst[:n], text[:namePrefixBytes]) {
			t.Errorf("a declared size of %d gives %q, %v; want %q", size, dst[:n], err, text[:namePrefixBytes])
		}
	}
}

func TestDecodeLZ4Prefix_RefusesBadBlocks(t *testing.T) {
	stored := lz4Stored(t, bytes.Repeat([]byte("MESSAGE=abc"), 200))
	block := stored[lz4SizePrefix:]
	// A match that reaches before the start: token 0x00 (no literal, a
	// match of 4 bytes), offset 1, with nothing decoded yet.
	before := append(append([]byte(nil), stored[:lz4SizePrefix]...), 0x00, 0x01, 0x00)
	rows := map[string]struct {
		stored []byte
		atEnd  bool
	}{
		"no block after the size":          {stored[:lz4SizePrefix], true},
		"a match before the start":         {before, false},
		"a block cut inside its first run": {append(append([]byte(nil), stored[:lz4SizePrefix]...), block[:2]...), true},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			n, atEnd, err := decodeLZ4Prefix(row.stored, make([]byte, namePrefixBytes))
			if err == nil || atEnd != row.atEnd {
				t.Errorf("gives %d bytes, at end %v, %v; want an error, at end %v", n, atEnd, err, row.atEnd)
			}
		})
	}
}
