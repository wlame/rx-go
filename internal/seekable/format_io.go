package seekable

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/wlame/rx-go/internal/compression"
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

// ErrFrameTooLargeToHold reports a seek table that describes its file
// but gives a frame more than compression.WindowLimit of text or of
// compressed bytes. rx decodes a seekable file's frames whole, so it
// does not use such a table: the file is still a zstd stream, and rx
// reads it as plain zstd, through a decoder that holds one window at a
// time instead of the frame. The answer is the same; only the frame by
// frame access is lost. A crafted file of a few kilobytes can give a
// frame 4 GiB of text, so the table is refused before any frame is
// decoded.
var ErrFrameTooLargeToHold = errors.New("a seekable zstd frame is larger than rx decodes whole")

// zstdFrameMagic starts every zstd frame (RFC 8878, section 3.1.1).
const zstdFrameMagic uint32 = 0xFD2FB528

// maxFrameHeaderSize is the longest header a zstd frame starts with:
// magic (4), frame header descriptor (1), window descriptor (1),
// dictionary ID (up to 4) and frame content size (up to 8).
const maxFrameHeaderSize = 18

// footerLayout says where one layout of the 9-byte seek-table footer
// keeps its three fields, and how it reads its descriptor byte.
type footerLayout struct {
	// name says which layout an error is about.
	name string
	// magicAt, framesAt and descriptorAt are the positions of
	// FooterMagic (4 bytes), the frame count (4 bytes) and the
	// descriptor (1 byte) within the footer.
	magicAt, framesAt, descriptorAt int
	// checksumFlag is the descriptor bit that says each entry carries a
	// 4-byte checksum after its two sizes.
	checksumFlag byte
	// reservedBits are descriptor bits that must be zero. A footer with
	// one of them set is not a table of this layout.
	reservedBits byte
}

// footerLayouts are the footer layouts ReadSeekTable reads, in the order
// it prefers them. The first is the zstd seekable format specification's
// (contrib/seekable_format in facebook/zstd): Number_Of_Frames,
// Seek_Table_Descriptor (Checksum_Flag is bit 7, bits 6 to 2 are
// reserved), Seekable_Magic_Number. The second is the layout rx-go up to
// v0.3.0 and rx-python write: magic, frame count, flags (bit 0 is the
// checksum flag; the other bits were never defined, so they are not
// checked).
var footerLayouts = [...]footerLayout{
	{name: "zstd seekable format", framesAt: 0, descriptorAt: 4, magicAt: 5, checksumFlag: 0x80, reservedBits: 0x7C},
	{name: "legacy rx", magicAt: 0, framesAt: 4, descriptorAt: 8, checksumFlag: 0x01},
}

// ReadSeekTable parses the seek table at the tail of a seekable zstd
// file. The caller provides a ReaderAt and the full file size so we
// can probe absolute offsets without a seek.
//
// Algorithm:
//  1. Read the last 9 bytes (the footer) once.
//  2. For each layout in footerLayouts whose magic is where that layout
//     keeps it, read the table that layout describes and check it
//     against the file (readTableIn). The first table that describes the
//     file is the answer.
//
// Only crafted bytes can carry the magic in both places: both frame
// counts would then exceed 2^31, a table of more than 16 GiB. Each
// table is checked, and only one that describes the file is used.
//
// Returns ErrNotSeekable if neither layout finds its magic, and
// io.ErrUnexpectedEOF for a file shorter than a footer. A table that
// does not describe the file is ErrSeekTableMismatch, from the
// preferred layout that found its magic: a parsed table is never
// returned unchecked. A table that describes the file but gives a
// frame more than rx decodes whole is ErrFrameTooLargeToHold.
func ReadSeekTable(r io.ReaderAt, fileSize int64) (*SeekTable, error) {
	if fileSize < FooterSize {
		return nil, io.ErrUnexpectedEOF
	}
	var footer [FooterSize]byte
	if _, err := r.ReadAt(footer[:], fileSize-FooterSize); err != nil {
		return nil, fmt.Errorf("read footer: %w", err)
	}
	var firstErr error
	for _, layout := range footerLayouts {
		if binary.LittleEndian.Uint32(footer[layout.magicAt:]) != FooterMagic {
			continue
		}
		tbl, err := readTableIn(r, fileSize, footer, layout)
		if err == nil {
			if tooLarge := checkFramesCanBeHeld(tbl); tooLarge != nil {
				return nil, tooLarge
			}
			return tbl, nil
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s footer: %w", layout.name, err)
		}
	}
	if firstErr == nil {
		return nil, ErrNotSeekable
	}
	return nil, firstErr
}

// readTableIn reads the seek table that footer, read in layout,
// describes, and returns it only when it describes the file of fileSize
// bytes:
//
//   - the descriptor sets no reserved bit;
//   - the table fits in the file, and the skippable frame that carries
//     it starts with the seek-table magic and a length that runs to the
//     end of the file (checkTableFrame, read before the entries are
//     allocated);
//   - its frames end where the table starts, and each starts with a zstd
//     frame header that agrees with its entry (validateSeekTable).
//
// Bounds: the entries buffer is the table's size, which the second
// check keeps within the file; the frame list holds one FrameInfo per
// entry. It reads the footer's table and at most maxFrameHeaderSize
// bytes per frame.
func readTableIn(r io.ReaderAt, fileSize int64, footer [FooterSize]byte, layout footerLayout) (*SeekTable, error) {
	numFrames := int64(binary.LittleEndian.Uint32(footer[layout.framesAt:]))
	descriptor := footer[layout.descriptorAt]
	if reserved := descriptor & layout.reservedBits; reserved != 0 {
		return nil, fmt.Errorf("%w: the descriptor %#x sets reserved bits %#x", ErrSeekTableMismatch, descriptor, reserved)
	}
	entrySize := int64(EntrySize)
	if descriptor&layout.checksumFlag != 0 {
		// The checksums are read past and never checked: verifying one
		// means decompressing its frame, which reading a table must not.
		entrySize = ChecksumEntrySize
	}
	// The skippable frame is its 8-byte header, the entries and the
	// footer, and it ends the file.
	tableStart := fileSize - FooterSize - numFrames*entrySize - SkippableHeaderSize
	if tableStart < 0 {
		return nil, fmt.Errorf("%w: a table of %d entries does not fit in a file of %d bytes",
			ErrSeekTableMismatch, numFrames, fileSize)
	}
	if err := checkTableFrame(r, fileSize, tableStart); err != nil {
		return nil, err
	}

	entries := make([]byte, numFrames*entrySize)
	if _, err := r.ReadAt(entries, tableStart+SkippableHeaderSize); err != nil {
		return nil, fmt.Errorf("read seek-table entries: %w", err)
	}
	frames := make([]FrameInfo, numFrames)
	var cOff, dOff int64
	for i := range frames {
		entry := entries[int64(i)*entrySize:]
		cSize := int64(binary.LittleEndian.Uint32(entry[0:4]))
		dSize := int64(binary.LittleEndian.Uint32(entry[4:8]))
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
		NumFrames: len(frames),
		Flags:     descriptor,
		Frames:    frames,
	}
	if err := validateSeekTable(r, tableStart, tbl); err != nil {
		return nil, err
	}
	return tbl, nil
}

// checkTableFrame checks that the skippable frame at tableStart, in a
// file of fileSize bytes, carries the seek-table magic and a length that
// runs to the end of the file. It reads the frame's 8-byte header.
func checkTableFrame(r io.ReaderAt, fileSize, tableStart int64) error {
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
	return nil
}

// validateSeekTable checks that tbl, parsed from a file whose
// seek-table skippable frame starts at tableStart (already checked by
// checkTableFrame), describes the frames before it, whatever footer
// layout it was read from:
//
//   - the frames, laid end to end from byte 0, end exactly at
//     tableStart;
//   - each frame starts with a zstd frame header, and a header that
//     records the frame's content size records the entry's decompressed
//     size.
//
// It reads at most maxFrameHeaderSize bytes per frame, one ReadAt each,
// and never a frame's data. The frame count is bounded by the table,
// which fits in the file. An error wraps ErrSeekTableMismatch and says
// what does not agree.
func validateSeekTable(r io.ReaderAt, tableStart int64, tbl *SeekTable) error {
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
// to w, with the footer in the zstd seekable format specification's
// layout, so other tools that follow the specification can seek in the
// file. rx-python reads only the legacy rx layout and takes such a file
// for plain zstd.
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

	// Footer, in the zstd seekable format specification's layout:
	// num_frames (4), descriptor (1), magic (4). A zero descriptor says
	// the entries carry no checksums.
	footer := make([]byte, FooterSize)
	// #nosec G115 -- len(frames) bound-checked above.
	binary.LittleEndian.PutUint32(footer[0:4], uint32(len(frames)))
	footer[4] = 0
	binary.LittleEndian.PutUint32(footer[5:9], FooterMagic)

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

// checkFramesCanBeHeld returns an error wrapping ErrFrameTooLargeToHold
// for the first frame of tbl too large to decode whole (frameTooLarge).
func checkFramesCanBeHeld(tbl *SeekTable) error {
	for _, frame := range tbl.Frames {
		if frameTooLarge(frame) {
			return fmt.Errorf("%w: frame %d holds %d bytes of text in %d bytes, more than the %d allowed",
				ErrFrameTooLargeToHold, frame.Index, frame.DecompressedSize, frame.CompressedSize, compression.WindowLimit)
		}
	}
	return nil
}

// frameTooLarge reports whether frame holds more text, or more
// compressed bytes, than compression.WindowLimit: more than a reader
// that decodes the frame whole may hold.
func frameTooLarge(frame FrameInfo) bool {
	return frame.DecompressedSize > compression.WindowLimit || frame.CompressedSize > compression.WindowLimit
}
