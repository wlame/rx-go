package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/journalfile"
)

// objectReads counts the ReadAt calls each object of a written journal
// gets, by the object the call starts in, and the bytes read in all.
// ReadAt may be called from several goroutines, so a mutex guards the
// counts.
type objectReads struct {
	r      io.ReaderAt
	starts []uint64 // object starts, sorted; 0 is the header
	mu     sync.Mutex
	calls  map[uint64]int
	// objectBytes are the bytes of the reads that start in each object.
	objectBytes map[uint64]int64
	bytes       int64
}

// newObjectReads wraps r, the bytes of j, to count its reads by object.
func newObjectReads(r io.ReaderAt, j journalfile.Journal) *objectReads {
	starts := []uint64{0}
	for _, d := range j.Data {
		starts = append(starts, d.Offset)
	}
	for _, e := range j.Entries {
		starts = append(starts, e.Offset)
	}
	for _, a := range j.Arrays {
		starts = append(starts, a.Offset)
	}
	slices.Sort(starts)
	return &objectReads{r: r, starts: starts, calls: map[uint64]int{}, objectBytes: map[uint64]int64{}}
}

func (o *objectReads) ReadAt(p []byte, off int64) (int, error) {
	o.mu.Lock()
	// The object the read starts in: the last start at or before off.
	i := sort.Search(len(o.starts), func(i int) bool { return o.starts[i] > uint64(off) }) - 1 // #nosec G115 -- off is never negative here
	o.calls[o.starts[i]]++
	o.objectBytes[o.starts[i]] += int64(len(p))
	o.bytes += int64(len(p))
	o.mu.Unlock()
	return o.r.ReadAt(p, off)
}

// testWords are the words randomWords picks from.
var testWords = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
	"india", "juliet", "kilo", "lima", "mike", "november", "oscar", "papa",
	"quebec", "romeo", "sierra", "tango", "uniform", "victor", "whiskey", "xray",
	"yankee", "zulu", "one", "two", "three", "four", "five", "six",
}

// randomWords returns n bytes of words picked by a fixed sequence: text
// that zstd and xz store in about a quarter of its size (8 KiB in about
// 2 KiB, more than the probe; 1 MiB in about 220 KiB, more than the name
// search reads).
func randomWords(n int, seed uint32) []byte {
	out := make([]byte, 0, n+16)
	for len(out) < n {
		seed = seed*1_664_525 + 1_013_904_223
		out = append(out, testWords[seed>>27]...)
		out = append(out, ' ')
	}
	return out[:n]
}

func TestTextReader_ReadsADataObjectInTwoReadsAndALargeShownValueInThree(t *testing.T) {
	const entries = 40
	for _, compression := range []journalfile.Compression{journalfile.Zstd, journalfile.XZ} {
		t.Run(string(compression), func(t *testing.T) {
			var written []journalfile.Entry
			for i := range entries {
				seed := uint32(i) * 2 // #nosec G115 -- a small loop index
				written = append(written, journalfile.Entry{
					Realtime: caseRealtime + uint64(i), // #nosec G115 -- a small loop index
					Payloads: [][]byte{
						journalfile.Payload("_HOSTNAME", []byte("reads-host")),
						journalfile.Payload("MESSAGE", randomWords(8<<10, seed)),
						journalfile.Payload("BLOB", randomWords(8<<10, seed+1)),
					},
				})
			}
			// One shown value whose stored bytes pass the 129 KiB the name
			// search reads: it is read in three.
			written = append(written, journalfile.Entry{
				Realtime: caseRealtime + entries,
				Payloads: [][]byte{journalfile.Payload("MESSAGE", randomWords(1<<20, 99))},
			})
			j := journalfile.Write(t, written, journalfile.Options{Compression: compression, Compact: true})
			large := j.Data[len(j.Data)-1]
			if large.StoredAs != compression || large.Size <= nameStoredLimit {
				t.Fatalf("the large value is stored as %s in %d bytes", large.StoredAs, large.Size)
			}
			reads := newObjectReads(bytes.NewReader(j.Bytes), j)
			w := &compareWriter{want: expectedTextOf(j)}
			streamText(t, reads, int64(len(j.Bytes)), Options{}, w)
			w.check(t)

			for _, d := range j.Data {
				limit := 2
				if d.Offset == large.Offset {
					limit = 3
				}
				if got := reads.calls[d.Offset]; got < 1 || got > limit {
					t.Errorf("DATA object at %d (%d bytes, %s) read in %d ReadAt calls, want 1 to %d", d.Offset, d.Size, d.StoredAs, got, limit)
				}
				// The reads of an object read each of its bytes once, and
				// the probe may run past its end: a shown value whose stored
				// bytes pass the name search's is completed with its rest,
				// not read again from its start.
				if got, limit := reads.objectBytes[d.Offset], int64(d.Size)+dataProbeBytes; got > limit { // #nosec G115 -- a small size
					t.Errorf("the reads of the DATA object at %d (%d bytes, %s) took %d bytes, want at most %d", d.Offset, d.Size, d.StoredAs, got, limit)
				}
			}
			for _, e := range j.Entries {
				if got := reads.calls[e.Offset]; got != 1 {
					t.Errorf("ENTRY object at %d read in %d ReadAt calls, want 1", e.Offset, got)
				}
			}
			// Every object is read once; a read overruns its object by at
			// most the probe.
			objects := int64(len(j.Data) + len(j.Entries) + len(j.Arrays))
			if limit := int64(len(j.Bytes)) + objects*arrayProbeBytes; reads.bytes > limit {
				t.Errorf("read %d bytes of a %d-byte journal, want at most %d", reads.bytes, len(j.Bytes), limit)
			}
			t.Logf("%d objects, %d-byte journal: %d bytes read", objects, len(j.Bytes), reads.bytes)
		})
	}
}

// zstdFrame returns a zstd frame without a content size or a checksum,
// with a 1 KiB window, holding the given blocks as they are.
func zstdFrame(blocks ...[]byte) []byte {
	frame := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00}
	for _, b := range blocks {
		frame = append(frame, b...)
	}
	return frame
}

// Block types of a zstd block header.
const (
	zstdRawBlock        = 0
	zstdCompressedBlock = 2
	zstdReservedBlock   = 3
)

// zstdBlock returns a block: a 3-byte header (last flag, type, size) and
// its content. A raw block's size is its content's; a compressed block's
// content is taken as it is.
func zstdBlock(last bool, blockType int, content []byte) []byte {
	header := uint32(len(content))<<3 | uint32(blockType)<<1 // #nosec G115 -- a small length
	if last {
		header |= 1
	}
	return append([]byte{byte(header), byte(header >> 8), byte(header >> 16)}, content...)
}

// emptyBlocks returns n empty raw blocks that are not the last.
func emptyBlocks(n int) [][]byte {
	blocks := make([][]byte, n)
	for i := range blocks {
		blocks[i] = zstdBlock(false, zstdRawBlock, nil)
	}
	return blocks
}

// craftedJournal writes n entries that each list one value stored as
// stored, with the DATA object flags flags, and a short message of their
// own. The value is written plain first, as a placeholder of the same
// length, and its bytes and flags are then replaced.
func craftedJournal(tb testing.TB, n int, flags byte, stored []byte) (journalfile.Journal, []byte) {
	tb.Helper()
	placeholder := journalfile.Payload("PLACEHOLDER", bytes.Repeat([]byte("p"), len(stored)-len("PLACEHOLDER=")))
	entries := make([]journalfile.Entry, n)
	for i := range entries {
		entries[i] = journalfile.Entry{
			Realtime: caseRealtime + uint64(i), // #nosec G115 -- a small loop index
			Payloads: [][]byte{placeholder, journalfile.Payload("MESSAGE", fmt.Appendf(nil, "crafted %d", i))},
		}
	}
	j := journalfile.Write(tb, entries, journalfile.Options{})
	d := j.Data[0]
	data := bytes.Clone(j.Bytes)
	copy(data[d.PayloadOffset:d.End()], stored)
	data[d.Offset+1] = flags
	return j, data
}

func TestTextReader_BoundsTheNameSearchOfACraftedZstdValue(t *testing.T) {
	const entries = 20
	rows := []struct {
		name   string
		stored []byte
		// leftOut is true when every entry is left out for the value.
		leftOut bool
		reason  string
	}{
		{"empty blocks ending the frame", zstdFrame(append(emptyBlocks(34_000), zstdBlock(true, zstdRawBlock, nil))...), false, ""},
		{"empty blocks then a reserved block", zstdFrame(append(emptyBlocks(34_000), zstdBlock(true, zstdReservedBlock, nil))...), true, "does not decompress (zstd)"},
		{"empty blocks past the name search", zstdFrame(append(emptyBlocks(60_000), zstdBlock(true, zstdRawBlock, nil))...), true, "needs more than"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			j, data := craftedJournal(t, entries, objectCompressedZstd, row.stored)
			reads := newObjectReads(bytes.NewReader(data), j)
			tr, err := NewTextReader(reads, int64(len(data)), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Close()
			text, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			report := tr.Report()
			want := Report{Entries: entries, Shown: entries}
			if row.leftOut {
				want = Report{Entries: entries, LeftOut: entries, FirstDamaged: 1}
			}
			want.DamageReason = report.DamageReason
			if report != want || !bytes.Contains([]byte(report.DamageReason), []byte(row.reason)) {
				t.Errorf("Report = %+v, want %+v with a reason holding %q", report, want, row.reason)
			}
			if !row.leftOut && bytes.Count(text, []byte("\n")) != entries {
				t.Errorf("%d lines of text, want %d", bytes.Count(text, []byte("\n")), entries)
			}
			// Read once (the answer is cached, damage too), in at most
			// two ReadAt calls, at most the probe and the name search.
			d := j.Data[0]
			if got := reads.calls[d.Offset]; got < 1 || got > 2 {
				t.Errorf("the crafted value (%d stored bytes) was read in %d ReadAt calls, want 1 or 2", len(row.stored), got)
			}
			if limit := int64(len(data)) + int64(entries)*2*arrayProbeBytes; reads.bytes > limit {
				t.Errorf("read %d bytes of a %d-byte journal, want at most %d", reads.bytes, len(data), limit)
			}
		})
	}
}

func TestTextReader_StopsDecodingAZstdValueTheTextDoesNotShowAfterItsName(t *testing.T) {
	garbage := zstdBlock(true, zstdCompressedBlock, bytes.Repeat([]byte{0xff}, 10))
	rows := []struct {
		field   string
		leftOut bool
	}{
		// The name is in the first block; the second is never decoded.
		{"COREDUMP", false},
		// A field the text shows is decoded whole, and the second block
		// does not decode.
		{"MESSAGE", true},
	}
	for _, row := range rows {
		t.Run(row.field, func(t *testing.T) {
			first := zstdBlock(false, zstdRawBlock, journalfile.Payload(row.field, bytes.Repeat([]byte("A"), 100)))
			j, data := craftedJournal(t, 3, objectCompressedZstd, zstdFrame(first, garbage))
			text, report := readBytes(t, data, Options{})
			want := Report{Entries: 3, Shown: 3}
			if row.leftOut {
				want = Report{Entries: 3, LeftOut: 3, FirstDamaged: 1}
			}
			want.DamageReason = report.DamageReason
			if report != want {
				t.Errorf("Report = %+v, want %+v", report, want)
			}
			if !row.leftOut {
				wantText, _ := expectRead(writtenEntries(j), 3, nil, 3)
				if !bytes.Equal(text, wantText) {
					t.Errorf("text differs: %s", firstDifference(text, wantText))
				}
			}
		})
	}
}

// The size an lz4 value declares does not bound its name search. A size
// shorter than the name of a field the text shows would otherwise cut
// the name before its '=', the field would pass as one the text does not
// show, and the entry would be shown without it: with " unknown" in place
// of its identifier here. The whole decode of the shown field checks the
// block against the size and refuses it, so the entry is left out.
func TestTextReader_LeavesOutAnEntryWhoseLZ4IdentifierDeclaresASizeShorterThanItsName(t *testing.T) {
	stored := journalfile.Stored(t, journalfile.LZ4, journalfile.Payload("SYSLOG_IDENTIFIER", bytes.Repeat([]byte("A"), 200)))
	// 17 is the name's length: the text cut there has no '='.
	for _, size := range []uint64{0, 3, 17} {
		t.Run(fmt.Sprintf("size %d", size), func(t *testing.T) {
			j, data := craftedJournal(t, 3, objectCompressedLZ4, stored)
			binary.LittleEndian.PutUint64(data[j.Data[0].PayloadOffset:], size)
			text, report := readBytes(t, data, Options{})
			checkRead(t, text, report, nil, Report{Entries: 3, LeftOut: 3, FirstDamaged: 1}, "does not decompress (lz4)")
		})
	}
}

// An lz4 match of length M is written as its 2-byte offset and then about
// M/255 length bytes. A value whose text is a short name and one long
// match, such as MESSAGE= and 40 MiB of one byte, so keeps about 160 KiB
// of length bytes after the name's literals, more than the name search
// reads; the first of them already cover the bytes the name search still
// lacks. The entry is shown, with the text of the whole value.
func TestTextReader_ShowsAnLZ4MessageThatStartsWithAMatchOf40MiB(t *testing.T) {
	payload := journalfile.Payload("MESSAGE", bytes.Repeat([]byte("A"), 40<<20))
	entries := []journalfile.Entry{{Realtime: caseRealtime, Payloads: [][]byte{payload}}}
	j := journalfile.Write(t, entries, journalfile.Options{Compression: journalfile.LZ4})
	d := j.Data[0]
	stored := j.Bytes[d.PayloadOffset:d.End()]
	if d.StoredAs != journalfile.LZ4 || len(stored) <= nameStoredLimit {
		t.Fatalf("the message is stored as %s in %d bytes, want lz4 in more than %d", d.StoredAs, len(stored), nameStoredLimit)
	}
	if value, err := decodeLZ4(stored); err != nil || !bytes.Equal(value, payload) {
		t.Fatalf("the full decode gives %d bytes, %v; want the %d-byte payload", len(value), err, len(payload))
	}
	w := &compareWriter{want: expectedTextOf(j)}
	tr := streamText(t, bytes.NewReader(j.Bytes), int64(len(j.Bytes)), Options{}, w)
	if got := tr.Report(); got != (Report{Entries: 1, Shown: 1}) {
		t.Errorf("Report = %+v, want the entry shown", got)
	}
	w.check(t)
}

// A value of a field the text does not show that starts with one long
// match, such as BLOB= and 40 MiB of zero bytes, costs its name search
// alone, as any other value of such a field: the entries that list it
// are shown, and the value is neither decoded nor read past the stored
// bytes of its name search.
func TestTextReader_ReadsOnlyTheNameOfAnUnreadLZ4ValueThatStartsWithAMatchOf40MiB(t *testing.T) {
	const entries = 3
	blob := journalfile.Payload("BLOB", make([]byte, 40<<20))
	written := make([]journalfile.Entry, entries)
	for i := range written {
		written[i] = journalfile.Entry{
			Realtime: caseRealtime + uint64(i), // #nosec G115 -- a small loop index
			Payloads: [][]byte{blob, journalfile.Payload("MESSAGE", fmt.Appendf(nil, "entry %d", i))},
		}
	}
	j := journalfile.Write(t, written, journalfile.Options{Compression: journalfile.LZ4})
	d := j.Data[0]
	nameSearchBytes := int64(dataProbeBytes + nameStoredLimit)
	if storedSize := int64(d.End() - d.PayloadOffset); d.StoredAs != journalfile.LZ4 || storedSize <= nameSearchBytes { // #nosec G115 -- a small size
		t.Fatalf("the value is stored as %s in %d bytes, want lz4 in more than %d", d.StoredAs, storedSize, nameSearchBytes)
	}
	reads := newObjectReads(bytes.NewReader(j.Bytes), j)
	w := &compareWriter{want: expectedTextOf(j)}
	var tr *TextReader
	allocated := allocatedBytes(func() { tr = streamText(t, reads, int64(len(j.Bytes)), Options{}, w) })
	if got := tr.Report(); got != (Report{Entries: entries, Shown: entries}) {
		t.Errorf("Report = %+v, want every entry shown", got)
	}
	w.check(t)
	// The value whole is 40 MiB: decoded, it would count 40 MiB; read, it
	// would take every stored byte of its object.
	if limit := int64(entries * namePrefixBytes); tr.walk.decoded > limit {
		t.Errorf("decoded %d bytes for %d entries, want at most %d", tr.walk.decoded, entries, limit)
	}
	if got := reads.objectBytes[d.Offset]; got > nameSearchBytes {
		t.Errorf("the reads of the value's object took %d bytes, want at most the probe and the name search, %d", got, nameSearchBytes)
	}
	if allocated > 16<<20 {
		t.Errorf("the read allocated %d bytes", allocated)
	}
	t.Logf("decoded %d bytes, read %d bytes of the value, allocated %d bytes", tr.walk.decoded, reads.objectBytes[d.Offset], allocated)
}

// appendLZ4Length appends the bytes that continue an lz4 length whose 4
// bits in the token are 15, for a length of 15 and extra.
func appendLZ4Length(b []byte, extra int) []byte {
	for ; extra >= 255; extra -= 255 {
		b = append(b, 255)
	}
	return append(b, byte(extra))
}

// lz4AfterLiterals returns "name=", literals and then zeroes zero bytes,
// and that text as systemd stores an lz4 value, in the block a compressor
// writes for it when it finds no match in the literals: one run of
// literals up to the first zero byte, one match of the zero bytes but the
// last five, and those five as the last literals. Both lengths continue
// past their token: the run of literals is at least 15 bytes long and
// zeroes at least 25.
func lz4AfterLiterals(name string, literals []byte, zeroes int) (text, stored []byte) {
	text = append(journalfile.Payload(name, literals), make([]byte, zeroes)...)
	run := len(name) + 1 + len(literals) + 1
	stored = make([]byte, lz4SizePrefix, lz4SizePrefix+len(text))
	le.PutUint64(stored, uint64(len(text))) // #nosec G115 -- a byte count
	stored = appendLZ4Length(append(stored, 0xff), run-15)
	stored = append(stored, text[:run]...)
	stored = append(stored, 1, 0) // the match copies the byte before it
	stored = appendLZ4Length(stored, zeroes-1-5-4-15)
	return text, append(append(stored, 0x50), make([]byte, 5)...)
}

// A run of literals is written as its length bytes and then its bytes,
// about one length byte per 255 bytes of the run, so a value whose text
// starts with a run of literals longer than about 32 MiB, as lz4 writes
// for bytes it finds no match in, keeps its name past the stored bytes
// the name search reads. Its entries are left out as damaged, though the
// whole value decodes.
func TestTextReader_LeavesOutAnEntryWhoseLZ4ValueStartsWithARunOfLiteralsPastTheNameSearch(t *testing.T) {
	literals := make([]byte, 255*nameStoredLimit)
	if _, err := rand.NewChaCha8([32]byte{1}).Read(literals); err != nil {
		t.Fatal(err)
	}
	text, stored := lz4AfterLiterals("BLOB", literals, 1<<20)
	if len(stored) >= len(text) {
		t.Fatalf("the value is stored in %d bytes, not fewer than its %d bytes of text", len(stored), len(text))
	}
	if value, err := decodeLZ4(stored); err != nil || !bytes.Equal(value, text) {
		t.Fatalf("the full decode gives %d bytes, %v; want the %d-byte text", len(value), err, len(text))
	}
	_, data := craftedJournal(t, 3, objectCompressedLZ4, stored)
	got, report := readBytes(t, data, Options{})
	checkRead(t, got, report, nil, Report{Entries: 3, LeftOut: 3, FirstDamaged: 1}, "needs more than")
}

// startFailingReaderAt fails every read that starts in [from, to), with
// err, and hands out every byte asked for with the error: the read that a
// decoder's own reads (io.ReadFull and the like) would take for a success.
type startFailingReaderAt struct {
	data     []byte
	from, to int64
	err      error
}

func (f startFailingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := bytes.NewReader(f.data).ReadAt(p, off)
	if off >= f.from && off < f.to {
		return n, f.err
	}
	return n, err
}

func TestTextReader_ReturnsAnIOErrorOfTheNameSearch(t *testing.T) {
	for _, compression := range []journalfile.Compression{journalfile.Zstd, journalfile.XZ} {
		for _, field := range []string{"BLOB", "MESSAGE"} {
			t.Run(fmt.Sprintf("%s %s", compression, field), func(t *testing.T) {
				entries := []journalfile.Entry{{Realtime: caseRealtime, Payloads: [][]byte{
					journalfile.Payload(field, randomWords(1<<20, 7)), // several zstd blocks: the first gives the name
					journalfile.Payload("MESSAGE", []byte("short")),
				}}}
				j := journalfile.Write(t, entries, journalfile.Options{Compression: compression})
				d := j.Data[0]
				if d.StoredAs != compression || d.Size <= dataProbeBytes {
					t.Fatalf("the value is stored as %s in %d bytes", d.StoredAs, d.Size)
				}
				// The object's first read succeeds; the read of the rest of
				// its stored bytes fails, and no other read does.
				failure := errors.New("disk on fire")
				r := startFailingReaderAt{data: j.Bytes, from: int64(d.Offset) + 8, to: int64(d.End()), err: failure} // #nosec G115 -- small offsets
				tr, err := NewTextReader(r, int64(len(j.Bytes)), Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer tr.Close()
				if _, err := io.ReadAll(tr); !errors.Is(err, failure) {
					t.Errorf("ReadAll error = %v, want one wrapping %v; report %+v", err, failure, tr.Report())
				}
			})
		}
	}
}

// sparseReaderAt reads data, and zero bytes from its end up to size.
type sparseReaderAt struct {
	data []byte
	size int64
}

func (s sparseReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	if off < int64(len(s.data)) {
		n = copy(p, s.data[off:])
	}
	end := min(int64(len(p)), s.size-off)
	clear(p[n:max(int64(n), end)])
	if end < int64(len(p)) {
		return int(max(end, 0)), io.EOF
	}
	return len(p), nil
}

// A value whose stored bytes pass 128 MiB is damage only in a field the
// text shows, which is read whole. A value of another field, such as a
// core dump systemd-coredump keeps in COREDUMP=, costs its name search
// alone, whatever its size, and its entry is shown.
//
// The DATA object declares 128 MiB and one byte of stored value in a
// file it declares that large: the reader is a sparse one that holds the
// written journal and zero bytes after it, so the test makes no 128 MiB
// value. The value's first stored bytes are a real stream of its codec.
func TestTextReader_StoredValueAbove128MiBIsDamageOnlyInAShownField(t *testing.T) {
	codecs := []struct {
		name        string
		flags       byte
		compression journalfile.Compression
	}{
		{"plain", 0, journalfile.None},
		{"zstd", objectCompressedZstd, journalfile.Zstd},
		{"xz", objectCompressedXZ, journalfile.XZ},
		{"lz4", objectCompressedLZ4, journalfile.LZ4},
	}
	for _, field := range []string{"COREDUMP", "MESSAGE"} {
		for _, codec := range codecs {
			t.Run(fmt.Sprintf("%s %s", field, codec.name), func(t *testing.T) {
				// More than the 65 bytes of a name search, so that it
				// stops inside the value.
				payload := journalfile.Payload(field, bytes.Repeat([]byte("A"), 200))
				stored := payload
				if codec.compression != journalfile.None {
					stored = journalfile.Stored(t, codec.compression, payload)
				}
				j, data := craftedJournal(t, 3, codec.flags, stored)
				d := j.Data[0]
				size := int64(d.PayloadOffset-d.Offset) + maxValue + 1                                    // #nosec G115 -- a small header
				binary.LittleEndian.PutUint64(data[d.Offset+journalfile.OffsetObjectSize:], uint64(size)) // #nosec G115 -- positive
				fileSize := int64(d.Offset) + size + 1<<20                                                // #nosec G115 -- a small offset
				binary.LittleEndian.PutUint64(data[96:], uint64(fileSize)-journalfile.HeaderSize)         // #nosec G115 -- positive
				reads := newObjectReads(sparseReaderAt{data: data, size: fileSize}, j)
				tr, err := NewTextReader(reads, fileSize, Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer tr.Close()
				text, err := io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
				report := tr.Report()
				if field == "MESSAGE" {
					if report.LeftOut != 3 || !bytes.Contains([]byte(report.DamageReason), []byte("more than 128 MiB")) {
						t.Errorf("Report = %+v, want 3 entries left out for a value of more than 128 MiB", report)
					}
				} else {
					wantText, want := expectRead(writtenEntries(j), 3, nil, 3)
					checkRead(t, text, report, wantText, want, "")
				}
				// The value is read as far as its name search at most: the
				// probe and up to nameStoredLimit stored bytes. The bound is
				// on every byte read, not on the reads charged to the object:
				// objectReads charges a read to the last written object
				// start before it, and the object's declared span covers
				// every object written after it, so a read of the value past
				// its first bytes would be charged to a later object.
				objects := int64(len(j.Data) + len(j.Entries) + len(j.Arrays))
				if limit := int64(len(data)) + objects*arrayProbeBytes + nameStoredLimit; reads.bytes > limit {
					t.Errorf("read %d bytes of a journal written in %d bytes, want at most %d", reads.bytes, len(data), limit)
				}
				t.Logf("%d objects, written in %d bytes: %d bytes read", objects, len(data), reads.bytes)
			})
		}
	}
}
