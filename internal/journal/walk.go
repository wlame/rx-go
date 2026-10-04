package journal

// This file walks a journal's entry list and reads its entries. The list
// is a chain of ENTRY_ARRAY objects that starts at the header's
// entry_array_offset; each array holds the offsets of entries in the
// order of their sequence numbers, the order `journalctl --file` prints
// one file in.
//
// The walk takes the header's n_entries entries, once: entries a live
// file lists after its header was read belong to a later read.
//
// INVARIANT: the walk ends, and each entry is read at most once. Each
// array links to an array after it: an array whose link goes back or
// stays is damaged and ends the walk, so a list that points back to
// itself cannot loop. Each item is taken once. An entry is read only when
// its offset is after the last entry read (the last offset that held an
// ENTRY object); an item that goes back or stays leaves out that one
// entry, and the walk goes on with the next item.

import (
	"bytes"
	"errors"
	"io"
	"math"

	"github.com/klauspost/compress/zstd"
)

// errListDone ends a walk that took every entry the header counts.
var errListDone = errors.New("the walk took every entry the header counts")

// walker walks the entry list of one journal and reads its entries. It
// is used by one goroutine.
type walker struct {
	f     *journalFile
	cache *dataCache

	// total is the number of entries the header counts, taken the number
	// handed out so far.
	total, taken int64

	// The place in the list: the array whose items are being handed out
	// and the offset of the array after it (0: none).
	array     itemCursor
	nextArray uint64
	// lastRead is the offset of the last entry read: the last item whose
	// offset held an ENTRY object that passed its checks. A wild item (one
	// beyond the file, or at another object) does not move it, so it
	// costs only its own entry.
	lastRead uint64
	// hasList is false when the header names no entry list.
	hasList bool

	// Buffers reused for every object: the items of the current array and
	// of the current entry, and the first read of a DATA object.
	arrayChunk, entryChunk, dataProbe []byte

	// fields holds the fields of the entry being read; it is reused, so
	// an Entry from readEntry is valid until the next call.
	fields []Field

	// zstdNames decodes the names of zstd values (namePrefixReader),
	// made at the first one; nameBuf receives a name.
	zstdNames *zstd.Decoder
	nameBuf   [namePrefixBytes]byte
	// decoded counts the bytes the decoders gave the walk: names and
	// whole values. Tests read it.
	decoded int64
}

// newWalker returns a walker at the start of the entry list of the
// journal r, of size bytes, whose header is h.
func newWalker(r io.ReaderAt, h Header, size uint64, cacheBytes int64) *walker {
	return &walker{
		f: &journalFile{
			r: r, size: size, end: h.readEnd(size),
			headerSize: h.HeaderSize, compact: h.Compact,
		},
		cache: newDataCache(cacheBytes),
		// A header may count more entries than an int64 holds; no file
		// lists that many, so the count is clamped where a Report can
		// hold it.
		total:      int64(min(h.NEntries, math.MaxInt64)), // #nosec G115 -- clamped to MaxInt64
		nextArray:  h.EntryArrayOffset,
		hasList:    h.EntryArrayOffset != 0,
		arrayChunk: make([]byte, itemChunkBytes),
		entryChunk: make([]byte, itemChunkBytes),
		dataProbe:  make([]byte, dataProbeBytes),
		fields:     make([]Field, 0, shortFieldCount),
	}
}

// itemWidths returns the size of an ENTRY item and of an ENTRY_ARRAY item:
// 16 and 8 bytes in a regular journal (an offset and a hash, an offset),
// 4 and 4 in a compact one (32-bit offsets).
func (w *walker) itemWidths() (entryItem, arrayItem uint64) {
	if w.f.compact {
		return 4, 4
	}
	return 16, 8
}

// next takes the next item of the list and returns the entry offset it
// holds; the item counts as taken whatever readEntry finds there. It
// returns errListDone after the last entry the header counts, damage
// when the list cannot go on (the walk ends there), and the reader's
// error when a read fails.
func (w *walker) next() (uint64, error) {
	if w.taken == w.total {
		return 0, errListDone
	}
	if w.array.left == 0 {
		if err := w.openNextArray(); err != nil {
			return 0, err
		}
	}
	offset, err := w.array.nextItem()
	switch {
	case err != nil:
		return 0, err
	case offset == 0:
		return 0, w.listEnds()
	}
	w.taken++
	return offset, nil
}

// listEnds is the damage of a list that ends before the entries the
// header counts.
func (w *walker) listEnds() error {
	if !w.hasList {
		return damaged("the header counts %d entries and names no entry list", w.total)
	}
	return damaged("the entry list ends after %d of the %d entries the header counts", w.taken, w.total)
}

// openNextArray reads the next ENTRY_ARRAY object of the list and makes
// its items the next ones handed out. As systemd does (check_object,
// journal-file.c:1044-1063), the array is checked whole first, its link
// to the next array included: an array that links back is damaged, and
// its items are not used.
func (w *walker) openNextArray() error {
	offset := w.nextArray
	if offset == 0 {
		return w.listEnds()
	}
	_, width := w.itemWidths()
	o, err := w.f.readObject(offset, objectEntryArray, entryArrayItemsOffset, w.arrayChunk[:arrayProbeBytes])
	if err != nil {
		return err
	}
	what := objectRef{objectEntryArray, offset}
	if err := checkItems(o, entryArrayItemsOffset, width, what); err != nil {
		return err
	}
	next := le.Uint64(o.head[offsetNextEntryArray:])
	switch {
	case next%8 != 0:
		return damaged("%s links to offset %d, not a multiple of 8", what, next)
	case next != 0 && next <= offset:
		return damaged("%s links to offset %d, not after itself", what, next)
	}
	w.nextArray = next
	w.array.start(w.f, o, objectEntryArray, entryArrayItemsOffset, width, w.arrayChunk)
	return nil
}

// checkItems checks that the object o, whose items of width bytes start
// at itemsOffset, holds a whole number of them, and at least one.
func checkItems(o object, itemsOffset, width uint64, what objectRef) error {
	switch items := o.size - itemsOffset; {
	case items%width != 0:
		return damaged("%s has size %d, which does not hold whole %d-byte items", what, o.size, width)
	case items == 0:
		return damaged("%s holds no item", what)
	}
	return nil
}

// readEntry reads the ENTRY object at offset and the DATA objects it
// lists, and returns the entry the renderer needs. The offset must come
// after the last entry read; an item that goes back or stays is damage,
// and only its own entry is left out. The ENTRY object is checked as
// systemd's check_object checks it (journal-file.c:983-1024), and every
// DATA object is read and checked: a failure of any of them is damage,
// and the whole entry is left out, since the damaged item could be its
// MESSAGE.
//
// The last entry read moves to offset once the ENTRY object passes its
// own checks, before its DATA objects are read: an entry that is real
// but holds a damaged value is then not read again by a later item that
// points to it.
//
// The entry's Fields hold, for each field the renderer reads, the last
// item of that name in the entry's stored order: the renderer reads no
// other item, and a later item of the same name replaces an earlier one
// (logs-show.c:145), so the text is the same as with every item, and an
// entry of many large values holds at most one value per field. A
// payload without '=' names no field and is skipped. The returned Entry
// is valid until the next call.
func (w *walker) readEntry(offset uint64) (Entry, error) {
	if offset <= w.lastRead {
		return Entry{}, damaged("the entry list points to offset %d, not after the last entry read at offset %d", offset, w.lastRead)
	}
	width, _ := w.itemWidths()
	o, err := w.f.readObject(offset, objectEntry, entryItemsOffset, w.entryChunk[:entryProbeBytes])
	if err != nil {
		return Entry{}, err
	}
	what := objectRef{objectEntry, offset}
	if err = checkItems(o, entryItemsOffset, width, what); err != nil {
		return Entry{}, err
	}
	realtime, err := checkEntryHeader(o, what)
	if err != nil {
		return Entry{}, err
	}
	w.lastRead = offset
	var items itemCursor
	items.start(w.f, o, objectEntry, entryItemsOffset, width, w.entryChunk)
	var picked pickedFields
	for items.left > 0 {
		dataOffset, err := items.nextItem()
		if err != nil {
			return Entry{}, err
		}
		field, value, rendered, err := w.dataField(dataOffset)
		if err != nil {
			return Entry{}, err
		}
		if rendered {
			picked.value[field], picked.present[field] = value, true
		}
	}
	w.fields = w.fields[:0]
	for field, present := range picked.present {
		if present {
			w.fields = append(w.fields, Field{Name: shortFieldNames[field], Value: picked.value[field]})
		}
	}
	return Entry{Realtime: realtime, Fields: w.fields}, nil
}

// checkEntryHeader checks the fixed fields of an ENTRY object as
// check_object does (journal-file.c:1001-1022) and returns its realtime:
// a sequence number above 0, a realtime above 0 and below 2^55, a
// monotonic time below 2^55 and a boot id that is not all zero.
// journalctl leaves out an entry that fails one of them.
func checkEntryHeader(o object, what objectRef) (uint64, error) {
	seqnum := le.Uint64(o.head[offsetEntrySeqnum:])
	realtime := le.Uint64(o.head[offsetEntryRealtime:])
	monotonic := le.Uint64(o.head[offsetEntryMonotonic:])
	switch {
	case seqnum == 0:
		return 0, damaged("%s has the sequence number 0", what)
	case !validRealtime(realtime):
		return 0, damaged("%s has the invalid realtime %d", what, realtime)
	case monotonic >= maxValidTime:
		return 0, damaged("%s has the invalid monotonic time %d", what, monotonic)
	case bytes.Count(o.head[offsetEntryBootID:offsetEntryBootID+16], []byte{0}) == 16:
		return 0, damaged("%s has an empty boot id", what)
	}
	return realtime, nil
}

// renderedField returns the field the renderer reads under name, if any.
// Comparing string(name) with a string does not copy name.
func renderedField(name []byte) (shortField, bool) {
	for field, fieldName := range shortFieldNames {
		if string(name) == fieldName {
			return shortField(field), true
		}
	}
	return 0, false
}

// dataField returns what the DATA object at offset gives the renderer:
// for a field it reads, the field and its value (rendered is true); for
// any other field, nothing; for a damaged object, its damage. The answer
// comes from the cache, or from readData and is then cached, damage
// included, so that an object is read once however many entries list it.
// An I/O error is not an answer and is not cached.
func (w *walker) dataField(offset uint64) (field shortField, value []byte, rendered bool, err error) {
	answer, cached := w.cache.get(offset)
	if !cached {
		payload, err := w.readData(offset)
		reason, isDamage := damageReason(err)
		switch {
		case isDamage:
			answer = dataAnswer{damage: reason}
		case err != nil:
			return 0, nil, false, err
		case payload == nil:
			answer = dataAnswer{unread: true}
		default:
			answer = dataAnswer{payload: payload}
		}
		w.cache.put(offset, answer)
	}
	switch {
	case answer.damage != "":
		return 0, nil, false, &damageError{reason: answer.damage}
	case answer.unread:
		return 0, nil, false, nil
	}
	// readData returns a payload only when its name, before '=', is one
	// the renderer reads.
	name, value, _ := bytes.Cut(answer.payload, []byte("="))
	field, _ = renderedField(name)
	return field, value, true, nil
}

// close releases the walker's zstd decoder.
func (w *walker) close() {
	if w.zstdNames != nil {
		w.zstdNames.Close()
		w.zstdNames = nil
	}
}
