// Package seekable implements the seekable-zstd binary format used by
// rx-go for large compressed log files. Per decision 5.4 and 5.14,
// rx-go ships its own Go-native encoder/decoder and drops the t2sz
// external dependency.
//
// Format overview:
//
//	+------------------+------------------+--------+------------------+
//	| zstd frame 1     | zstd frame 2     |  ...   | skippable frame  |
//	|                  |                  |        | (the seek table) |
//	+------------------+------------------+--------+------------------+
//
// Each "zstd frame N" is an independent zstd-compressed chunk, so a
// reader can decompress any frame without touching its neighbors.
//
// The skippable frame at the tail contains:
//
//   - Per-entry records: (compressed_size: u32, decompressed_size: u32)
//     — 8 bytes each (we don't implement optional checksums).
//   - Footer: (footer_magic: u32, num_frames: u32, flags: u8) — 9 bytes.
//
// Both the leading skippable-frame header (8 bytes: magic + length)
// and the tail footer use LittleEndian encoding.
//
// Magic constants are compatible with the t2sz / mcuadros spec used by
// rx-python, so files cross-decode with the Python decoder.
package seekable

// Magic constants. Kept at package level (not file-level) so external
// packages can reference them when probing binary data.
const (
	// SeekableMagic identifies the skippable-frame type used to carry
	// the seek table. Per the zstd spec, skippable frames start with
	// 0x184D2A5? — this code picks nibble 0xE, matching the
	// t2sz/python-seekable convention.
	SeekableMagic uint32 = 0x184D2A5E

	// FooterMagic identifies the last 9 bytes of a seekable file.
	// Readers probe tail-first: seek -9, read 9, match FooterMagic.
	FooterMagic uint32 = 0x8F92EAB1

	// SkippableHeaderSize is the size of the leading magic + length
	// prefix of the skippable frame (8 bytes = 4 magic + 4 length).
	SkippableHeaderSize = 8

	// FooterSize is the size of the trailing footer: magic + num_frames + flags.
	FooterSize = 9

	// EntrySize is the size of one seek-table entry without checksums.
	// We don't emit checksums so the entries are always 8 bytes each.
	EntrySize = 8
)

// FrameInfo describes one zstd frame in a seekable file.
//
// Offsets and sizes are uint64-safe (files can be > 4 GB), but
// individual frames are limited to 4 GB by the on-disk format (u32
// fields in the seek table). Encoder enforces this.
type FrameInfo struct {
	Index              int
	CompressedOffset   int64
	CompressedSize     int64
	DecompressedOffset int64
	DecompressedSize   int64
}

// CompressedEnd is the exclusive end offset of compressed data for this frame.
func (f FrameInfo) CompressedEnd() int64 { return f.CompressedOffset + f.CompressedSize }

// DecompressedEnd is the exclusive end offset of decompressed data.
func (f FrameInfo) DecompressedEnd() int64 { return f.DecompressedOffset + f.DecompressedSize }

// SeekTable is the parsed seek-table plus convenience fields.
type SeekTable struct {
	NumFrames int
	Flags     byte
	Frames    []FrameInfo
}
