package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/journalfile"
)

// readText reads the whole text of the journal r (size bytes) through a
// TextReader, handing Read buffers of chunk bytes, and returns the text
// and the report.
func readText(tb testing.TB, r io.ReaderAt, size int64, opts Options, chunk int) ([]byte, Report) {
	tb.Helper()
	tr, err := NewTextReader(r, size, opts)
	if err != nil {
		tb.Fatalf("NewTextReader: %v", err)
	}
	// defer runs Close when this function returns, whichever way.
	defer func() {
		if err := tr.Close(); err != nil {
			tb.Errorf("Close: %v", err)
		}
	}()
	var text []byte
	buf := make([]byte, chunk)
	for {
		n, err := tr.Read(buf)
		text = append(text, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return text, tr.Report()
		}
		if err != nil {
			tb.Fatalf("Read after %d bytes: %v", len(text), err)
		}
		if n == 0 {
			tb.Fatalf("Read returned no byte and no error after %d bytes", len(text))
		}
	}
}

// readBytes reads the text of the journal held in data.
func readBytes(tb testing.TB, data []byte, opts Options) ([]byte, Report) {
	tb.Helper()
	return readText(tb, bytes.NewReader(data), int64(len(data)), opts, 32<<10)
}

// entryClass is what a reader makes of one entry of a journal.
type entryClass int

const (
	classShown entryClass = iota
	classWithoutMessage
	classLeftOut
)

// writtenEntry is what a reader should give for one entry a test wrote:
// its class and, for a shown entry, its text.
type writtenEntry struct {
	class entryClass
	text  []byte
}

// entryOfPayloads is the entry the payloads of one ENTRY object make for
// the renderer: each payload split at its first '=', a payload without
// '=' left out.
func entryOfPayloads(realtime uint64, payloads [][]byte) Entry {
	e := Entry{Realtime: realtime}
	for _, payload := range payloads {
		name, value, ok := bytes.Cut(payload, []byte("="))
		if ok {
			e.Fields = append(e.Fields, Field{Name: string(name), Value: value})
		}
	}
	return e
}

// writtenEntries classifies the entries of a written journal, in list
// order, from what the test wrote: an invalid realtime makes an entry
// damaged, a missing MESSAGE an entry without a message.
func writtenEntries(j journalfile.Journal) []writtenEntry {
	out := make([]writtenEntry, len(j.Entries))
	for i, eo := range j.Entries {
		if !validRealtime(eo.Realtime) {
			out[i] = writtenEntry{class: classLeftOut}
			continue
		}
		text, shown := AppendText(nil, entryOfPayloads(eo.Realtime, j.EntryPayloads(i)))
		if !shown {
			out[i] = writtenEntry{class: classWithoutMessage}
			continue
		}
		out[i] = writtenEntry{class: classShown, text: text}
	}
	return out
}

// expectRead returns the text and the counts of a read of entries whose
// header counts headerCount entries, when the entries at the indexes in
// leftOut are damaged and the walk ends before entry notReachedFrom
// (len(entries) when it reaches every listed entry).
func expectRead(entries []writtenEntry, headerCount int, leftOut []int, notReachedFrom int) ([]byte, Report) {
	var text []byte
	report := Report{Entries: int64(headerCount)}
	for i := range min(notReachedFrom, headerCount) {
		e := entries[i]
		switch {
		case slices.Contains(leftOut, i) || e.class == classLeftOut:
			report.LeftOut++
		case e.class == classWithoutMessage:
			report.WithoutMessage++
		default:
			report.Shown++
			text = append(text, e.text...)
		}
		if report.LeftOut == 1 && report.FirstDamaged == 0 {
			report.FirstDamaged = int64(i) + 1
		}
	}
	report.NotReached = report.Entries - report.Shown - report.WithoutMessage - report.LeftOut
	if report.NotReached > 0 && report.FirstDamaged == 0 {
		report.FirstDamaged = int64(notReachedFrom) + 1
	}
	return text, report
}

// checkRead compares a read with what was expected: the text byte for
// byte, every count, the first damaged entry, and a reason that holds
// reason when anything was damaged and is empty otherwise.
func checkRead(t *testing.T, text []byte, got Report, wantText []byte, want Report, reason string) {
	t.Helper()
	if !bytes.Equal(text, wantText) {
		t.Errorf("text differs: %s", firstDifference(text, wantText))
	}
	wantReason := got.DamageReason
	if want.FirstDamaged == 0 {
		wantReason = ""
	}
	want.DamageReason = wantReason
	if got != want {
		t.Errorf("Report = %+v\nwant     %+v", got, want)
	}
	if want.FirstDamaged > 0 && !strings.Contains(got.DamageReason, reason) {
		t.Errorf("DamageReason %q does not hold %q", got.DamageReason, reason)
	}
	if got.Entries != got.Shown+got.WithoutMessage+got.LeftOut+got.NotReached {
		t.Errorf("Entries %d is not the sum of the other counts: %+v", got.Entries, got)
	}
	if got.Damaged() != (got.LeftOut > 0 || got.NotReached > 0) {
		t.Errorf("Damaged() = %v for %+v", got.Damaged(), got)
	}
}

// failingReaderAt reads data and fails every read that reaches from or
// beyond with err, the way a disk error or a cancelled context does.
type failingReaderAt struct {
	data []byte
	from int64
	err  error
}

func (f failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.from {
		return 0, f.err
	}
	return bytes.NewReader(f.data).ReadAt(p, off)
}

func TestTextReader_EveryFixtureGivesJournalctlsTextAndCounts(t *testing.T) {
	for _, f := range readFixtureManifest(t) {
		if f.IsChainPart() {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			file, err := os.Open(fixtureJournal(t, f.Name))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(f.TextPath())
			if err != nil {
				t.Fatal(err)
			}
			text, report := readText(t, file, info.Size(), Options{}, 32<<10)
			checkRead(t, text, report, want, Report{Entries: f.Entries, Shown: f.Shown, WithoutMessage: f.WithoutMessage}, "")
		})
	}
}

func TestTextReader_ReadsTheSameTextWhateverTheReadBufferSize(t *testing.T) {
	data, err := os.ReadFile(fixtureJournal(t, "basic-xz-compact"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(fixtureInfo{Set: "basic"}.TextPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{1, 7, 64, 4096} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			text, _ := readText(t, bytes.NewReader(data), int64(len(data)), Options{}, chunk)
			if !bytes.Equal(text, want) {
				t.Errorf("text differs: %s", firstDifference(text, want))
			}
		})
	}
}

func TestTextReader_EveryChainPartGivesItsCountsAndItsPartOfTheChainText(t *testing.T) {
	texts := map[string][]byte{}
	totals := map[string]int{}
	for _, f := range readFixtureManifest(t) {
		if !f.IsChainPart() {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			data, err := os.ReadFile(fixtureJournal(t, f.Name))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := texts[f.Set]; !ok {
				if texts[f.Set], err = os.ReadFile(f.TextPath()); err != nil {
					t.Fatal(err)
				}
			}
			text, report := readBytes(t, data, Options{})
			want := Report{Entries: f.Entries, Shown: f.Shown, WithoutMessage: f.WithoutMessage}
			checkRead(t, text, report, text, want, "")
			if len(text) == 0 || !bytes.Contains(texts[f.Set], text) {
				t.Errorf("the part's %d-byte text is not a part of %s", len(text), f.TextPath())
			}
			totals[f.Set] += len(text)
		})
	}
	for set, total := range totals {
		if total != len(texts[set]) {
			t.Errorf("the parts of %s give %d bytes of text, all.txt holds %d", set, total, len(texts[set]))
		}
	}
}

// damageJournal writes the journal the damage tests change: 40 entries
// listed by four ENTRY_ARRAY objects (4, 8, 26 and 78 slots), sharing a
// hostname, three identifiers and four groups, every fifth message large
// enough to be stored compressed, and every seventh entry without a
// message.
func damageJournal(tb testing.TB, compression journalfile.Compression, compact bool) journalfile.Journal {
	tb.Helper()
	entries := make([]journalfile.Entry, 40)
	for i := range entries {
		message := fmt.Appendf(nil, "damage entry %d", i)
		if i%5 == 2 {
			message = bytes.Repeat(fmt.Appendf(nil, "large entry %d; ", i), 60)
		}
		payloads := [][]byte{
			journalfile.Payload("_HOSTNAME", []byte("damage-host")),
			journalfile.Payload("SYSLOG_IDENTIFIER", fmt.Appendf(nil, "app-%d", i%3)),
			journalfile.Payload("_PID", fmt.Appendf(nil, "%d", 300+i)),
			journalfile.Payload("GROUP", fmt.Appendf(nil, "group-%d", i%4)),
		}
		if i%7 != 3 {
			payloads = append(payloads, journalfile.Payload("MESSAGE", message))
		}
		entries[i] = journalfile.Entry{
			Realtime:  caseRealtime + uint64(i)*1_000_001, // #nosec G115 -- a small loop index
			Monotonic: 7_000 + uint64(i),                  // #nosec G115 -- a small loop index
			Payloads:  payloads,
		}
	}
	return journalfile.Write(tb, entries, journalfile.Options{Compression: compression, Compact: compact})
}

// truncationOutcome returns the entries a read of j cut at cut leaves out
// and the first entry it does not reach: an array that does not fit ends
// the walk, and an entry whose ENTRY or DATA objects do not fit is left
// out.
func truncationOutcome(j journalfile.Journal, cut uint64) (leftOut []int, notReachedFrom int) {
	for _, a := range j.Arrays {
		if a.End() > cut {
			return leftOut, a.FirstEntry
		}
		for i := a.FirstEntry; i < a.FirstEntry+a.Count; i++ {
			fits := j.Entries[i].End() <= cut
			for _, item := range j.Entries[i].Items {
				fits = fits && j.Data[item].End() <= cut
			}
			if !fits {
				leftOut = append(leftOut, i)
			}
		}
	}
	return leftOut, len(j.Entries)
}

// entriesUsing returns the entries that list the DATA object d.
func entriesUsing(j journalfile.Journal, d int) []int {
	var out []int
	for i, e := range j.Entries {
		if slices.Contains(e.Items, d) {
			out = append(out, i)
		}
	}
	return out
}

// dataOf returns the index of the DATA object holding payload.
func dataOf(tb testing.TB, j journalfile.Journal, payload string) int {
	tb.Helper()
	i := slices.IndexFunc(j.Data, func(d journalfile.DataObject) bool { return string(d.Payload) == payload })
	if i < 0 {
		tb.Fatalf("no DATA object holds %q", payload)
	}
	return i
}

// itemSizes returns the size of an ENTRY item and of an ENTRY_ARRAY item.
func itemSizes(compact bool) (entryItem, arrayItem uint64) {
	if compact {
		return 4, 4
	}
	return 16, 8
}

// listItemOf returns the file offset of the entry-list item that lists
// entry i.
func listItemOf(j journalfile.Journal, i int, compact bool) uint64 {
	_, arrayItem := itemSizes(compact)
	for _, a := range j.Arrays {
		if i < a.FirstEntry+a.Count {
			return a.Offset + 24 + uint64(i-a.FirstEntry)*arrayItem // #nosec G115 -- a small index
		}
	}
	panic(fmt.Sprintf("no array lists entry %d", i))
}

// putItem writes an item's offset into data at at, in the item width of
// the journal.
func putItem(data []byte, at, value uint64, compact bool) {
	if compact {
		binary.LittleEndian.PutUint32(data[at:], uint32(value)) // #nosec G115 -- test journals are far below 4 GiB
		return
	}
	binary.LittleEndian.PutUint64(data[at:], value)
}

// damageCase changes a copy of a damage journal and says what a read of
// it gives.
type damageCase struct {
	name string
	// apply returns the changed bytes, the entries left out, the first
	// entry not reached (-1: every listed entry is reached), the
	// header's count (0: the 40 written) and a part of the reason.
	apply func(t *testing.T, j journalfile.Journal, data []byte, compact bool) (out []byte, leftOut []int, notReachedFrom, headerCount int, reason string)
}

func damageCases() []damageCase {
	return []damageCase{
		{"the file cut inside an ENTRY object", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			cut := j.Entries[20].Offset + 8
			leftOut, notReachedFrom := truncationOutcome(j, cut)
			return data[:cut], leftOut, notReachedFrom, 0, "beyond the end of the file"
		}},
		{"the file cut inside an ENTRY_ARRAY object", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			cut := j.Arrays[2].Offset + 20
			leftOut, notReachedFrom := truncationOutcome(j, cut)
			return data[:cut], leftOut, notReachedFrom, 0, "beyond the end of the file"
		}},
		{"the file cut inside a DATA object", func(t *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			cut := j.Data[dataOf(t, j, "_PID=325")].Offset + 10
			leftOut, notReachedFrom := truncationOutcome(j, cut)
			return data[:cut], leftOut, notReachedFrom, 0, "beyond the end of the file"
		}},
		{"an arena that ends inside an ENTRY object", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			cut := j.Entries[20].Offset + 8
			leftOut, notReachedFrom := truncationOutcome(j, cut)
			return withUint64(data, 96, cut-journalfile.HeaderSize), leftOut, notReachedFrom, 0, "beyond the end of the arena"
		}},
		{"a zeroed span over one entry", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			e := j.Entries[17]
			clear(data[e.Offset:e.End()])
			return data, []int{17}, -1, 0, "size 0"
		}},
		{"a changed type byte in an ENTRY object", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			data[j.Entries[9].Offset] = journalfile.ObjectData
			return data, []int{9}, -1, 0, "not an ENTRY object"
		}},
		{"a changed realtime byte in an ENTRY object", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			data[j.Entries[11].Offset+24+7] = 0x80
			return data, []int{11}, -1, 0, "invalid realtime"
		}},
		{"a changed type byte in a shared DATA object", func(t *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			d := dataOf(t, j, "GROUP=group-1")
			data[j.Data[d].Offset] = journalfile.ObjectEntry
			return data, entriesUsing(j, d), -1, 0, "not a DATA object"
		}},
		{"a DATA object whose size runs beyond the file", func(t *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			d := dataOf(t, j, "SYSLOG_IDENTIFIER=app-2")
			binary.LittleEndian.PutUint64(data[j.Data[d].Offset+journalfile.OffsetObjectSize:], 1<<40)
			return data, entriesUsing(j, d), -1, 0, "beyond the end of the file"
		}},
		{"a changed type byte in an ENTRY_ARRAY object", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			data[j.Arrays[2].Offset] = journalfile.ObjectData
			return data, nil, j.Arrays[2].FirstEntry, 0, "not an ENTRY_ARRAY object"
		}},
		{"an array pointer that goes back", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			binary.LittleEndian.PutUint64(data[j.Arrays[2].Offset+journalfile.OffsetNextEntryArray:], j.Arrays[0].Offset)
			return data, nil, j.Arrays[2].FirstEntry, 0, "not after itself"
		}},
		{"an array pointer that stays", func(_ *testing.T, j journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			binary.LittleEndian.PutUint64(data[j.Arrays[1].Offset+journalfile.OffsetNextEntryArray:], j.Arrays[1].Offset)
			return data, nil, j.Arrays[1].FirstEntry, 0, "not after itself"
		}},
		{"an entry pointer that goes back", func(_ *testing.T, j journalfile.Journal, data []byte, compact bool) ([]byte, []int, int, int, string) {
			putItem(data, listItemOf(j, 17, compact), j.Entries[3].Offset, compact)
			return data, []int{17}, -1, 0, "not after the last entry read"
		}},
		{"an entry pointer that stays", func(_ *testing.T, j journalfile.Journal, data []byte, compact bool) ([]byte, []int, int, int, string) {
			putItem(data, listItemOf(j, 17, compact), j.Entries[16].Offset, compact)
			return data, []int{17}, -1, 0, "not after the last entry read"
		}},
		{"an entry pointer beyond the file", func(_ *testing.T, j journalfile.Journal, data []byte, compact bool) ([]byte, []int, int, int, string) {
			// The entries after it are compared with the last entry read,
			// not with this pointer, so they are all read.
			putItem(data, listItemOf(j, 18, compact), 0xfffffff8, compact)
			return data, []int{18}, -1, 0, "beyond the end of the file"
		}},
		{"an entry pointer forward to a DATA object", func(t *testing.T, j journalfile.Journal, data []byte, compact bool) ([]byte, []int, int, int, string) {
			putItem(data, listItemOf(j, 22, compact), j.Data[dataOf(t, j, "_PID=325")].Offset, compact)
			return data, []int{22}, -1, 0, "not an ENTRY object"
		}},
		{"two bad entry pointers in one array", func(_ *testing.T, j journalfile.Journal, data []byte, compact bool) ([]byte, []int, int, int, string) {
			putItem(data, listItemOf(j, 13, compact), 0xfffffff8, compact)
			putItem(data, listItemOf(j, 14, compact), j.Entries[2].Offset, compact)
			return data, []int{13, 14}, -1, 0, "beyond the end of the file"
		}},
		{"an entry item that points at an ENTRY_ARRAY object", func(_ *testing.T, j journalfile.Journal, data []byte, compact bool) ([]byte, []int, int, int, string) {
			putItem(data, j.Entries[30].Offset+64, j.Arrays[1].Offset, compact)
			return data, []int{30}, -1, 0, "not a DATA object"
		}},
		{"an entry item that is not a multiple of 8", func(_ *testing.T, j journalfile.Journal, data []byte, compact bool) ([]byte, []int, int, int, string) {
			e := j.Entries[31]
			putItem(data, e.Offset+64, j.Data[e.Items[0]].Offset+4, compact)
			return data, []int{31}, -1, 0, "multiple of 8"
		}},
		{"a header that counts more entries than the list holds", func(_ *testing.T, _ journalfile.Journal, data []byte, _ bool) ([]byte, []int, int, int, string) {
			return withUint64(data, journalfile.OffsetNEntries, 45), nil, 40, 45, "ends after 40 of the 45"
		}},
	}
}

func TestTextReader_LeavesOutDamagedEntriesAndStopsAtABrokenList(t *testing.T) {
	for _, compression := range journalfile.Compressions {
		for _, compact := range []bool{false, true} {
			j := damageJournal(t, compression, compact)
			entries := writtenEntries(j)
			for _, c := range damageCases() {
				t.Run(fmt.Sprintf("%s-compact=%v/%s", compression, compact, c.name), func(t *testing.T) {
					data, leftOut, notReachedFrom, headerCount, reason := c.apply(t, j, bytes.Clone(j.Bytes), compact)
					if notReachedFrom < 0 {
						notReachedFrom = len(j.Entries)
					}
					if headerCount == 0 {
						headerCount = len(j.Entries)
					}
					wantText, want := expectRead(entries, headerCount, leftOut, notReachedFrom)
					text, report := readBytes(t, data, Options{})
					checkRead(t, text, report, wantText, want, reason)
					if !report.Damaged() {
						t.Errorf("the damage was not reported: %+v", report)
					}
				})
			}
		}
	}
}

func TestTextReader_LeavesOutAnEntryWhoseCompressedValueDoesNotDecode(t *testing.T) {
	for _, compression := range journalfile.Compressions[1:] {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-compact=%v", compression, compact), func(t *testing.T) {
				j := damageJournal(t, compression, compact)
				d := dataOf(t, j, string(journalfile.Payload("MESSAGE", bytes.Repeat([]byte("large entry 22; "), 60))))
				if j.Data[d].StoredAs != compression {
					t.Fatalf("the large message is stored as %s", j.Data[d].StoredAs)
				}
				data := bytes.Clone(j.Bytes)
				at := j.Data[d].PayloadOffset
				if compression == journalfile.LZ4 {
					// The size lz4 declares no longer matches the block.
					binary.LittleEndian.PutUint64(data[at:], binary.LittleEndian.Uint64(data[at:])+1)
				} else {
					data[at] ^= 0xff // the stream's magic
				}
				wantText, want := expectRead(writtenEntries(j), len(j.Entries), []int{22}, len(j.Entries))
				text, report := readBytes(t, data, Options{})
				checkRead(t, text, report, wantText, want, "does not decompress")
			})
		}
	}
}

// A DATA object zeroed after its 16-byte header keeps its type, its lz4
// flag and its size, and its zeroed links pass their checks; its value is
// then a size prefix of 0 before a block of zero bytes, which does not
// decode. The entry is left out: the size prefix must not cut the name
// search short, or the MESSAGE would be read as a field the text does
// not show and the entry counted as one without a message.
func TestTextReader_LeavesOutAnEntryWhoseLZ4MessageIsZeroedAfterItsObjectHeader(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%v", compact), func(t *testing.T) {
			j := damageJournal(t, journalfile.LZ4, compact)
			d := dataOf(t, j, string(journalfile.Payload("MESSAGE", bytes.Repeat([]byte("large entry 22; "), 60))))
			if j.Data[d].StoredAs != journalfile.LZ4 {
				t.Fatalf("the large message is stored as %s", j.Data[d].StoredAs)
			}
			data := bytes.Clone(j.Bytes)
			clear(data[j.Data[d].Offset+objectHeaderSize : j.Data[d].End()])
			wantText, want := expectRead(writtenEntries(j), len(j.Entries), entriesUsing(j, d), len(j.Entries))
			text, report := readBytes(t, data, Options{})
			checkRead(t, text, report, wantText, want, "does not decompress (lz4)")
		})
	}
}

func TestTextReader_LeavesOutAnEntryWhoseValueDecompressesToMoreThan128MiB(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%v", compact), func(t *testing.T) {
			j := damageJournal(t, journalfile.LZ4, compact)
			d := dataOf(t, j, string(journalfile.Payload("MESSAGE", bytes.Repeat([]byte("large entry 22; "), 60))))
			if j.Data[d].StoredAs != journalfile.LZ4 {
				t.Fatalf("the large message is stored as %s", j.Data[d].StoredAs)
			}
			data := bytes.Clone(j.Bytes)
			// The size lz4 declares: 1 TiB, refused before its buffer is
			// made.
			binary.LittleEndian.PutUint64(data[j.Data[d].PayloadOffset:], 1<<40)
			wantText, want := expectRead(writtenEntries(j), len(j.Entries), []int{22}, len(j.Entries))
			var text []byte
			var report Report
			allocated := allocatedBytes(func() { text, report = readBytes(t, data, Options{}) })
			checkRead(t, text, report, wantText, want, "decompresses to more than 128 MiB (lz4)")
			if allocated > 16<<20 {
				t.Errorf("the read allocated %d bytes", allocated)
			}
		})
	}
}

func TestTextReader_ShortReadsAtTheEndOfTheFileAreDamageNotErrors(t *testing.T) {
	j := damageJournal(t, journalfile.Zstd, true)
	cut := j.Entries[20].Offset + 8
	// The caller says the file is as large as before, but reads end at
	// the cut, as when a file shrinks while it is read.
	r := bytes.NewReader(j.Bytes[:cut])
	leftOut, notReachedFrom := truncationOutcome(j, cut)
	wantText, want := expectRead(writtenEntries(j), len(j.Entries), leftOut, notReachedFrom)
	text, report := readText(t, r, int64(len(j.Bytes)), Options{}, 32<<10)
	checkRead(t, text, report, wantText, want, "")
	if report.DamageReason == "" {
		t.Error("no damage reason")
	}
}

func TestTextReader_ReturnsAnIOErrorFromTheReaderWrapped(t *testing.T) {
	j := damageJournal(t, journalfile.None, false)
	failure := errors.New("disk on fire")
	r := failingReaderAt{data: j.Bytes, from: int64(j.Entries[25].Offset), err: failure} // #nosec G115 -- a small offset
	tr, err := NewTextReader(r, int64(len(j.Bytes)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	text, err := io.ReadAll(tr)
	if !errors.Is(err, failure) {
		t.Fatalf("ReadAll error = %v, want one wrapping %v", err, failure)
	}
	full, _ := readBytes(t, j.Bytes, Options{})
	if !bytes.HasPrefix(full, text) || len(text) == 0 {
		t.Errorf("the %d bytes read before the error are not the start of the text", len(text))
	}
	if _, err := tr.Read(make([]byte, 10)); !errors.Is(err, failure) {
		t.Errorf("a later Read returns %v, want the same error", err)
	}
}

// partialFailingReaderAt fails every read that reaches from or beyond,
// as failingReaderAt does, but hands out bytes with the error, as
// os.File.ReadAt does when a read fails partway: those before from, and
// at least the first half of the bytes asked for.
type partialFailingReaderAt failingReaderAt

func (f partialFailingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) <= f.from {
		return bytes.NewReader(f.data).ReadAt(p, off)
	}
	end := min(max(f.from, off+int64(len(p)/2)), int64(len(f.data)))
	n := 0
	if off < end {
		n = copy(p, f.data[off:end])
	}
	return n, f.err
}

func TestTextReader_ReturnsAnIOErrorThatComesWithSomeBytes(t *testing.T) {
	j := damageJournal(t, journalfile.None, false)
	failure := errors.New("disk on fire")
	// From inside entry 25 on, every read fails partway: the bytes that
	// arrive hold whole object headers, often whole objects.
	from := int64(j.Entries[25].Offset) + 24 // #nosec G115 -- a small offset
	r := partialFailingReaderAt{data: j.Bytes, from: from, err: failure}
	tr, err := NewTextReader(r, int64(len(j.Bytes)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	text, err := io.ReadAll(tr)
	if !errors.Is(err, failure) {
		t.Fatalf("ReadAll error = %v, want one wrapping %v; report %+v", err, failure, tr.Report())
	}
	full, _ := readBytes(t, j.Bytes, Options{})
	if !bytes.HasPrefix(full, text) {
		t.Errorf("the %d bytes read before the error are not the start of the text", len(text))
	}
	if _, err := ReadHeader(partialFailingReaderAt{data: j.Bytes, from: 100, err: failure}, int64(len(j.Bytes))); !errors.Is(err, failure) {
		t.Errorf("ReadHeader error = %v, want one wrapping %v", err, failure)
	}
}

func TestTextReader_ReadsOnlyTheEntriesTheHeaderCountsOfALiveFile(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%v", compact), func(t *testing.T) {
			j := damageJournal(t, journalfile.Zstd, compact)
			// A live file mid-write: the arrays list 40 entries, the header
			// counts 30 of them.
			data := withUint64(j.Bytes, journalfile.OffsetNEntries, 30)
			data[16] = byte(StateOnline)
			wantText, want := expectRead(writtenEntries(j), 30, nil, 30)
			text, report := readBytes(t, data, Options{})
			checkRead(t, text, report, wantText, want, "")
		})
	}
}

func TestTextReader_EntryListedButNotCountedAndAnEmptyJournal(t *testing.T) {
	empty := journalfile.Write(t, nil, journalfile.Options{})
	text, report := readBytes(t, empty.Bytes, Options{})
	checkRead(t, text, report, nil, Report{}, "")

	j := damageJournal(t, journalfile.None, false)
	noList := withUint64(j.Bytes, 176, 0)
	text, report = readBytes(t, noList, Options{})
	checkRead(t, text, report, nil, Report{Entries: 40, NotReached: 40, FirstDamaged: 1}, "no entry list")
}

func TestTextReader_EntryWithAnInvalidTimeOrIdIsLeftOutNotCountedWithoutMessage(t *testing.T) {
	message := journalfile.Payload("MESSAGE", []byte("kept"))
	entries := []journalfile.Entry{
		{Realtime: caseRealtime, Payloads: [][]byte{message}},
		{Realtime: 0, Payloads: [][]byte{journalfile.Payload("MESSAGE", []byte("realtime 0"))}},
		{Realtime: 1 << 55, Payloads: [][]byte{journalfile.Payload("MESSAGE", []byte("realtime 2^55"))}},
		{Realtime: 0, Payloads: [][]byte{journalfile.Payload("PRIORITY", []byte("6"))}},
		{Realtime: caseRealtime + 1, Payloads: [][]byte{message}},
		{Realtime: caseRealtime + 2, Payloads: [][]byte{message}},
		{Realtime: caseRealtime + 3, Payloads: [][]byte{message}},
		{Realtime: caseRealtime + 4, Payloads: [][]byte{message}},
	}
	j := journalfile.Write(t, entries, journalfile.Options{})
	data := bytes.Clone(j.Bytes)
	// systemd refuses these ENTRY objects too (journal-file.c:1001-1022):
	// a sequence number 0, a monotonic time of 2^55, an empty boot id.
	binary.LittleEndian.PutUint64(data[j.Entries[5].Offset+16:], 0)
	binary.LittleEndian.PutUint64(data[j.Entries[6].Offset+32:], 1<<55)
	clear(data[j.Entries[7].Offset+40 : j.Entries[7].Offset+56])

	wantText, want := expectRead(writtenEntries(j), len(entries), []int{1, 2, 3, 5, 6, 7}, len(entries))
	text, report := readBytes(t, data, Options{})
	checkRead(t, text, report, wantText, want, "invalid realtime 0")
	if report.LeftOut != 6 || report.WithoutMessage != 0 || report.Shown != 2 {
		t.Errorf("Report = %+v, want 2 shown and 6 left out", report)
	}
}

func TestTextReader_PayloadWithoutAnEqualsSignIsSkippedNotDamage(t *testing.T) {
	entries := []journalfile.Entry{{
		Realtime: caseRealtime,
		Payloads: [][]byte{
			journalfile.Payload("_HOSTNAME", []byte("host")),
			[]byte("NO_SEPARATOR_HERE"),
			journalfile.Payload("MESSAGE", []byte("still shown")),
		},
	}}
	j := journalfile.Write(t, entries, journalfile.Options{})
	text, report := readBytes(t, j.Bytes, Options{})
	want := caseStamp + " host unknown: still shown\n"
	checkRead(t, text, report, []byte(want), Report{Entries: 1, Shown: 1}, "")
}

func TestTextReader_EntryThatWouldRenderToMoreThan256MiBIsLeftOut(t *testing.T) {
	// A 935-byte prefix indents every continuation line by 935 spaces, so
	// 287,200 newlines need more than 256 MiB of room.
	long := func(c byte) []byte { return bytes.Repeat([]byte{c}, 299) }
	payloads := [][]byte{
		journalfile.Payload("_HOSTNAME", long('h')),
		journalfile.Payload("SYSLOG_IDENTIFIER", long('i')),
		journalfile.Payload("_PID", long('9')),
	}
	huge := journalfile.Entry{Realtime: caseRealtime, Payloads: append(slices.Clone(payloads),
		journalfile.Payload("MESSAGE", bytes.Repeat([]byte("\n"), 287_200)))}
	small := journalfile.Entry{Realtime: caseRealtime + 1, Payloads: [][]byte{journalfile.Payload("MESSAGE", []byte("after"))}}
	j := journalfile.Write(t, []journalfile.Entry{huge, small}, journalfile.Options{Compression: journalfile.Zstd})

	room, shown := textRoom(entryOfPayloads(huge.Realtime, j.EntryPayloads(0)))
	if !shown || room <= maxEntryText {
		t.Fatalf("textRoom = %d, %v: the test entry does not exceed the bound", room, shown)
	}
	var text []byte
	var report Report
	allocated := allocatedBytes(func() { text, report = readBytes(t, j.Bytes, Options{}) })
	wantText, want := expectRead(writtenEntries(j), 2, []int{0}, 2)
	checkRead(t, text, report, wantText, want, "renders to more than 256 MiB of text")
	if report.DamageReason != "renders to more than 256 MiB of text" {
		t.Errorf("DamageReason = %q", report.DamageReason)
	}
	if allocated > 64<<20 {
		t.Errorf("the read allocated %d bytes, want no room made for the text", allocated)
	}
}

func TestTextRoom_IsWhatAppendTextReservesFromAnEmptyBuffer(t *testing.T) {
	for _, c := range renderCases(t) {
		e := entryFromCase(t, c)
		room, shown := textRoom(e)
		out, appended := AppendText(nil, e)
		if shown != appended {
			t.Errorf("%s: textRoom says shown=%v, AppendText %v", c.Name, shown, appended)
			continue
		}
		if shown && (cap(out) != room || len(out) > room) {
			t.Errorf("%s: textRoom %d, AppendText reserved %d for %d bytes", c.Name, room, cap(out), len(out))
		}
	}
}

func TestTextReader_ReadAfterCloseFails(t *testing.T) {
	data := smallJournal(t)
	tr, err := NewTextReader(bytes.NewReader(data), int64(len(data)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	if n, err := tr.Read(make([]byte, 64)); n != 0 || err == nil || errors.Is(err, io.EOF) {
		t.Errorf("Read after Close = %d, %v, want an error other than io.EOF", n, err)
	}
}
