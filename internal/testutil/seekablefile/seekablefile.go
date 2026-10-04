// Package seekablefile writes seekable-zstd files whose frame boundaries
// a test chooses byte by byte.
//
// rx's own encoder ends every frame at a line break, so a file it
// writes never has a line that continues from one frame into the next.
// Seekable files written by other tools (t2sz without newline
// alignment, or any encoder cutting at a fixed size) do, and a line
// longer than a frame spans several frames. This package makes such
// files on demand, so the code that numbers lines frame by frame can be
// tested against them.
//
// This is test-only infrastructure. It lives under internal/testutil so
// production binaries never depend on it, and it must not be imported
// from non-test code.
package seekablefile

import (
	"bytes"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/wlame/rx-go/internal/seekable"
)

// SplitAt cuts text into frames at the given byte positions, in
// ascending order. A position may repeat, which yields an empty frame,
// and may equal len(text), which yields an empty last frame.
func SplitAt(text []byte, cuts ...int) [][]byte {
	frames := make([][]byte, 0, len(cuts)+1)
	previous := 0
	for _, cut := range cuts {
		frames = append(frames, text[previous:cut])
		previous = cut
	}
	return append(frames, text[previous:])
}

// SplitEvery cuts text into frames of size bytes each; the last frame
// holds what is left. A frame boundary lands wherever the byte count
// says, mid-line or not.
func SplitEvery(text []byte, size int) [][]byte {
	var cuts []int
	for cut := size; cut < len(text); cut += size {
		cuts = append(cuts, cut)
	}
	return SplitAt(text, cuts...)
}

// Write compresses each element of frames as one zstd frame, in order,
// appends the seek table that describes them, and writes the result to
// path. The file's text is the concatenation of frames.
//
// It fails the test on any error, so a caller can use the file at once.
func Write(t testing.TB, path string, frames [][]byte) {
	t.Helper()
	// A nil writer is enough: EncodeAll compresses a whole buffer at a
	// time and never writes through the encoder's own io.Writer.
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatalf("create zstd encoder: %v", err)
	}
	defer func() { _ = encoder.Close() }()

	var out bytes.Buffer
	table := make([]seekable.FrameInfo, 0, len(frames))
	var decompressedOffset int64
	for i, text := range frames {
		compressed := encoder.EncodeAll(text, nil)
		table = append(table, seekable.FrameInfo{
			Index:              i,
			CompressedOffset:   int64(out.Len()),
			CompressedSize:     int64(len(compressed)),
			DecompressedOffset: decompressedOffset,
			DecompressedSize:   int64(len(text)),
		})
		out.Write(compressed)
		decompressedOffset += int64(len(text))
	}
	if err := seekable.WriteSeekTable(&out, table); err != nil {
		t.Fatalf("write seek table: %v", err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// DamageFrame overwrites five bytes in the middle of frame's compressed
// bytes in the seekable file at path, the way a bad sector or a partial
// copy damages an archive in one place: the seek table and every other
// frame stay intact, and the frame's header still parses. It fails the
// test unless the frame then no longer decompresses, so a test that
// uses it does test a damaged frame.
func DamageFrame(t testing.TB, path string, frame int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	table, err := seekable.ReadSeekTable(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read seek table of %s: %v", path, err)
	}
	if frame < 0 || frame >= table.NumFrames {
		t.Fatalf("frame %d: %s has %d frames", frame, path, table.NumFrames)
	}
	info := table.Frames[frame]
	const damagedBytes = 5
	if info.CompressedSize < 4*damagedBytes {
		t.Fatalf("frame %d is %d bytes, too short to damage in the middle", frame, info.CompressedSize)
	}
	at := info.CompressedOffset + info.CompressedSize/2
	for i := at; i < at+damagedBytes; i++ {
		data[i] ^= 0xFF
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("create zstd decoder: %v", err)
	}
	defer decoder.Close()
	if _, err := decoder.DecodeAll(data[info.CompressedOffset:info.CompressedEnd()], nil); err == nil {
		t.Fatalf("frame %d of %s still decompresses after the damage", frame, path)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // test fixture path the test chose
		t.Fatalf("write %s: %v", path, err)
	}
}
