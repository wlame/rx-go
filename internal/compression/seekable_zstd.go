// seekable_zstd.go implements reading and parsing of the seekable zstd format.
//
// A seekable zstd file consists of independently-decompressable zstd frames followed
// by a seek table stored as a zstd skippable frame at the end of the file.
//
// File layout:
//
//	[Frame 0 compressed data]
//	[Frame 1 compressed data]
//	...
//	[Frame N compressed data]
//	[Skippable Frame containing Seek Table]
//
// Seek table binary format (inside the skippable frame):
//
//	Skippable Frame Header (8 bytes):
//	  Magic:      0x184D2A5E  (LE u32) — zstd skippable frame with nibble 0xE
//	  Frame Size: N           (LE u32) — byte count of entries + footer
//
//	Per-frame entries (8 bytes each, or 12 if checksums are present):
//	  compressed_size:   LE u32
//	  decompressed_size: LE u32
//	  [checksum:         LE u32, only when flags & 0x01]
//
//	Footer (9 bytes):
//	  Magic:      0x8F92EAB1  (LE u32)
//	  num_frames: LE u32
//	  flags:      u8  (bit 0 = has_checksums)
package compression

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// decoderPool reuses zstd.Decoder instances to avoid the expensive allocation of
// decoding tables on every DecompressFrame call. Each decoder is created with
// zstd.NewReader(nil) for use with DecodeAll (stateless, single-buffer decompression).
var decoderPool = sync.Pool{
	New: func() interface{} {
		d, _ := zstd.NewReader(nil)
		return d
	},
}

// SeekableSkippableMagic is the magic number for the skippable frame that wraps
// the seek table (0x184D2A5E little-endian).
const SeekableSkippableMagic uint32 = 0x184D2A5E

// FrameEntry describes one independently-decompressable frame in a seekable zstd file.
type FrameEntry struct {
	CompressedOffset   int64  // Byte offset of compressed frame data in the file.
	CompressedSize     uint32 // Size of compressed frame in bytes.
	DecompressedOffset int64  // Cumulative decompressed byte offset (start of this frame).
	DecompressedSize   uint32 // Size of decompressed frame output in bytes.
	Checksum           uint32 // Frame checksum (zero when HasChecksums is false).
}

// SeekTable holds the parsed seek table from a seekable zstd file.
type SeekTable struct {
	Frames       []FrameEntry // One entry per independently-decompressable frame.
	HasChecksums bool         // True when the flags byte has bit 0 set.
}

// TotalDecompressedSize returns the sum of all frame decompressed sizes.
func (st *SeekTable) TotalDecompressedSize() int64 {
	if len(st.Frames) == 0 {
		return 0
	}
	last := st.Frames[len(st.Frames)-1]
	return last.DecompressedOffset + int64(last.DecompressedSize)
}

// ReadSeekTable parses the seek table from the end of a seekable zstd file.
//
// Algorithm:
//  1. Read the 9-byte footer (last 9 bytes of file).
//  2. Validate footer magic (0x8F92EAB1).
//  3. Extract num_frames and flags.
//  4. Compute entry size (8 or 12 bytes depending on checksum flag).
//  5. Seek back by 9 + (num_frames * entry_size) bytes from EOF.
//  6. Parse each entry, accumulating compressed and decompressed offsets.
func ReadSeekTable(f *os.File) (*SeekTable, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	fileSize := info.Size()

	if fileSize < seekTableFooterSize {
		return nil, fmt.Errorf("file too small for seekable zstd footer (%d bytes)", fileSize)
	}

	// Step 1-2: Read and validate footer.
	var footer [seekTableFooterSize]byte
	if _, err := f.ReadAt(footer[:], fileSize-seekTableFooterSize); err != nil {
		return nil, fmt.Errorf("read footer: %w", err)
	}

	magic := binary.LittleEndian.Uint32(footer[0:4])
	if magic != SeekTableFooterMagic {
		return nil, fmt.Errorf("not a seekable zstd file (footer magic 0x%08X, want 0x%08X)", magic, SeekTableFooterMagic)
	}

	// Step 3: Extract frame count and flags.
	numFrames := binary.LittleEndian.Uint32(footer[4:8])
	flags := footer[8]
	hasChecksums := (flags & 0x01) != 0

	// Step 4: Entry size depends on checksum flag.
	entrySize := 8
	if hasChecksums {
		entrySize = 12
	}

	// Step 5: Read all seek table entries.
	tableDataSize := int64(numFrames) * int64(entrySize)
	tableStart := fileSize - seekTableFooterSize - tableDataSize

	// Account for the 8-byte skippable frame header before the table data.
	// The header sits at (tableStart - 8), so the total skippable frame is:
	//   header(8) + entries(tableDataSize) + footer(9)
	if tableStart < 8 {
		return nil, fmt.Errorf("seekable zstd file too small for %d frames", numFrames)
	}

	tableData := make([]byte, tableDataSize)
	if _, err := f.ReadAt(tableData, tableStart); err != nil {
		return nil, fmt.Errorf("read seek table entries: %w", err)
	}

	// Step 6: Parse entries, accumulate offsets.
	table := &SeekTable{
		Frames:       make([]FrameEntry, numFrames),
		HasChecksums: hasChecksums,
	}

	var compressedOffset, decompressedOffset int64

	for i := uint32(0); i < numFrames; i++ {
		off := int(i) * entrySize
		compSize := binary.LittleEndian.Uint32(tableData[off : off+4])
		decompSize := binary.LittleEndian.Uint32(tableData[off+4 : off+8])

		var checksum uint32
		if hasChecksums {
			checksum = binary.LittleEndian.Uint32(tableData[off+8 : off+12])
		}

		table.Frames[i] = FrameEntry{
			CompressedOffset:   compressedOffset,
			CompressedSize:     compSize,
			DecompressedOffset: decompressedOffset,
			DecompressedSize:   decompSize,
			Checksum:           checksum,
		}

		compressedOffset += int64(compSize)
		decompressedOffset += int64(decompSize)
	}

	return table, nil
}

// DecompressFrame reads and decompresses a single frame from a seekable zstd file.
// It borrows a decoder from the package-level sync.Pool, avoiding the expensive
// allocation of zstd decoding tables on every call.
func DecompressFrame(f *os.File, frame FrameEntry) ([]byte, error) {
	dec := decoderPool.Get().(*zstd.Decoder)
	defer decoderPool.Put(dec)
	return DecompressFrameWith(f, frame, dec)
}

// DecompressFrameWith reads and decompresses a single frame using the provided decoder.
// Callers that already hold a *zstd.Decoder (e.g., tight loops that process many frames)
// can use this to avoid pool Get/Put overhead per frame.
//
// It uses ReadAt to read exactly frame.CompressedSize bytes at frame.CompressedOffset,
// then decompresses them with decoder.DecodeAll (stateless, single-buffer decompression).
func DecompressFrameWith(f *os.File, frame FrameEntry, decoder *zstd.Decoder) ([]byte, error) {
	compressed := make([]byte, frame.CompressedSize)
	n, err := f.ReadAt(compressed, frame.CompressedOffset)
	if err != nil {
		return nil, fmt.Errorf("read compressed frame at offset %d: %w", frame.CompressedOffset, err)
	}
	if n < int(frame.CompressedSize) {
		return nil, fmt.Errorf("short read: got %d bytes, want %d", n, frame.CompressedSize)
	}

	decompressed, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress frame: %w", err)
	}

	return decompressed, nil
}

// IsLineAligned checks whether frames in the seekable zstd file end on newline
// boundaries. It decompresses the first frame and checks if its last byte is '\n'.
//
// Line-aligned frames guarantee that no line is split across frame boundaries, which
// simplifies parallel search offset adjustment and line counting.
func IsLineAligned(f *os.File, table *SeekTable) (bool, error) {
	if len(table.Frames) == 0 {
		return false, fmt.Errorf("seek table has no frames")
	}

	// For single-frame files, alignment is trivially true.
	if len(table.Frames) == 1 {
		return true, nil
	}

	data, err := DecompressFrame(f, table.Frames[0])
	if err != nil {
		return false, fmt.Errorf("decompress first frame: %w", err)
	}

	if len(data) == 0 {
		return false, nil
	}

	return data[len(data)-1] == '\n', nil
}
