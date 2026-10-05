package journal

// This file reads a journal's header: the first bytes of the file, which
// say how to read the rest. Its layout is systemd's Header struct
// (src/libsystemd/sd-journal/journal-def.h, v257.13).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Signature is the 8 bytes every journal file starts with.
const Signature = "LPKSHHRH"

// State is the header's state byte: whether a writer had the file open.
type State uint8

// The states a header can hold (journal-def.h). A reader may read a
// journal in any of them.
const (
	// StateOffline is a journal closed cleanly and not archived.
	StateOffline State = 0
	// StateOnline is a journal a writer has open, or had open when it
	// stopped without closing it.
	StateOnline State = 1
	// StateArchived is a journal rotated away and closed.
	StateArchived State = 2

	// stateCount is the number of valid states, not a state.
	stateCount = 3
)

// Header is what rx reads of a journal's header. Every field is stored
// little-endian at a fixed offset below 208, the size of the smallest
// header systemd writes, so every header has them all.
type Header struct {
	State State
	// IncompatibleFlags says how the file is written: bit 0, 1 and 3 that
	// values may be compressed with xz, lz4 or zstd, bit 2 that hashes
	// are keyed, bit 4 compact mode. A reader must know every bit set.
	IncompatibleFlags uint32
	// Compact is bit 4 of IncompatibleFlags: offsets are 32-bit and DATA
	// objects 8 bytes longer.
	Compact bool
	// HeaderSize is the header's size in bytes, and the offset of the
	// first object.
	HeaderSize uint64
	// ArenaSize is the number of bytes after the header the file may use
	// for objects: preallocated room, not all of it written.
	ArenaSize uint64
	// TailObjectOffset is the offset of the last object written.
	TailObjectOffset uint64
	// EntryArrayOffset is the offset of the first ENTRY_ARRAY object of
	// the entry list; 0 when the journal has no entry.
	EntryArrayOffset uint64
	// NEntries is the number of entries the entry list holds.
	NEntries uint64
	// SeqnumID names the counter that numbered the entries; the journals
	// of one journald share it.
	SeqnumID [16]byte
	// HeadSeqnum and TailSeqnum are the sequence numbers of the first and
	// the last entry.
	HeadSeqnum uint64
	TailSeqnum uint64
	// HeadRealtime and TailRealtime are the realtime, in microseconds
	// since the epoch, of the first and the last entry.
	HeadRealtime uint64
	TailRealtime uint64
}

// The offsets of the header fields rx reads (journal-def.h).
const (
	offsetIncompatibleFlags = 12
	offsetState             = 16
	offsetSeqnumID          = 72
	offsetHeaderSize        = 88
	offsetArenaSize         = 96
	offsetTailObjectOffset  = 136
	offsetNEntries          = 152
	offsetTailEntrySeqnum   = 160
	offsetHeadEntrySeqnum   = 168
	offsetEntryArrayOffset  = 176
	offsetHeadEntryRealtime = 184
	offsetTailEntryRealtime = 192

	// minHeaderSize is the size of the oldest header systemd writes:
	// every field up to tail_entry_monotonic at offset 200
	// (HEADER_SIZE_MIN, journal-file.c).
	minHeaderSize = 208
)

// The incompatible flags rx knows (journal-def.h). A file with any other
// bit set is written in a way rx cannot read, as libsystemd refuses it
// (journal-file.c:555-560).
const (
	flagCompressedXZ   = 1 << 0
	flagCompressedLZ4  = 1 << 1
	flagKeyedHash      = 1 << 2
	flagCompressedZstd = 1 << 3
	flagCompact        = 1 << 4

	knownIncompatibleFlags = flagCompressedXZ | flagCompressedLZ4 | flagKeyedHash | flagCompressedZstd | flagCompact
)

// ErrUnreadable is wrapped by ReadHeader's error for a file with the
// signature whose header rx cannot read; its text says why.
//
// The error reads "a systemd journal rx cannot read: <why>". A header is
// refused only for what makes the rest of the file unreadable: a
// header_size below 208 or beyond the file, a state of 3 or more, or an
// incompatible flag rx does not know. Offsets that point beyond the file
// are not refused: that is a truncated copy, whose entries are read as
// far as the file goes.
var ErrUnreadable = errors.New("a systemd journal rx cannot read")

// errNotJournal is ReadHeader's error for a file that does not start
// with the signature. It does not wrap ErrUnreadable: such a file is not
// a journal at all.
var errNotJournal = errors.New("not a systemd journal: the file does not start with " + Signature)

// le is the byte order of every integer in a journal. binary.LittleEndian
// reads an integer from the first bytes of a slice (Uint64 reads 8,
// Uint32 4) and panics when the slice is shorter, so every read below
// is from a slice whose length has been checked.
var le = binary.LittleEndian

// ReadHeader reads the header of the journal r, a file of size bytes.
//
// It reads the first 208 bytes once. An error wraps ErrUnreadable when
// the file has the signature but a header rx cannot read, errNotJournal
// when it lacks the signature, and the reader's own error, wrapped, when
// ReadAt fails for another reason than the end of the file.
//
// Go note: io.ReaderAt is the interface of anything that reads bytes at
// a given offset, such as *os.File or *bytes.Reader. ReadAt does not
// move a shared position, so one file can serve several readers.
func ReadHeader(r io.ReaderAt, size int64) (Header, error) {
	var raw [minHeaderSize]byte
	n, err := r.ReadAt(raw[:min(max(size, 0), minHeaderSize)], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		// %w wraps err: errors.Is and errors.As see it inside the new
		// error, which adds where the read failed.
		return Header{}, fmt.Errorf("read the journal header: %w", err)
	}
	if n < len(Signature) || string(raw[:len(Signature)]) != Signature {
		return Header{}, errNotJournal
	}
	if n < minHeaderSize {
		return Header{}, unreadable("the file is %d bytes, shorter than the smallest journal header (%d bytes)", n, minHeaderSize)
	}
	h := parseHeader(raw[:])
	if err := h.check(uint64(size)); err != nil { // #nosec G115 -- size is at least minHeaderSize here
		return Header{}, err
	}
	return h, nil
}

// parseHeader decodes the fields of a header from its first 208 bytes.
func parseHeader(raw []byte) Header {
	h := Header{
		State:             State(raw[offsetState]),
		IncompatibleFlags: le.Uint32(raw[offsetIncompatibleFlags:]),
		HeaderSize:        le.Uint64(raw[offsetHeaderSize:]),
		ArenaSize:         le.Uint64(raw[offsetArenaSize:]),
		TailObjectOffset:  le.Uint64(raw[offsetTailObjectOffset:]),
		EntryArrayOffset:  le.Uint64(raw[offsetEntryArrayOffset:]),
		NEntries:          le.Uint64(raw[offsetNEntries:]),
		HeadSeqnum:        le.Uint64(raw[offsetHeadEntrySeqnum:]),
		TailSeqnum:        le.Uint64(raw[offsetTailEntrySeqnum:]),
		HeadRealtime:      le.Uint64(raw[offsetHeadEntryRealtime:]),
		TailRealtime:      le.Uint64(raw[offsetTailEntryRealtime:]),
	}
	h.Compact = h.IncompatibleFlags&flagCompact != 0
	copy(h.SeqnumID[:], raw[offsetSeqnumID:])
	return h
}

// check refuses a header rx cannot read in a file of size bytes.
func (h Header) check(size uint64) error {
	switch {
	case h.HeaderSize < minHeaderSize:
		return unreadable("header_size %d is below %d", h.HeaderSize, minHeaderSize)
	case h.HeaderSize > size:
		return unreadable("header_size %d is beyond the end of the file (%d bytes)", h.HeaderSize, size)
	case h.State >= stateCount:
		return unreadable("state %d is not offline (0), online (1) or archived (2)", h.State)
	case h.IncompatibleFlags&^knownIncompatibleFlags != 0:
		return unreadable("incompatible flags %#x hold bits rx does not know (%#x)",
			h.IncompatibleFlags, h.IncompatibleFlags&^knownIncompatibleFlags)
	}
	return nil
}

// unreadable returns an error wrapping ErrUnreadable with the reason.
func unreadable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnreadable, fmt.Sprintf(format, args...))
}

// ContentEnd returns the offset just past the tail object of the journal
// r, a file of size bytes: the last object journald wrote, which the
// header names (tail_object_offset), plus that object's size. journald
// grows a journal in steps (8 MiB) and writes its objects into the front
// of that space, so the bytes of a live journal after its tail object are
// zeros that no reader of the text reaches; a reader that reports how far
// it has read reports it against this offset.
//
// The offset is capped at the end of what a reader may read: the file,
// or the arena when the header says it ends sooner. It is that read end
// when the tail object cannot be read: a copy cut short before it, or a
// header field that names no object (outside the arena, inside the
// header, misaligned, or an object whose size does not fit).
//
// It reads the header and the 16 bytes of the tail object's header. The
// error is ReadHeader's, or the reader's own, wrapped, when ReadAt fails
// for another reason than the end of the file.
func ContentEnd(r io.ReaderAt, size int64) (int64, error) {
	h, err := ReadHeader(r, size)
	if err != nil {
		return 0, err
	}
	end := h.readEnd(uint64(size)) // #nosec G115 -- ReadHeader accepted size, so it is at least 208
	tail := h.TailObjectOffset
	if tail < h.HeaderSize || tail%8 != 0 || tail >= end || end-tail < objectHeaderSize {
		return int64(end), nil // #nosec G115 -- end <= size, an int64
	}
	var head [objectHeaderSize]byte
	n, err := r.ReadAt(head[:], int64(tail)) // #nosec G115 -- tail < end <= size
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read the tail object of the journal: %w", err)
	}
	objectSize := le.Uint64(head[offsetObjectSize:])
	if n < objectHeaderSize || objectSize < objectHeaderSize || objectSize > end-tail {
		return int64(end), nil // #nosec G115 -- end <= size, an int64
	}
	return int64(tail + objectSize), nil // #nosec G115 -- tail + objectSize <= end <= size
}

// readEnd returns where the reads of a journal end: the end of the file,
// or the end of the arena when the header says it ends sooner. A sum
// that overflows ends at the file's end.
func (h Header) readEnd(size uint64) uint64 {
	if h.ArenaSize > ^uint64(0)-h.HeaderSize {
		return size
	}
	return min(size, h.HeaderSize+h.ArenaSize)
}
