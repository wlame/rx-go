package journal

// This file reads the objects of a journal: every object starts with a
// 16-byte header (type, flags, 6 reserved bytes, size), on an 8-byte
// boundary after the file header. The layouts are systemd's
// (journal-def.h, v257.13); the checks follow journal-file.c's
// check_object_header and check_object (lines 890-1082).
//
// SECURITY: a journal may be hostile. Every offset and size read from
// it is checked against the end of what may be read before it is used,
// no buffer is sized from a value the file declares before that value is
// checked, and an object's items are read in pieces of a fixed size.

import (
	"errors"
	"fmt"
	"io"
)

// Object types (journal-def.h).
const (
	objectData       = 1
	objectEntry      = 3
	objectEntryArray = 6
)

// objectNames names each object type in a reason: "a DATA object".
var objectNames = map[uint8]string{
	0: "an UNUSED object", 1: "a DATA object", 2: "a FIELD object", 3: "an ENTRY object",
	4: "a DATA_HASH_TABLE object", 5: "a FIELD_HASH_TABLE object", 6: "an ENTRY_ARRAY object", 7: "a TAG object",
}

// objectKinds is the short name of each type rx reads, for the subject of
// a reason: "the ENTRY object at offset 4096".
var objectKinds = map[uint8]string{objectData: "DATA", objectEntry: "ENTRY", objectEntryArray: "ENTRY_ARRAY"}

// Object layout: offsets count from the object's start.
const (
	objectHeaderSize = 16
	offsetObjectSize = 8

	// An ENTRY object: its sequence number, times, boot and items.
	offsetEntrySeqnum    = 16
	offsetEntryRealtime  = 24
	offsetEntryMonotonic = 32
	offsetEntryBootID    = 40
	entryItemsOffset     = 64

	// A DATA object: the links check_object checks, and the payload.
	offsetDataNextHash       = 24
	offsetDataNextField      = 32
	offsetDataEntryOffset    = 40
	offsetDataEntryArray     = 48
	offsetDataNEntries       = 56
	dataPayloadOffsetRegular = 64
	dataPayloadOffsetCompact = 72 // compact mode adds two uint32 before the payload

	// An ENTRY_ARRAY object: the next array of the list, then the items.
	offsetNextEntryArray  = 16
	entryArrayItemsOffset = 24
)

// How much is read at once. The first read of an object takes its header
// and, for most objects, all of it, so that one ReadAt reads an object;
// a larger object takes a second read, and the items of a large ENTRY or
// ENTRY_ARRAY object are read in pieces of itemChunkBytes.
const (
	entryProbeBytes = 512  // an ENTRY object with up to 28 regular or 112 compact items
	dataProbeBytes  = 1024 // a DATA object whose value journald stores uncompressed (under 512 bytes)
	arrayProbeBytes = 4096
	itemChunkBytes  = 64 << 10
)

// maxValidTime is the first time systemd does not accept as a realtime
// or a monotonic time: 2^55 microseconds (VALID_REALTIME and
// VALID_MONOTONIC, journal-file.h:172-180).
const maxValidTime = 1 << 55

// damageError is the error of a read that found the journal's bytes not
// as the format requires: an object outside the file, misaligned, of the
// wrong type or size, a pointer that goes back, a value that does not
// decode. It is counted in the Report, never returned by Read.
type damageError struct{ reason string }

func (d *damageError) Error() string { return d.reason }

// damaged returns a damageError with the formatted reason.
func damaged(format string, args ...any) error {
	return &damageError{reason: fmt.Sprintf(format, args...)}
}

// damageReason returns the reason of err when it is damage.
//
// Go note: errors.As walks the chain of wrapped errors and, when it finds
// one of the target's type (*damageError), stores it in the target.
func damageReason(err error) (string, bool) {
	if err == nil {
		// Returning here keeps d below from being made: errors.As takes
		// its address as an interface, which moves d to the heap.
		return "", false
	}
	var d *damageError
	if errors.As(err, &d) {
		return d.reason, true
	}
	return "", false
}

// journalFile is a journal open for reading: the file and the bounds of
// every read.
type journalFile struct {
	r io.ReaderAt
	// size is the file's size as the caller gave it, end where reads end:
	// size, or the end of the arena when the header says it ends sooner.
	size, end  uint64
	headerSize uint64
	compact    bool
}

// errShortRead says that ReadAt ended at the end of the file before the
// bytes asked for: the file is shorter than its size said.
var errShortRead = errors.New("the file ends before the bytes asked for")

// readAtLeast reads into p, which lies before f.end, the bytes at off,
// and returns how many it read: at least need, or errShortRead when the
// file ended sooner. Another failure of ReadAt (a disk error, a cancelled
// read) is returned wrapped: it says nothing about the journal's bytes.
func (f *journalFile) readAtLeast(p []byte, off uint64, need int) (int, error) {
	// Go note: io.ReaderAt may return io.EOF together with the bytes of
	// a read that reaches the end of the file, so the count decides.
	n, err := f.r.ReadAt(p, int64(off)) // #nosec G115 -- off < f.end <= the int64 size the caller gave
	if n >= need {
		return n, nil
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return n, errShortRead
	}
	return n, fmt.Errorf("read the journal at offset %d: %w", off, err)
}

// beyondEnd returns the damage of an object that lies or runs beyond the
// end of what may be read: the file, or the arena the header declares.
func (f *journalFile) beyondEnd(what objectRef, verb string) error {
	if f.end < f.size {
		return damaged("%s %s beyond the end of the arena (which ends at offset %d)", what, verb, f.end)
	}
	return damaged("%s %s beyond the end of the file (%d bytes)", what, verb, f.size)
}

// object is one object as read: its header and its first bytes.
type object struct {
	offset uint64
	flags  uint8
	size   uint64
	// head is the object's first bytes: min(size, the bytes read), and at
	// least the minimum size the caller asked for.
	head []byte
}

// objectRef names an object in a reason: "the ENTRY object at offset
// 4096". It is a plain value, formatted only when a reason is written, so
// that reading an object costs no formatting.
type objectRef struct {
	kind   uint8
	offset uint64
}

// String formats the reference.
//
// Go note: having this method makes objectRef a fmt.Stringer, so %s in
// a format prints what String returns.
func (r objectRef) String() string {
	return fmt.Sprintf("the %s object at offset %d", objectKinds[r.kind], r.offset)
}

// readObject reads the object at offset, which should be of type want and
// at least minSize bytes, with one ReadAt into buf (up to len(buf) bytes),
// and checks what every object must be: at a multiple of 8 after the
// header, of a size that holds its 16-byte header and minSize, of type
// want, and wholly before f.end. A failed check is damage.
func (f *journalFile) readObject(offset uint64, want uint8, minSize uint64, buf []byte) (object, error) {
	what := objectRef{want, offset}
	switch {
	case offset%8 != 0:
		return object{}, damaged("%s is not at a multiple of 8", what)
	case offset < f.headerSize:
		return object{}, damaged("%s lies inside the header", what)
	case offset >= f.end:
		return object{}, f.beyondEnd(what, "lies")
	case f.end-offset < objectHeaderSize:
		return object{}, f.beyondEnd(what, "runs")
	}
	readable := min(uint64(len(buf)), f.end-offset)
	n, err := f.readAtLeast(buf[:readable], offset, objectHeaderSize)
	if errors.Is(err, errShortRead) {
		return object{}, damaged("the file ends inside %s", what)
	}
	if err != nil {
		return object{}, err
	}
	o := object{offset: offset, flags: buf[1], size: le.Uint64(buf[offsetObjectSize:])}
	switch kind := buf[0]; {
	case o.size == 0:
		return object{}, damaged("%s has size 0", what)
	case o.size < objectHeaderSize:
		return object{}, damaged("%s has size %d, smaller than its 16-byte header", what, o.size)
	case kind != want:
		return object{}, damaged("the object at offset %d is %s, not %s", offset, typeName(kind), objectNames[want])
	case o.size < minSize:
		return object{}, damaged("%s has size %d, below the %d bytes it needs", what, o.size, minSize)
	case o.size > f.end-offset:
		return object{}, f.beyondEnd(what, "runs")
	case uint64(n) < min(o.size, readable): // #nosec G115 -- n is a byte count
		// The object lies before f.end, but the file ended inside it.
		return object{}, damaged("the file ends inside %s", what)
	}
	o.head = buf[:min(uint64(n), o.size)] // #nosec G115 -- n is a byte count
	return o, nil
}

// typeName names an object type found in a file.
func typeName(kind uint8) string {
	if name, ok := objectNames[kind]; ok {
		return name
	}
	return fmt.Sprintf("an object of unknown type %d", kind)
}

// itemCursor hands out the items of an ENTRY or ENTRY_ARRAY object in
// order: first those its first read brought, then the rest, read into
// chunk in pieces of at most len(chunk) bytes. However many items an
// object declares, the cursor holds one chunk.
type itemCursor struct {
	f     *journalFile
	what  objectRef // the object, for reasons
	width uint64    // an item's size in the file: 4, 8 or 16 bytes
	next  uint64    // the file offset of the first item not read yet
	left  uint64    // the items not handed out yet
	buf   []byte    // items read and not handed out yet, whole items only
	chunk []byte    // the buffer the items are read into
}

// start makes the cursor hand out the items of o, which start at
// itemsOffset and are width bytes each. o.head must lie in chunk, where
// readObject read it; the items it holds are handed out from there.
func (c *itemCursor) start(f *journalFile, o object, kind uint8, itemsOffset, width uint64, chunk []byte) {
	count := (o.size - itemsOffset) / width
	inHead := min(count, (uint64(len(o.head))-itemsOffset)/width)
	*c = itemCursor{
		f:     f,
		what:  objectRef{kind, o.offset},
		width: width,
		next:  o.offset + itemsOffset + inHead*width,
		left:  count,
		buf:   o.head[itemsOffset : itemsOffset+inHead*width],
		chunk: chunk,
	}
}

// nextItem returns the offset the next item holds (the first 4 or 8 bytes
// of the item). The caller checks left first.
func (c *itemCursor) nextItem() (uint64, error) {
	if uint64(len(c.buf)) < c.width {
		pieceItems := min(c.left, uint64(len(c.chunk))/c.width)
		piece := c.chunk[:pieceItems*c.width]
		if _, err := c.f.readAtLeast(piece, c.next, len(piece)); err != nil {
			if errors.Is(err, errShortRead) {
				return 0, damaged("the file ends inside %s", c.what)
			}
			return 0, err
		}
		c.buf, c.next = piece, c.next+uint64(len(piece))
	}
	item := c.buf[:c.width]
	c.buf = c.buf[c.width:]
	c.left--
	if c.width == 4 {
		return uint64(le.Uint32(item)), nil
	}
	return le.Uint64(item), nil
}
