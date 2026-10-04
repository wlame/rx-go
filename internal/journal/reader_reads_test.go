package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

func TestTextReader_RefusesAStoredValueAbove128MiBInAnyField(t *testing.T) {
	for _, field := range []string{"BLOB", "MESSAGE"} {
		for _, flags := range []byte{0, objectCompressedZstd, objectCompressedXZ} {
			t.Run(fmt.Sprintf("%s flags %d", field, flags), func(t *testing.T) {
				stored := journalfile.Payload(field, []byte("a value of the test"))
				j, data := craftedJournal(t, 3, flags, stored)
				// The DATA object claims 128 MiB and one byte of value, in
				// a file that is that large; only its first bytes exist.
				d := j.Data[0]
				size := int64(d.PayloadOffset-d.Offset) + maxValue + 1                                    // #nosec G115 -- a small header
				binary.LittleEndian.PutUint64(data[d.Offset+journalfile.OffsetObjectSize:], uint64(size)) // #nosec G115 -- positive
				fileSize := int64(d.Offset) + size + 1<<20                                                // #nosec G115 -- a small offset
				binary.LittleEndian.PutUint64(data[96:], uint64(fileSize)-journalfile.HeaderSize)         // #nosec G115 -- positive
				counted := newObjectReads(sparseReaderAt{data: data, size: fileSize}, j)
				tr, err := NewTextReader(counted, fileSize, Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer tr.Close()
				if _, err := io.ReadAll(tr); err != nil {
					t.Fatal(err)
				}
				report := tr.Report()
				if report.LeftOut != 3 || !bytes.Contains([]byte(report.DamageReason), []byte("more than 128 MiB")) {
					t.Errorf("Report = %+v, want 3 entries left out for a value of more than 128 MiB", report)
				}
				if counted.bytes > 1<<20 {
					t.Errorf("read %d bytes", counted.bytes)
				}
			})
		}
	}
}
