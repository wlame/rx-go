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

// ErrSeekTableMismatch reports that a file ends with a seek-table
// footer whose table does not describe the file: its frames do not end
// where the table starts, the table's skippable frame header is wrong,
// or a frame does not start with a zstd frame header that agrees with
// its entry. Two seekable files joined with `cat` end with the second
// file's table, which describes the second file only; a damaged table
// describes nothing reliably. Such a file is still a zstd stream, and
// rx reads it as plain zstd.
var ErrSeekTableMismatch = errors.New("seek table does not describe the file")

// zstdFrameMagic starts every zstd frame (RFC 8878, section 3.1.1).
const zstdFrameMagic uint32 = 0xFD2FB528

// maxFrameHeaderSize is the longest header a zstd frame starts with:
// magic (4), frame header descriptor (1), window descriptor (1),
// dictionary ID (up to 4) and frame content size (up to 8).
const maxFrameHeaderSize = 18

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
// for truncated files. A table that does not describe the file is
// ErrSeekTableMismatch (see validateSeekTable): a parsed table is never
// returned unchecked.
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
	tbl := &SeekTable{
		NumFrames: numFrames,
		Flags:     flags,
		Frames:    frames,
	}
	// The skippable frame that carries the table starts with its 8-byte
	// header, just before the entries.
	if err := validateSeekTable(r, fileSize, entriesStart-SkippableHeaderSize, tbl); err != nil {
		return nil, err
	}
	return tbl, nil
}

// validateSeekTable checks that tbl, parsed from a file of fileSize
// bytes whose seek-table skippable frame starts at tableStart,
// describes that file. It is the one check every parsed table goes
// through, whatever footer layout it was read from:
//
//   - the skippable frame at tableStart carries the seek-table magic and
//     a length that runs to the end of the file;
//   - the frames, laid end to end from byte 0, end exactly at
//     tableStart;
//   - each frame starts with a zstd frame header, and a header that
//     records the frame's content size records the entry's decompressed
//     size.
//
// It reads the 8-byte skippable header and at most maxFrameHeaderSize
// bytes per frame, one ReadAt each, and never a frame's data. The frame
// count is bounded by the table, which fits in the file. An error wraps
// ErrSeekTableMismatch and says what does not agree.
func validateSeekTable(r io.ReaderAt, fileSize, tableStart int64, tbl *SeekTable) error {
	if tableStart < 0 {
		return fmt.Errorf("%w: the table would start before the file", ErrSeekTableMismatch)
	}
	var header [SkippableHeaderSize]byte
	if _, err := r.ReadAt(header[:], tableStart); err != nil {
		return fmt.Errorf("read seek-table frame header: %w", err)
	}
	if magic := binary.LittleEndian.Uint32(header[0:4]); magic != SeekableMagic {
		return fmt.Errorf("%w: the table's frame starts with %#x, not the seek-table magic", ErrSeekTableMismatch, magic)
	}
	if length := int64(binary.LittleEndian.Uint32(header[4:8])); length != fileSize-tableStart-SkippableHeaderSize {
		return fmt.Errorf("%w: the table's frame is %d bytes long, the file leaves %d",
			ErrSeekTableMismatch, length, fileSize-tableStart-SkippableHeaderSize)
	}
	end := int64(0)
	if tbl.NumFrames > 0 {
		end = tbl.Frames[tbl.NumFrames-1].CompressedEnd()
	}
	if end != tableStart {
		return fmt.Errorf("%w: its %d frames end at byte %d, the table starts at byte %d",
			ErrSeekTableMismatch, tbl.NumFrames, end, tableStart)
	}
	for _, frame := range tbl.Frames {
		if err := checkFrameHeader(r, frame); err != nil {
			return err
		}
	}
	return nil
}

// checkFrameHeader reads the header of frame and checks that it is a
// zstd frame header and that the content size it records, when it
// records one, is the entry's decompressed size. An entry of no bytes
// and no text is an empty frame: zstd encoders write nothing for an
// empty input, and there is no header to check.
func checkFrameHeader(r io.ReaderAt, frame FrameInfo) error {
	if frame.CompressedSize == 0 && frame.DecompressedSize == 0 {
		return nil
	}
	var buf [maxFrameHeaderSize]byte
	header := buf[:min(frame.CompressedSize, maxFrameHeaderSize)]
	if _, err := r.ReadAt(header, frame.CompressedOffset); err != nil {
		return fmt.Errorf("read the header of frame %d: %w", frame.Index, err)
	}
	contentSize, hasContentSize, ok := parseFrameHeader(header)
	if !ok {
		return fmt.Errorf("%w: frame %d at byte %d does not start with a zstd frame header",
			ErrSeekTableMismatch, frame.Index, frame.CompressedOffset)
	}
	if hasContentSize && contentSize != uint64(frame.DecompressedSize) { // #nosec G115 -- a u32 from the table
		return fmt.Errorf("%w: frame %d holds %d bytes of text, the table says %d",
			ErrSeekTableMismatch, frame.Index, contentSize, frame.DecompressedSize)
	}
	return nil
}

// frameContentSizeBytes is the length of the Frame_Content_Size field
// for each value of the descriptor's two top bits (RFC 8878, section
// 3.1.1.1.1.1); flag 0 means 1 byte in a single-segment frame and none
// otherwise.
var frameContentSizeBytes = [4]int{0, 2, 4, 8}

// dictionaryIDBytes is the length of the Dictionary_ID field for each
// value of the descriptor's two low bits.
var dictionaryIDBytes = [4]int{0, 1, 2, 4}

// parseFrameHeader reads a zstd frame header from the start of header
// and returns the content size it records, whether it records one, and
// whether header starts with a whole zstd frame header at all.
func parseFrameHeader(header []byte) (contentSize uint64, hasContentSize, ok bool) {
	if len(header) < 5 || binary.LittleEndian.Uint32(header[0:4]) != zstdFrameMagic {
		return 0, false, false
	}
	descriptor := header[4]
	singleSegment := descriptor&0x20 != 0
	sizeBytes := frameContentSizeBytes[descriptor>>6]
	if sizeBytes == 0 && singleSegment {
		sizeBytes = 1
	}
	at := 5 + dictionaryIDBytes[descriptor&0x03]
	if !singleSegment {
		at++ // the window descriptor
	}
	if len(header) < at+sizeBytes {
		return 0, false, false
	}
	if sizeBytes == 0 {
		return 0, false, true
	}
	var field [8]byte
	copy(field[:], header[at:at+sizeBytes])
	contentSize = binary.LittleEndian.Uint64(field[:])
	if sizeBytes == 2 {
		contentSize += 256 // a 2-byte field stores the size less 256
	}
	return contentSize, true, true
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

// IsSeekable reports whether the file at path is named .zst and ends
// with a seek table that describes it (ReadSeekTable succeeds). It
// reads the table and one frame header per frame, never frame data.
// A file with the footer magic and a table that does not add up is not
// seekable: it is read as plain zstd.
//
// Returns false on I/O errors (missing file, permission denied, too
// short) — callers that need distinguishing info should use
// ReadSeekTable directly and inspect the error.
func IsSeekable(path string) bool {
	// Extension heuristic first — cheap and catches obvious non-matches.
	if !HasSeekableExtension(path) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return hasValidSeekTable(f, info.Size())
}

// IsSeekableFile is IsSeekable for a file the caller already has open,
// size bytes long, such as one opened through a pin: name gives the
// extension, and the table is read from r. Nothing is looked up by
// path, so the answer is about the file that is open.
func IsSeekableFile(name string, r io.ReaderAt, size int64) bool {
	return HasSeekableExtension(name) && hasValidSeekTable(r, size)
}

// HasSeekableExtension reports whether name ends in .zst, in any case:
// the only name a seekable file is looked for under.
func HasSeekableExtension(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".zst")
}

// hasValidSeekTable reports whether r, a file of size bytes, ends with
// a seek table that describes it. A read error is false.
func hasValidSeekTable(r io.ReaderAt, size int64) bool {
	_, err := ReadSeekTable(r, size)
	return err == nil
}
