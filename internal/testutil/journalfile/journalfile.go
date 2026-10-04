// Package journalfile writes small systemd journal files for tests: the
// entries a test chooses, regular or compact, with values stored plain or
// compressed with xz, lz4 or zstd, and the place of every object it
// wrote, so that a test can damage a chosen object.
//
// The files follow the layout systemd 257 writes (journal-def.h and
// journal-file.c), with what a reader that walks the entry list needs:
// the header, the DATA objects, the ENTRY objects and the chain of
// ENTRY_ARRAY objects. They hold no hash tables and no FIELD objects,
// and every hash is 0: those serve lookups by value, which a reader of
// the whole text never makes. journalctl --file reads them (checked with
// systemd 257.13).
//
// The package does not import internal/journal, whose own tests use it;
// it keeps its own copy of the few layout constants it needs.
package journalfile

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// Compression is how the DATA objects of a journal may store their
// values, and the header flag that says so.
type Compression string

// The compressions a journal can use.
const (
	None Compression = "none"
	XZ   Compression = "xz"
	LZ4  Compression = "lz4"
	Zstd Compression = "zstd"
)

// Compressions lists every compression, for tests that run over each.
var Compressions = []Compression{None, XZ, LZ4, Zstd}

// The header of the files this package writes: systemd 257's size, and
// the offset of every field it fills (journal-def.h). Every integer is
// little-endian.
const (
	HeaderSize = 272

	offsetCompatibleFlags      = 8
	offsetIncompatibleFlags    = 12
	offsetState                = 16
	offsetFileID               = 24
	offsetMachineID            = 40
	offsetTailEntryBootID      = 56
	offsetSeqnumID             = 72
	offsetHeaderSize           = 88
	offsetArenaSize            = 96
	offsetTailObjectOffset     = 136
	offsetNObjects             = 144
	OffsetNEntries             = 152
	offsetTailEntrySeqnum      = 160
	offsetHeadEntrySeqnum      = 168
	offsetEntryArrayOffset     = 176
	offsetHeadEntryRealtime    = 184
	offsetTailEntryRealtime    = 192
	offsetTailEntryMonotonic   = 200
	offsetNData                = 208
	offsetNEntryArrays         = 232
	offsetTailEntryArrayOffset = 256
	offsetTailEntryArrayN      = 260
	offsetTailEntryOffset      = 264
)

// Header flags. The compatible flag TAIL_ENTRY_BOOT_ID says the header's
// tail_entry_boot_id is the boot of the last entry.
const (
	compatibleTailEntryBootID = 1 << 1
	incompatibleCompact       = 1 << 4
)

// headerFlags is the incompatible header flag of each compression, and
// objectFlags the flag of a DATA object stored with it.
var (
	headerFlags = map[Compression]uint32{None: 0, XZ: 1 << 0, LZ4: 1 << 1, Zstd: 1 << 3}
	objectFlags = map[Compression]uint8{None: 0, XZ: 1, LZ4: 2, Zstd: 4}
)

// Object types and layout (journal-def.h). Offsets count from the start
// of the object; every object starts on an 8-byte boundary.
const (
	ObjectData       = 1
	ObjectEntry      = 3
	ObjectEntryArray = 6

	objectHeaderSize = 16
	// OffsetObjectSize is where an object's size (uint64, its 16-byte
	// header included) is.
	OffsetObjectSize = 8

	dataPayloadRegular = 64
	dataPayloadCompact = 72

	entryOffsetSeqnum    = 16
	entryOffsetRealtime  = 24
	entryOffsetMonotonic = 32
	entryOffsetBootID    = 40
	entryItems           = 64

	// OffsetNextEntryArray is where an ENTRY_ARRAY object holds the
	// offset of the next array of the list.
	OffsetNextEntryArray = 16
	entryArrayItems      = 24
)

// defaultCompressThreshold is the smallest value journald compresses
// (DEFAULT_COMPRESS_THRESHOLD in journal-file.c).
const defaultCompressThreshold = 512

// Invented ids, used when Options leaves one zero.
var (
	defaultFileID    = [16]byte{0x0f, 0x11, 0xe1, 0xd0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	defaultMachineID = [16]byte{0x3a, 0xc4, 0x14, 0xe1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	defaultBootID    = [16]byte{0xb0, 0x07, 0x1d, 0x00, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	defaultSeqnumID  = [16]byte{0x5e, 0x91, 0x0d, 0x00, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
)

// Entry is one entry to write.
type Entry struct {
	// Seqnum is the entry's sequence number; 0 means the previous
	// entry's plus one, or Options.FirstSeqnum for the first entry.
	Seqnum uint64
	// Realtime and Monotonic are the entry's times, in microseconds,
	// written as given (a test may write an invalid realtime).
	Realtime  uint64
	Monotonic uint64
	// BootID is the entry's boot; zero means Options.BootID.
	BootID [16]byte
	// Payloads are the entry's items as journald stores them, each
	// "NAME=value" (Payload builds one). Equal payloads share one DATA
	// object across the journal, and an entry lists each DATA object
	// once, sorted by its offset, as journald does.
	Payloads [][]byte
}

// Payload returns the payload "name=value".
func Payload(name string, value []byte) []byte {
	return append(append([]byte(name), '='), value...)
}

// Options says how to write a journal. The zero value writes a regular,
// uncompressed, offline journal with invented ids.
type Options struct {
	Compact     bool
	Compression Compression
	// CompressThreshold is the smallest payload stored compressed; 0
	// means journald's 512 bytes. A payload is stored compressed only
	// when that makes it smaller, as journald does.
	CompressThreshold int
	// State is the header's state: 0 offline, 1 online, 2 archived.
	State uint8
	// FirstSeqnum numbers the first entry whose Seqnum is 0; 0 means 1.
	FirstSeqnum uint64
	// SeqnumID, FileID, MachineID and BootID are the header's ids and the
	// entries' default boot; zero means an invented id.
	SeqnumID, FileID, MachineID, BootID [16]byte
	// ArrayCapacity is the number of items of every ENTRY_ARRAY object;
	// 0 means journald's growth, where each new array holds about twice
	// the entries listed before it (journal-file.c, link_entry_into_array).
	ArrayCapacity int
}

// Object is the place of one object in a written journal.
type Object struct {
	Offset uint64
	// Size is the object's size field: its 16-byte header and its body,
	// without the padding to the next 8-byte boundary.
	Size uint64
}

// End returns the offset just after the object's bytes.
func (o Object) End() uint64 { return o.Offset + o.Size }

// DataObject is a written DATA object.
type DataObject struct {
	Object
	// Payload is the value as the entry gave it, before compression.
	Payload []byte
	// StoredAs is the compression the object is stored with; None for a
	// value under the threshold or one compression did not make smaller.
	StoredAs Compression
	// PayloadOffset is where the stored payload starts in the file.
	PayloadOffset uint64
}

// EntryObject is a written ENTRY object.
type EntryObject struct {
	Object
	Seqnum   uint64
	Realtime uint64
	// Items are the indexes in Journal.Data of the entry's DATA objects,
	// in the order the entry lists them (by offset).
	Items []int
}

// ArrayObject is a written ENTRY_ARRAY object.
type ArrayObject struct {
	Object
	// FirstEntry is the index in Journal.Entries of the array's first
	// item, Count the items the array holds, Capacity its slots.
	FirstEntry, Count, Capacity int
}

// Journal is a written journal: its bytes and the place of its objects.
type Journal struct {
	Bytes   []byte
	Data    []DataObject
	Entries []EntryObject
	// Arrays are the ENTRY_ARRAY objects in list order, which is also
	// file order.
	Arrays []ArrayObject
}

// EntryPayloads returns the payloads of entry i in the order the entry
// lists them, which is the order a reader of the file gives them.
func (j Journal) EntryPayloads(i int) [][]byte {
	payloads := make([][]byte, 0, len(j.Entries[i].Items))
	for _, item := range j.Entries[i].Items {
		payloads = append(payloads, j.Data[item].Payload)
	}
	return payloads
}

// Write returns a journal holding entries, written as opts says. It
// fails the test when a value cannot be compressed.
func Write(tb testing.TB, entries []Entry, opts Options) Journal {
	tb.Helper()
	b := newBuilder(tb, opts)
	defer b.close()
	for _, e := range entries {
		b.appendEntry(e)
	}
	b.writeHeader()
	b.journal.Bytes = b.buf
	return b.journal
}

// builder holds a journal while it is written.
type builder struct {
	tb      testing.TB
	opts    Options
	buf     []byte
	journal Journal
	// dataIndex finds the DATA object of a payload already written.
	dataIndex map[string]int
	nObjects  uint64
	tail      uint64 // offset of the last object written
	seqnum    uint64 // the last sequence number written
	zstd      *zstd.Encoder
}

func newBuilder(tb testing.TB, opts Options) *builder {
	if opts.Compression == "" {
		opts.Compression = None
	}
	if _, ok := headerFlags[opts.Compression]; !ok {
		tb.Fatalf("journalfile: unknown compression %q", opts.Compression)
	}
	if opts.CompressThreshold == 0 {
		opts.CompressThreshold = defaultCompressThreshold
	}
	if opts.FirstSeqnum == 0 {
		opts.FirstSeqnum = 1
	}
	for id, fallback := range map[*[16]byte][16]byte{
		&opts.SeqnumID: defaultSeqnumID, &opts.FileID: defaultFileID,
		&opts.MachineID: defaultMachineID, &opts.BootID: defaultBootID,
	} {
		if *id == ([16]byte{}) {
			*id = fallback
		}
	}
	b := &builder{tb: tb, opts: opts, buf: make([]byte, HeaderSize), dataIndex: map[string]int{}}
	b.seqnum = opts.FirstSeqnum - 1
	if opts.Compression == Zstd {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			tb.Fatalf("journalfile: zstd encoder: %v", err)
		}
		b.zstd = encoder
	}
	return b
}

// close releases the zstd encoder's goroutines.
func (b *builder) close() {
	if b.zstd != nil {
		_ = b.zstd.Close()
	}
}

// le is the byte order of every integer in a journal.
var le = binary.LittleEndian

// appendObject appends an object of the given type, flags and body at
// the next 8-byte boundary and returns its offset and size.
func (b *builder) appendObject(objectType, flags uint8, body []byte) Object {
	for len(b.buf)%8 != 0 {
		b.buf = append(b.buf, 0)
	}
	o := Object{Offset: uint64(len(b.buf)), Size: uint64(objectHeaderSize + len(body))}
	var header [objectHeaderSize]byte
	header[0], header[1] = objectType, flags
	le.PutUint64(header[OffsetObjectSize:], o.Size)
	b.buf = append(append(b.buf, header[:]...), body...)
	b.nObjects++
	b.tail = o.Offset
	return o
}

// dataPayloadOffset is where a DATA object's payload starts.
func (b *builder) dataPayloadOffset() int {
	if b.opts.Compact {
		return dataPayloadCompact
	}
	return dataPayloadRegular
}

// dataObject returns the index of the DATA object holding payload,
// appending one when the journal has none yet.
func (b *builder) dataObject(payload []byte) int {
	if i, ok := b.dataIndex[string(payload)]; ok {
		return i
	}
	stored, storedAs := b.compress(payload)
	// The body before the payload holds the hash, the hash and field
	// chains and the entry links, all 0: no lookup by value reads them.
	body := make([]byte, b.dataPayloadOffset()-objectHeaderSize, b.dataPayloadOffset()-objectHeaderSize+len(stored))
	body = append(body, stored...)
	o := b.appendObject(ObjectData, objectFlags[storedAs], body)
	b.journal.Data = append(b.journal.Data, DataObject{
		Object: o, Payload: payload, StoredAs: storedAs,
		PayloadOffset: o.Offset + uint64(b.dataPayloadOffset()), // #nosec G115 -- 64 or 72
	})
	i := len(b.journal.Data) - 1
	b.dataIndex[string(payload)] = i
	return i
}

// compress returns payload as it is stored, and how: compressed with
// the journal's compression when it is at least the threshold and
// compression makes it smaller, else as it is (journal_file_append_data).
func (b *builder) compress(payload []byte) ([]byte, Compression) {
	if b.opts.Compression == None || len(payload) < b.opts.CompressThreshold {
		return payload, None
	}
	var stored []byte
	switch b.opts.Compression {
	case XZ:
		stored = b.compressXZ(payload)
	case LZ4:
		stored = b.compressLZ4(payload)
	case Zstd:
		stored = b.zstd.EncodeAll(payload, nil)
	}
	if stored == nil || len(stored) >= len(payload) {
		return payload, None
	}
	return stored, b.opts.Compression
}

// compressXZ returns payload as an xz stream without a check, the way
// systemd's compress_blob_xz writes it.
func (b *builder) compressXZ(payload []byte) []byte {
	var out bytes.Buffer
	w, err := xz.WriterConfig{NoCheckSum: true}.NewWriter(&out)
	if err != nil {
		b.tb.Fatalf("journalfile: xz writer: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		b.tb.Fatalf("journalfile: xz: %v", err)
	}
	if err := w.Close(); err != nil {
		b.tb.Fatalf("journalfile: xz: %v", err)
	}
	return out.Bytes()
}

// compressLZ4 returns payload as systemd stores an lz4 value: its size
// as a little-endian uint64, then one raw lz4 block. It returns nil when
// lz4 cannot make the payload smaller.
func (b *builder) compressLZ4(payload []byte) []byte {
	out := make([]byte, 8+lz4.CompressBlockBound(len(payload)))
	le.PutUint64(out, uint64(len(payload)))
	n, err := lz4.CompressBlock(payload, out[8:], nil)
	if err != nil {
		b.tb.Fatalf("journalfile: lz4: %v", err)
	}
	if n == 0 {
		return nil
	}
	return out[:8+n]
}

// itemSizes returns the size of an ENTRY item and of an ENTRY_ARRAY item:
// 16 and 8 bytes in a regular journal (an offset and a hash, an offset),
// 4 and 4 in a compact one (32-bit offsets, no hash).
func (b *builder) itemSizes() (entryItem, arrayItem int) {
	if b.opts.Compact {
		return 4, 4
	}
	return 16, 8
}

// appendEntry writes e's DATA objects that the journal lacks, its ENTRY
// object, and its item in the entry list.
func (b *builder) appendEntry(e Entry) {
	var items []int
	for _, payload := range e.Payloads {
		items = append(items, b.dataObject(payload))
	}
	// Data objects are appended in order, so sorting the indexes sorts
	// the items by offset; Compact drops an item listed twice.
	slices.Sort(items)
	items = slices.Compact(items)

	seqnum := e.Seqnum
	if seqnum == 0 {
		seqnum = b.seqnum + 1
	}
	b.seqnum = seqnum
	bootID := e.BootID
	if bootID == ([16]byte{}) {
		bootID = b.opts.BootID
	}
	entryItem, _ := b.itemSizes()
	body := make([]byte, entryItems-objectHeaderSize+len(items)*entryItem)
	le.PutUint64(body[entryOffsetSeqnum-objectHeaderSize:], seqnum)
	le.PutUint64(body[entryOffsetRealtime-objectHeaderSize:], e.Realtime)
	le.PutUint64(body[entryOffsetMonotonic-objectHeaderSize:], e.Monotonic)
	copy(body[entryOffsetBootID-objectHeaderSize:], bootID[:])
	for k, item := range items {
		at := entryItems - objectHeaderSize + k*entryItem
		b.putOffset(body[at:], b.journal.Data[item].Offset, entryItem)
	}
	o := b.appendObject(ObjectEntry, 0, body)
	b.journal.Entries = append(b.journal.Entries, EntryObject{Object: o, Seqnum: seqnum, Realtime: e.Realtime, Items: items})
	b.linkEntry(o.Offset)
}

// putOffset writes an item's offset: 32 bits in a compact journal, 64
// bits in a regular one (where the hash after it stays 0).
func (b *builder) putOffset(dst []byte, offset uint64, itemSize int) {
	if itemSize == 4 {
		le.PutUint32(dst, uint32(offset)) // #nosec G115 -- test journals are far below 4 GiB
		return
	}
	le.PutUint64(dst, offset)
}

// linkEntry writes the entry's offset into the last ENTRY_ARRAY object,
// appending a new array, linked from the last one or from the header,
// when the last is full.
func (b *builder) linkEntry(entryOffset uint64) {
	_, arrayItem := b.itemSizes()
	arrays := b.journal.Arrays
	if len(arrays) == 0 || arrays[len(arrays)-1].Count == arrays[len(arrays)-1].Capacity {
		b.appendArray(b.nextArrayCapacity())
	}
	last := &b.journal.Arrays[len(b.journal.Arrays)-1]
	at := last.Offset + entryArrayItems + uint64(last.Count*arrayItem) // #nosec G115 -- a count of items, never negative
	b.putOffset(b.buf[at:], entryOffset, arrayItem)
	last.Count++
}

// nextArrayCapacity is the capacity of the next ENTRY_ARRAY object:
// Options.ArrayCapacity, or journald's growth (link_entry_into_array):
// twice the last array's capacity, or twice the entries listed so far
// plus one when that is more, and at least 4.
func (b *builder) nextArrayCapacity() int {
	if b.opts.ArrayCapacity > 0 {
		return b.opts.ArrayCapacity
	}
	arrays := b.journal.Arrays
	if len(arrays) == 0 {
		return 4
	}
	listed := len(b.journal.Entries) - 1 // the entry being linked is not listed yet
	last := arrays[len(arrays)-1].Capacity
	if listed > last {
		return max((listed+1)*2, 4)
	}
	return max(last*2, 4)
}

// appendArray appends an empty ENTRY_ARRAY object of capacity slots and
// links it from the previous array, or from the header for the first.
func (b *builder) appendArray(capacity int) {
	_, arrayItem := b.itemSizes()
	body := make([]byte, entryArrayItems-objectHeaderSize+capacity*arrayItem)
	o := b.appendObject(ObjectEntryArray, 0, body)
	if n := len(b.journal.Arrays); n > 0 {
		le.PutUint64(b.buf[b.journal.Arrays[n-1].Offset+OffsetNextEntryArray:], o.Offset)
	} else {
		le.PutUint64(b.buf[offsetEntryArrayOffset:], o.Offset)
	}
	b.journal.Arrays = append(b.journal.Arrays, ArrayObject{
		Object: o, FirstEntry: len(b.journal.Entries) - 1, Capacity: capacity,
	})
}

// writeHeader fills the header from what was written, with the fields
// journalctl checks when it opens a file consistent with each other
// (journal_file_verify_header).
func (b *builder) writeHeader() {
	// The file ends on an 8-byte boundary, as the next object would start.
	for len(b.buf)%8 != 0 {
		b.buf = append(b.buf, 0)
	}
	h := b.buf[:HeaderSize]
	copy(h, "LPKSHHRH")
	le.PutUint32(h[offsetCompatibleFlags:], compatibleTailEntryBootID)
	incompatible := headerFlags[b.opts.Compression]
	if b.opts.Compact {
		incompatible |= incompatibleCompact
	}
	le.PutUint32(h[offsetIncompatibleFlags:], incompatible)
	h[offsetState] = b.opts.State
	copy(h[offsetFileID:], b.opts.FileID[:])
	copy(h[offsetMachineID:], b.opts.MachineID[:])
	copy(h[offsetSeqnumID:], b.opts.SeqnumID[:])
	le.PutUint64(h[offsetHeaderSize:], HeaderSize)
	le.PutUint64(h[offsetArenaSize:], uint64(len(b.buf)-HeaderSize)) // #nosec G115 -- the buffer starts with the header
	le.PutUint64(h[offsetTailObjectOffset:], b.tail)
	le.PutUint64(h[offsetNObjects:], b.nObjects)
	le.PutUint64(h[OffsetNEntries:], uint64(len(b.journal.Entries)))
	le.PutUint64(h[offsetNData:], uint64(len(b.journal.Data)))
	le.PutUint64(h[offsetNEntryArrays:], uint64(len(b.journal.Arrays)))
	entries := b.journal.Entries
	if len(entries) == 0 {
		return
	}
	first, last := entries[0], entries[len(entries)-1]
	le.PutUint64(h[offsetHeadEntrySeqnum:], first.Seqnum)
	le.PutUint64(h[offsetTailEntrySeqnum:], last.Seqnum)
	le.PutUint64(h[offsetHeadEntryRealtime:], first.Realtime)
	le.PutUint64(h[offsetTailEntryRealtime:], last.Realtime)
	le.PutUint64(h[offsetTailEntryMonotonic:], le.Uint64(b.buf[last.Offset+entryOffsetMonotonic:]))
	copy(h[offsetTailEntryBootID:], b.buf[last.Offset+entryOffsetBootID:last.Offset+entryOffsetBootID+16])
	tailArray := b.journal.Arrays[len(b.journal.Arrays)-1]
	le.PutUint32(h[offsetTailEntryArrayOffset:], uint32(tailArray.Offset)) // #nosec G115 -- test journals are far below 4 GiB
	le.PutUint32(h[offsetTailEntryArrayN:], uint32(tailArray.Count))       // #nosec G115 -- a test writes few entries
	le.PutUint64(h[offsetTailEntryOffset:], last.Offset)
}
