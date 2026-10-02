// Package seekable implements the seekable-zstd binary format used by
// rx-go for large compressed log files. rx-go ships its own Go-native
// encoder/decoder, so no external t2sz binary is needed.
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
//   - Per-entry records: (compressed_size: u32, decompressed_size: u32),
//     8 bytes each, plus a u32 checksum (12 bytes each) when the footer
//     says the entries carry one. rx never writes checksums and never
//     checks them; it reads past them.
//   - A 9-byte footer, in the layout of the zstd seekable format
//     specification (facebook/zstd, contrib/seekable_format):
//     (num_frames: u32, descriptor: u8, footer_magic: u32). Bit 7 of
//     the descriptor is the checksum flag; bits 6 to 2 are reserved and
//     must be zero.
//
// rx-go up to v0.3.0 and rx-python write the footer in another order,
// the legacy rx layout: (footer_magic: u32, num_frames: u32, flags: u8),
// with bit 0 of flags as the checksum flag. ReadSeekTable reads both
// layouts and prefers the specification's; WriteSeekTable writes the
// specification's.
//
// The skippable-frame header (8 bytes: magic + length), the entries and
// the footer use LittleEndian encoding.
package seekable

// Magic constants. Kept at package level (not file-level) so external
// packages can reference them when probing binary data.
const (
	// SeekableMagic identifies the skippable-frame type used to carry
	// the seek table. Per the zstd spec, skippable frames start with
	// 0x184D2A5? — this code picks nibble 0xE, matching the
	// t2sz/python-seekable convention.
	SeekableMagic uint32 = 0x184D2A5E

	// FooterMagic (Seekable_Magic_Number) marks the 9-byte footer at
	// the end of a seekable file: its last 4 bytes in the
	// specification's layout, its first 4 in the legacy rx layout.
	FooterMagic uint32 = 0x8F92EAB1

	// SkippableHeaderSize is the size of the leading magic + length
	// prefix of the skippable frame (8 bytes = 4 magic + 4 length).
	SkippableHeaderSize = 8

	// FooterSize is the size of the trailing footer: num_frames (4),
	// descriptor (1) and magic (4), in either layout.
	FooterSize = 9

	// EntrySize is the size of one seek-table entry without checksums,
	// the only kind rx writes.
	EntrySize = 8

	// ChecksumEntrySize is the size of one seek-table entry whose footer
	// sets the checksum flag: the two sizes and a 4-byte checksum.
	ChecksumEntrySize = 12
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
	// Flags is the footer's descriptor byte as the file holds it (the
	// flags byte of a legacy rx footer).
	Flags  byte
	Frames []FrameInfo
}
