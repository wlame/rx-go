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
	"encoding/binary"
	"fmt"
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
// appends the seek table that describes them, written by rx's own
// seekable.WriteSeekTable, and writes the result to path. The file's
// text is the concatenation of frames.
//
// It fails the test on any error, so a caller can use the file at once.
func Write(t testing.TB, path string, frames [][]byte) {
	t.Helper()
	body, table := compressFrames(t, frames)
	out := bytes.NewBuffer(body)
	if err := seekable.WriteSeekTable(out, table); err != nil {
		t.Fatalf("write seek table: %v", err)
	}
	writeFile(t, path, out.Bytes())
}

// WriteWithFooter is Write with the seek table built byte by byte in
// the given footer layout by SeekTable, not by rx's writer, so a test
// can give rx a file as another encoder would write it.
func WriteWithFooter(t testing.TB, path string, frames [][]byte, footer Footer) {
	t.Helper()
	writeFile(t, path, EncodeWithFooter(t, frames, footer))
}

// EncodeWithFooter returns the bytes WriteWithFooter writes: each
// element of frames compressed as one zstd frame, then the seek table
// in the given footer layout.
func EncodeWithFooter(t testing.TB, frames [][]byte, footer Footer) []byte {
	t.Helper()
	body, table := compressFrames(t, frames)
	return append(body, SeekTable(table, footer)...)
}

// compressFrames compresses each element of frames as one zstd frame
// and returns the frames laid end to end, with the table entry of each.
func compressFrames(t testing.TB, frames [][]byte) ([]byte, []seekable.FrameInfo) {
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
	return out.Bytes(), table
}

// writeFile writes data to path and fails the test if it cannot.
func writeFile(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Footer names a layout of the 9 bytes that end a seek table.
type Footer int

const (
	// SpecFooter is the layout of the zstd seekable format
	// specification (facebook/zstd, contrib/seekable_format):
	// Number_Of_Frames (4), Seek_Table_Descriptor (1, zero),
	// Seekable_Magic_Number (4). Each entry is 8 bytes.
	SpecFooter Footer = iota
	// SpecFooterWithChecksums is SpecFooter with the descriptor's
	// Checksum_Flag (bit 7) set: each entry carries a 4-byte checksum
	// after its two sizes, 12 bytes in all.
	SpecFooterWithChecksums
	// LegacyRxFooter is the layout rx wrote before it followed the
	// specification: magic (4), frame count (4), flags (1, zero).
	LegacyRxFooter
)

// footerLayout is what SeekTable needs to know about one Footer: the
// descriptor byte it writes, whether each entry carries a checksum, and
// how it lays out the three fields of the last 9 bytes.
type footerLayout struct {
	descriptor   byte
	hasChecksums bool
	write        func(footer []byte, frames uint32, descriptor byte)
}

// footerLayouts maps each Footer to its layout.
var footerLayouts = map[Footer]footerLayout{
	SpecFooter:              {descriptor: 0x00, write: writeSpecFooter},
	SpecFooterWithChecksums: {descriptor: 0x80, hasChecksums: true, write: writeSpecFooter},
	LegacyRxFooter:          {descriptor: 0x00, write: writeLegacyRxFooter},
}

// Magic numbers of the seekable format, written here as literals rather
// than taken from the seekable package, so a table built here does not
// follow a change to rx's constants.
const (
	seekTableFrameMagic uint32 = 0x184D2A5E
	seekableFooterMagic uint32 = 0x8F92EAB1
	skippableHeaderSize        = 8
	footerSize                 = 9
)

// writeSpecFooter fills footer in the specification's order: frame
// count, descriptor, magic.
func writeSpecFooter(footer []byte, frames uint32, descriptor byte) {
	binary.LittleEndian.PutUint32(footer[0:4], frames)
	footer[4] = descriptor
	binary.LittleEndian.PutUint32(footer[5:9], seekableFooterMagic)
}

// writeLegacyRxFooter fills footer in the legacy rx order: magic,
// frame count, flags.
func writeLegacyRxFooter(footer []byte, frames uint32, descriptor byte) {
	binary.LittleEndian.PutUint32(footer[0:4], seekableFooterMagic)
	binary.LittleEndian.PutUint32(footer[4:8], frames)
	footer[8] = descriptor
}

// SeekTable returns the skippable frame that lists table in the given
// footer layout: the frame's 8-byte header, one entry per frame, and
// the 9-byte footer. In a layout with checksums, each entry's checksum
// is a value made from the frame's index, not a hash of its text: rx
// reads the field's width and nothing else, and a distinct value per
// entry makes a reader that takes it for a size fail.
func SeekTable(table []seekable.FrameInfo, footer Footer) []byte {
	layout, ok := footerLayouts[footer]
	if !ok {
		panic(fmt.Sprintf("seekablefile: unknown footer layout %d", footer))
	}
	var entries []byte
	for _, frame := range table {
		entries = binary.LittleEndian.AppendUint32(entries, uint32(frame.CompressedSize))   //nolint:gosec // test frames are far below 4 GiB
		entries = binary.LittleEndian.AppendUint32(entries, uint32(frame.DecompressedSize)) //nolint:gosec // test frames are far below 4 GiB
		if layout.hasChecksums {
			entries = binary.LittleEndian.AppendUint32(entries, 0xC0DE0000|uint32(frame.Index)) //nolint:gosec // a test table has few frames
		}
	}
	tail := make([]byte, footerSize)
	layout.write(tail, uint32(len(table)), layout.descriptor) //nolint:gosec // a test table has few frames
	out := make([]byte, skippableHeaderSize, skippableHeaderSize+len(entries)+footerSize)
	binary.LittleEndian.PutUint32(out[0:4], seekTableFrameMagic)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(entries)+footerSize)) //nolint:gosec // a test table is small
	out = append(out, entries...)
	return append(out, tail...)
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

// IsSeekable reports whether the file at path ends with a seek table
// that describes it (seekable.ReadSeekTable succeeds).
func IsSeekable(t testing.TB, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a test's own file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, err = seekable.ReadSeekTable(bytes.NewReader(data), int64(len(data)))
	return err == nil
}
