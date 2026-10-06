package journal

// Tests of the memory a crafted journal can make the reader use: the
// shown values of one entry together, the values of the entry read
// before, and tiny values that declare far more text than they hold.
// The crafted values are written into DATA objects of journals the test
// writer made, in place of plain placeholders of the same length.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"runtime"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/journalfile"
)

const (
	mib = 1 << 20
	// shownValuesBudget is what the payloads of the shown fields of one
	// entry may hold together.
	shownValuesBudget = 128 * mib
	// nameWindow is what the decoder of zstd names reserves, once per
	// reader, for a single-segment frame of 128 MiB: its window, which is
	// the frame's content size, and 1 MiB (klauspost's low-memory mode).
	nameWindow = 128*mib + mib
	// budgetReason is the reason of an entry whose shown values pass the
	// budget.
	budgetReason = "holds shown values of more than 128 MiB together"
)

// zstdRLEBlock is the block type of a zstd RLE block: one byte repeated.
const zstdRLEBlock = 1

// zstdRLE returns an RLE block of n copies of b: a 3-byte header and the
// one byte.
func zstdRLE(last bool, n int, b byte) []byte {
	header := uint32(n)<<3 | zstdRLEBlock<<1 // #nosec G115 -- a block size, at most 128 KiB
	if last {
		header |= 1
	}
	return []byte{byte(header), byte(header >> 8), byte(header >> 16), b}
}

// zstdFrameDeclaring returns a zstd frame whose header declares size
// bytes of text in an 8-byte content-size field, then blocks as they
// are. A single-segment frame names no window: its window is its content
// size. The other frame names a 128 KiB window.
func zstdFrameDeclaring(size uint64, singleSegment bool, blocks ...[]byte) []byte {
	frame := []byte{0x28, 0xB5, 0x2F, 0xFD}
	if singleSegment {
		frame = append(frame, 0xE0) // an 8-byte content size, single segment
	} else {
		frame = append(frame, 0xC0, 0x38) // an 8-byte content size; a 2^(10+7)-byte window
	}
	frame = binary.LittleEndian.AppendUint64(frame, size)
	for _, b := range blocks {
		frame = append(frame, b...)
	}
	return frame
}

// zstdZerosValue returns the densest zstd frame there is of a payload
// of size bytes, name and '=' followed by zero bytes: the name in a raw
// block, the zeros in RLE blocks of 128 KiB, four stored bytes each.
func zstdZerosValue(name string, size int) []byte {
	head := []byte(name + "=")
	rest := size - len(head)
	blocks := [][]byte{zstdBlock(rest == 0, zstdRawBlock, head)}
	for rest > 0 {
		n := min(rest, 128<<10)
		rest -= n
		blocks = append(blocks, zstdRLE(rest == 0, n, 0))
	}
	return zstdFrameDeclaring(uint64(size), true, blocks...) // #nosec G115 -- a positive size
}

// zstdBogusValue returns a frame that declares declared bytes of text
// and holds the text head (at least 65 bytes, so that a name search
// stops inside it) in a raw block, then one byte in a last RLE block.
func zstdBogusValue(head []byte, declared uint64, singleSegment bool) []byte {
	return zstdFrameDeclaring(declared, singleSegment, zstdBlock(false, zstdRawBlock, head), zstdRLE(true, 1, 'z'))
}

// lz4LiteralsValue returns systemd's lz4 framing of a value that
// declares declared bytes of text and holds only literals, fewer than
// 15, in one sequence.
func lz4LiteralsValue(declared uint64, literals []byte) []byte {
	stored := binary.LittleEndian.AppendUint64(nil, declared)
	stored = append(stored, byte(len(literals))<<4) // #nosec G115 -- fewer than 15 literals
	return append(stored, literals...)
}

// xzWithDictionary returns a copy of the xz stream stored whose first
// block header declares the dictionary code (the LZMA2 property byte:
// code 24 is 16 MiB, 25 is 24 MiB, 30 is 128 MiB), with the header's
// CRC32 written again. The block's data is left as it is: a dictionary
// at least as large as the one it was written with decodes it the same.
// The header must carry no size fields, as the test writer's do not.
func xzWithDictionary(tb testing.TB, stored []byte, code byte) []byte {
	tb.Helper()
	out := bytes.Clone(stored)
	const blockHeader = 12 // after the stream header
	headerLen := (int(out[blockHeader]) + 1) * 4
	header := out[blockHeader : blockHeader+headerLen]
	// The block flags, then one filter: its id (0x21, LZMA2), the size of
	// its properties (1) and the property byte.
	if header[1]&0xC0 != 0 || header[2] != 0x21 || header[3] != 1 {
		tb.Fatalf("unexpected xz block header % x", header)
	}
	header[4] = code
	binary.LittleEndian.PutUint32(header[headerLen-4:], crc32.ChecksumIEEE(header[:headerLen-4]))
	return out
}

// craftedValue is the stored bytes of one DATA object and the
// compression flags it is stored with (0: plain).
type craftedValue struct {
	flags  byte
	stored []byte
}

// writeCrafted writes a journal whose DATA object k holds values[k] and
// whose entry i lists the objects entries[i], in that order, which must
// be the order the objects are first listed in (so that it is also
// their order in the file). Each value is written as a plain
// placeholder of its stored length, which is then replaced by its bytes
// and flags; it returns the journal as written and the changed bytes.
func writeCrafted(tb testing.TB, values []craftedValue, entries [][]int) (journalfile.Journal, []byte) {
	tb.Helper()
	placeholders := make([][]byte, len(values))
	for k, v := range values {
		p := fmt.Appendf(nil, "P%d=", k)
		if len(p) > len(v.stored) {
			tb.Fatalf("value %d of %d bytes is shorter than its placeholder", k, len(v.stored))
		}
		placeholders[k] = append(p, bytes.Repeat([]byte("p"), len(v.stored)-len(p))...)
	}
	written := make([]journalfile.Entry, len(entries))
	for i, items := range entries {
		written[i].Realtime = caseRealtime + uint64(i) // #nosec G115 -- a small loop index
		for _, k := range items {
			written[i].Payloads = append(written[i].Payloads, placeholders[k])
		}
	}
	j := journalfile.Write(tb, written, journalfile.Options{})
	data := bytes.Clone(j.Bytes)
	for k, v := range values {
		d := j.Data[dataOf(tb, j, string(placeholders[k]))]
		copy(data[d.PayloadOffset:d.End()], v.stored)
		data[d.Offset+1] = v.flags
	}
	return j, data
}

// Two entries list the same seven DATA objects, one per field the text
// shows, each a 4 KiB zstd frame of a 128 MiB payload. Each value is
// within the bound of one value, but the seven together are 896 MiB.
// The first value of each entry fills the budget, and the second is
// refused before it is decoded; the second entry decodes its first value
// again, as no value above the cache's bound is cached. What the read
// allocates is the name decoder's window and two values.
func TestTextReader_LeavesOutEntriesWhoseShownValuesPass128MiBTogether(t *testing.T) {
	values := make([]craftedValue, 0, len(shortFieldNames))
	items := make([]int, 0, len(shortFieldNames))
	for k, name := range shortFieldNames {
		values = append(values, craftedValue{objectCompressedZstd, zstdZerosValue(name, maxValue)})
		items = append(items, k)
	}
	_, data := writeCrafted(t, values, [][]int{items, items})
	var report Report
	allocated := allocatedBytes(func() { _, report = readBytes(t, data, Options{}) })
	want := Report{Entries: 2, LeftOut: 2, FirstDamaged: 1}
	if want.DamageReason = report.DamageReason; report != want || !bytes.Contains([]byte(report.DamageReason), []byte(budgetReason)) {
		t.Errorf("Report = %+v, want %+v with a reason holding %q", report, want, budgetReason)
	}
	if limit := uint64(nameWindow + 2*maxValue + 16*mib); allocated > limit {
		t.Errorf("the read allocated %d MiB, want at most %d MiB", allocated/mib, limit/mib)
	}
	t.Logf("%d-byte journal: %d MiB allocated", len(data), allocated/mib)
}

// The budget counts the payload of every shown item an entry lists,
// "FIELD=value", once per item. Entry 1 holds exactly 128 MiB and is
// shown; entry 2 one byte more, and its MESSAGE is refused before it is
// decoded; entry 3 lists one 70 MiB MESSAGE twice. The answer is the same
// with the cache and without it.
func TestTextReader_ShowsAnEntryOfExactly128MiBOfShownValuesAndLeavesOutOneByteMore(t *testing.T) {
	small := [][]byte{
		journalfile.Payload("_HOSTNAME", []byte("host")),
		journalfile.Payload("SYSLOG_IDENTIFIER", []byte("app")),
		journalfile.Payload("_PID", []byte("42")),
	}
	smallBytes := 0
	values := make([]craftedValue, 0, len(small)+4)
	for _, p := range small {
		smallBytes += len(p)
		values = append(values, craftedValue{0, p})
	}
	const repeated = 70 * mib
	values = append(values,
		craftedValue{objectCompressedZstd, zstdZerosValue("MESSAGE", shownValuesBudget-smallBytes)},
		craftedValue{objectCompressedZstd, zstdZerosValue("MESSAGE", shownValuesBudget-smallBytes+1)},
		craftedValue{objectCompressedZstd, zstdZerosValue("MESSAGE", repeated)},
		// A placeholder of the same length, so that the entry lists two
		// objects; its item is then made to list the one before it.
		craftedValue{objectCompressedZstd, zstdZerosValue("MESSAGE", repeated)},
	)
	j, data := writeCrafted(t, values, [][]int{{0, 1, 2, 3}, {0, 1, 2, 4}, {0, 1, 2, 5, 6}})
	entryItem, _ := itemSizes(false)
	third := j.Entries[2]
	putItem(data, third.Offset+entryItemsOffset+4*entryItem, j.Data[third.Items[3]].Offset, false)

	for _, opts := range []Options{{}, {CacheBytes: -1}} {
		t.Run(fmt.Sprintf("cache=%d", opts.CacheBytes), func(t *testing.T) {
			text, report := readBytes(t, data, opts)
			want := Report{Entries: 3, Shown: 1, LeftOut: 2, FirstDamaged: 2}
			if want.DamageReason = report.DamageReason; report != want || !bytes.Contains([]byte(report.DamageReason), []byte(budgetReason)) {
				t.Errorf("Report = %+v, want %+v with a reason holding %q", report, want, budgetReason)
			}
			if bytes.Count(text, []byte("\n")) != 1 || !bytes.HasPrefix(text, []byte(caseStamp+" host app[42]: [")) {
				t.Errorf("text = %.200q, want one line of the first entry", text)
			}
		})
	}
}

// heapAtRead is an io.ReaderAt that, at the first read at offset at,
// collects garbage and records the bytes of the heap still live
// (HeapAlloc after a collection). Two collections run, so that what a
// sync.Pool held through the first is gone too.
type heapAtRead struct {
	r    io.ReaderAt
	at   int64
	live uint64
	seen bool
}

func (h *heapAtRead) ReadAt(p []byte, off int64) (int, error) {
	if off == h.at && !h.seen {
		h.seen = true
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		h.live = ms.HeapAlloc
	}
	return h.r.ReadAt(p, off)
}

// twoEntriesOfSevenValues writes two entries that each list a value of
// value bytes for every field the text shows, all distinct, stored with
// zstd. It returns the journal's bytes and the offset of the second
// entry's last DATA object. It returns nothing else on purpose: the
// written journal also holds every payload before compression.
func twoEntriesOfSevenValues(tb testing.TB, value int) ([]byte, int64) {
	tb.Helper()
	entries := make([]journalfile.Entry, 2)
	for i := range entries {
		entries[i].Realtime = caseRealtime + uint64(i) // #nosec G115 -- a small loop index
		for _, name := range shortFieldNames {
			entries[i].Payloads = append(entries[i].Payloads, journalfile.Payload(name, append(fmt.Appendf(nil, "%d", i), make([]byte, value)...)))
		}
	}
	j := journalfile.Write(tb, entries, journalfile.Options{Compression: journalfile.Zstd})
	second := j.Entries[1]
	return j.Bytes, int64(j.Data[second.Items[len(second.Items)-1]].Offset) // #nosec G115 -- a small offset
}

// Two entries list seven shown values of 8 MiB each, all distinct, and
// the cache is off. When the reader comes to the last value of the
// second entry, the heap holds the six values of that entry read so far
// and the name decoder's window, and none of the first entry's values.
func TestTextReader_LetsGoOfThePreviousEntrysValuesBeforeReadingTheNext(t *testing.T) {
	const value = 8 * mib
	data, last := twoEntriesOfSevenValues(t, value)
	r := &heapAtRead{r: bytes.NewReader(data), at: last}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	tr, err := NewTextReader(r, int64(len(data)), Options{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if _, err := io.Copy(io.Discard, tr); err != nil {
		t.Fatal(err)
	}
	if report := tr.Report(); report.Shown != 2 || !r.seen {
		t.Fatalf("Report = %+v, the last value read: %v", report, r.seen)
	}
	live := int64(r.live) - int64(before.HeapAlloc) // #nosec G115 -- heap sizes
	// Six values and a window of one value make seven; the first entry's
	// seven values would make fourteen.
	if limit := int64(9 * value); live > limit {
		t.Errorf("%d MiB live while the last value of the second entry is read, want at most %d MiB", live/mib, limit/mib)
	}
	t.Logf("%d MiB live while the last value of the second entry is read", live/mib)
}

// Fifty entries each list a crafted value of their own, of a few dozen
// stored bytes that declare 128 MiB, and a short message. The entries are
// left out, and what the read allocates does not grow with the number of
// values: a zstd frame or an lz4 value that declares more text than its
// stored bytes can hold is refused before its buffer is made, and an xz
// stream that declares a dictionary above 16 MiB before the dictionary
// is made, whether its field is shown or not. A single-segment zstd
// frame's window is its content size, so the name decoder reserves that
// window once for the reader.
func TestTextReader_LeavesOutTinyValuesThatDeclare128MiBWithoutAllocatingIt(t *testing.T) {
	const entries = 50
	head := func(field string, i int) []byte {
		h := fmt.Appendf(nil, "%s=%d|", field, i)
		return append(h, bytes.Repeat([]byte("h"), namePrefixBytes+5-len(h))...)
	}
	xzValue := func(field string, i int) []byte {
		return xzWithDictionary(t, journalfile.Stored(t, journalfile.XZ, fmt.Appendf(nil, "%s=%d", field, i)), 30)
	}
	rows := []struct {
		name      string
		flags     byte
		value     func(i int) []byte
		reason    string
		allowance uint64
	}{
		{"zstd single segment", objectCompressedZstd, func(i int) []byte { return zstdBogusValue(head("MESSAGE", i), maxValue, true) },
			"stored bytes cannot hold (zstd)", nameWindow + 16*mib},
		{"zstd with a window", objectCompressedZstd, func(i int) []byte { return zstdBogusValue(head("MESSAGE", i), maxValue, false) },
			"stored bytes cannot hold (zstd)", 16 * mib},
		{"lz4", objectCompressedLZ4, func(i int) []byte { return lz4LiteralsValue(maxValue, fmt.Appendf(nil, "MESSAGE=%d", i)) },
			"stored bytes cannot hold (lz4)", 16 * mib},
		{"xz", objectCompressedXZ, func(i int) []byte { return xzValue("MESSAGE", i) },
			"declares an xz dictionary of more than 16 MiB", 16 * mib},
		{"xz in a field the text does not show", objectCompressedXZ, func(i int) []byte { return xzValue("COREDUMP", i) },
			"declares an xz dictionary of more than 16 MiB", 16 * mib},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			values := make([]craftedValue, 0, 2*entries)
			lists := make([][]int, entries)
			for i := range entries {
				values = append(values,
					craftedValue{row.flags, row.value(i)},
					craftedValue{0, fmt.Appendf(nil, "MESSAGE=crafted %d", i)})
				lists[i] = []int{2 * i, 2*i + 1}
			}
			_, data := writeCrafted(t, values, lists)
			var report Report
			allocated := allocatedBytes(func() { _, report = readBytes(t, data, Options{}) })
			want := Report{Entries: entries, LeftOut: entries, FirstDamaged: 1}
			if want.DamageReason = report.DamageReason; report != want || !bytes.Contains([]byte(report.DamageReason), []byte(row.reason)) {
				t.Errorf("Report = %+v, want %+v with a reason holding %q", report, want, row.reason)
			}
			if allocated > row.allowance {
				t.Errorf("the read allocated %d MiB, want at most %d MiB", allocated/mib, row.allowance/mib)
			}
			t.Logf("%d-byte journal: %d MiB allocated", len(data), allocated/mib)
		})
	}
}
