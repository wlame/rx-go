package seekable

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotSeekable is returned when a probed file lacks the seekable-zstd
// footer magic.
var ErrNotSeekable = errors.New("not a seekable zstd file (footer magic missing)")

// ReadSeekTable parses the seek table at the tail of a seekable zstd
// file. The caller provides a ReaderAt and the full file size so we
// can probe absolute offsets without a seek.
//
// Algorithm:
//  1. Read the last 9 bytes (footer) — validate FooterMagic.
//  2. Derive entry count and flags.
//  3. Read numFrames × entrySize bytes immediately before the footer.
//  4. Walk the entries, accumulating compressed/decompressed offsets.
//
// Returns ErrNotSeekable if the footer doesn't match. Returns io.ErrUnexpectedEOF
// for truncated files.
func ReadSeekTable(r io.ReaderAt, fileSize int64) (*SeekTable, error) {
	if fileSize < FooterSize {
		return nil, io.ErrUnexpectedEOF
	}

	// Tail 9 bytes = footer.
	var footer [FooterSize]byte
	if _, err := r.ReadAt(footer[:], fileSize-FooterSize); err != nil {
		return nil, fmt.Errorf("read footer: %w", err)
	}
	magic := binary.LittleEndian.Uint32(footer[0:4])
	if magic != FooterMagic {
		return nil, ErrNotSeekable
	}
	numFrames := int(binary.LittleEndian.Uint32(footer[4:8]))
	flags := footer[8]

	// Bit 0 of flags indicates checksums are included (per the t2sz spec).
	// rx-go's encoder never emits checksums, but a file produced by t2sz
	// might. Widen entry size accordingly.
	entrySize := int64(EntrySize)
	if flags&0x01 != 0 {
		entrySize = 12 // checksums add 4 bytes per entry
	}

	// Entries live immediately before the footer.
	entriesSize := int64(numFrames) * entrySize
	entriesStart := fileSize - FooterSize - entriesSize
	if entriesStart < 0 {
		return nil, fmt.Errorf("corrupt seek table: entries would start at negative offset")
	}

	entries := make([]byte, entriesSize)
	if _, err := r.ReadAt(entries, entriesStart); err != nil {
		return nil, fmt.Errorf("read seek-table entries: %w", err)
	}

	frames := make([]FrameInfo, numFrames)
	var cOff, dOff int64
	for i := 0; i < numFrames; i++ {
		base := int64(i) * entrySize
		cSize := int64(binary.LittleEndian.Uint32(entries[base : base+4]))
		dSize := int64(binary.LittleEndian.Uint32(entries[base+4 : base+8]))
		// If flags&0x01 we skip 4 bytes of checksum — intentionally ignored.
		frames[i] = FrameInfo{
			Index:              i,
			CompressedOffset:   cOff,
			CompressedSize:     cSize,
			DecompressedOffset: dOff,
			DecompressedSize:   dSize,
		}
		cOff += cSize
		dOff += dSize
	}
	return &SeekTable{
		NumFrames: numFrames,
		Flags:     flags,
		Frames:    frames,
	}, nil
}

// WriteSeekTable writes the skippable frame (header + entries + footer)
// to w in the exact byte order a Python reader expects.
//
// Fails if any frame is larger than MaxUint32 in either dimension —
// the on-disk format uses u32 for these fields and we refuse silent
// truncation.
func WriteSeekTable(w io.Writer, frames []FrameInfo) error {
	// u32 ceiling for seek-table entries. We reject anything larger
	// up front — the alternative would be silent truncation which
	// corrupts the seek table.
	const u32Max = 1<<32 - 1
	if len(frames) > u32Max {
		return fmt.Errorf("too many frames (%d) for u32 num_frames field", len(frames))
	}
	entries := make([]byte, 0, len(frames)*EntrySize)
	for _, f := range frames {
		if f.CompressedSize > u32Max || f.DecompressedSize > u32Max {
			return fmt.Errorf("frame %d too large for u32 encoding (compressed=%d decompressed=%d)",
				f.Index, f.CompressedSize, f.DecompressedSize)
		}
		cSize := make([]byte, 4)
		dSize := make([]byte, 4)
		// #nosec G115 -- bounds checked above.
		binary.LittleEndian.PutUint32(cSize, uint32(f.CompressedSize))
		// #nosec G115 -- bounds checked above.
		binary.LittleEndian.PutUint32(dSize, uint32(f.DecompressedSize))
		entries = append(entries, cSize...)
		entries = append(entries, dSize...)
	}

	// Footer: magic (4) + num_frames (4) + flags (1). flags=0 → no checksums.
	footer := make([]byte, FooterSize)
	binary.LittleEndian.PutUint32(footer[0:4], FooterMagic)
	// #nosec G115 -- len(frames) bound-checked above.
	binary.LittleEndian.PutUint32(footer[4:8], uint32(len(frames)))
	footer[8] = 0

	// Skippable frame wraps entries+footer. Its header is
	// (magic:u32) + (frame_size:u32), where frame_size = entries+footer length.
	// The body size cannot exceed u32 because entry count × 8 + 9 < u32Max
	// whenever entry count < u32Max/8, which we just enforced.
	skippableHeader := make([]byte, SkippableHeaderSize)
	binary.LittleEndian.PutUint32(skippableHeader[0:4], SeekableMagic)
	bodyLen := len(entries) + len(footer)
	if bodyLen > u32Max {
		return fmt.Errorf("seek-table body %d exceeds u32 max", bodyLen)
	}
	// #nosec G115 -- bounds checked above.
	bodySize := uint32(bodyLen)
	binary.LittleEndian.PutUint32(skippableHeader[4:8], bodySize)

	if _, err := w.Write(skippableHeader); err != nil {
		return fmt.Errorf("write skippable header: %w", err)
	}
	if _, err := w.Write(entries); err != nil {
		return fmt.Errorf("write seek-table entries: %w", err)
	}
	if _, err := w.Write(footer); err != nil {
		return fmt.Errorf("write footer: %w", err)
	}
	return nil
}

// IsSeekable reports whether the file at path has the seekable-zstd
// footer magic. Fast: O(1) file seeks regardless of file size.
//
// Returns false on I/O errors (missing file, permission denied, too
// short) — callers that need distinguishing info should use
// ReadSeekTable directly and inspect the error.
func IsSeekable(path string) bool {
	// Extension heuristic first — cheap and catches obvious non-matches.
	if !strings.EqualFold(filepath.Ext(path), ".zst") {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() < FooterSize {
		return false
	}
	var footer [FooterSize]byte
	if _, err := f.ReadAt(footer[:], info.Size()-FooterSize); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(footer[0:4]) == FooterMagic
}
