package journal

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/internal/testutil/journalfile"
)

// compareWriter checks the bytes written to it against want as they
// arrive, so that a test can read a long text without holding it.
type compareWriter struct {
	want []byte
	n    int
	diff string // the first difference, "" while there is none
}

func (w *compareWriter) Write(p []byte) (int, error) {
	if w.diff == "" {
		end := min(w.n+len(p), len(w.want))
		if !bytes.Equal(p[:end-w.n], w.want[w.n:end]) || w.n+len(p) > len(w.want) {
			got := append(append([]byte(nil), w.want[:w.n]...), p...)
			w.diff = firstDifference(got, w.want[:min(len(got), len(w.want))])
			if w.diff == "" {
				w.diff = fmt.Sprintf("the text runs past the %d bytes expected", len(w.want))
			}
		}
	}
	w.n += len(p)
	return len(p), nil
}

// check fails the test unless exactly the expected bytes were written.
func (w *compareWriter) check(tb testing.TB) {
	tb.Helper()
	if w.diff != "" {
		tb.Fatalf("text differs: %s", w.diff)
	}
	if w.n != len(w.want) {
		tb.Fatalf("text of %d bytes, want %d", w.n, len(w.want))
	}
}

// readStartRecorder counts the ReadAt calls that start at each offset.
// ReadAt may be called from several goroutines, so a mutex guards the
// map.
type readStartRecorder struct {
	r      io.ReaderAt
	mu     sync.Mutex
	starts map[int64]int
}

func (r *readStartRecorder) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	r.starts[off]++
	r.mu.Unlock()
	return r.r.ReadAt(p, off)
}

// callCounter counts ReadAt calls. The count is atomic because ReadAt
// may be called from several goroutines.
type callCounter struct {
	r     io.ReaderAt
	calls atomic.Int64
}

func (c *callCounter) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	return c.r.ReadAt(p, off)
}

// streamText reads the journal r through a TextReader into w and
// returns the reader, closed, for its report and cache.
func streamText(tb testing.TB, r io.ReaderAt, size int64, opts Options, w io.Writer) *TextReader {
	tb.Helper()
	tr, err := NewTextReader(r, size, opts)
	if err != nil {
		tb.Fatalf("NewTextReader: %v", err)
	}
	// io.Copy calls tr.Read with a 32 KiB buffer until io.EOF, which it
	// does not return as an error.
	if _, err := io.Copy(w, tr); err != nil {
		tb.Fatalf("read: %v", err)
	}
	return tr
}

// expectedTextOf renders every entry of j as the reader should.
func expectedTextOf(j journalfile.Journal) []byte {
	var text []byte
	for _, e := range writtenEntries(j) {
		text = append(text, e.text...)
	}
	return text
}

func TestTextReader_EntriesSharingFiveValuesReadWithinTheCacheBoundOneReadPerEntry(t *testing.T) {
	const n = 200_000
	shared := [][]byte{
		journalfile.Payload("_HOSTNAME", []byte("scale-host")),
		journalfile.Payload("SYSLOG_IDENTIFIER", []byte("scale-app")),
		journalfile.Payload("_PID", []byte("4242")),
		journalfile.Payload("PRIORITY", []byte("6")),
		journalfile.Payload("MESSAGE", []byte("the same message every time")),
	}
	entries := make([]journalfile.Entry, n)
	for i := range entries {
		entries[i] = journalfile.Entry{Realtime: caseRealtime + uint64(i)*1_000, Monotonic: uint64(i), Payloads: shared} // #nosec G115 -- a loop index
	}
	j := journalfile.Write(t, entries, journalfile.Options{Compact: true})
	want := expectedTextOf(j)

	counted := counting.NewReaderAt(bytes.NewReader(j.Bytes))
	calls := &callCounter{r: counted}
	w := &compareWriter{want: want}
	var tr *TextReader
	allocated := allocatedBytes(func() { tr = streamText(t, calls, int64(len(j.Bytes)), Options{}, w) })
	w.check(t)
	if got := tr.Report(); got != (Report{Entries: n, Shown: n}) {
		t.Errorf("Report = %+v", got)
	}

	cache := tr.walk.cache
	if cache.misses != int64(len(shared)) || cache.maxHeld > defaultCacheBytes {
		t.Errorf("the cache missed %d times holding at most %d bytes, want %d misses within %d bytes", cache.misses, cache.maxHeld, len(shared), defaultCacheBytes)
	}
	// Reads: the header, one per entry, one per shared value, and the
	// arrays in pieces.
	var arrayBytes uint64
	for _, a := range j.Arrays {
		arrayBytes += a.Size
	}
	readCalls := calls.calls.Load()
	maxCalls := int64(1 + n + len(shared) + 2*len(j.Arrays) + int(arrayBytes/itemChunkBytes)) // #nosec G115 -- a few MiB
	if readCalls > maxCalls {
		t.Errorf("%d ReadAt calls for %d entries, want at most %d", readCalls, n, maxCalls)
	}
	if read := counted.Load(); read > 8*int64(len(j.Bytes)) {
		t.Errorf("read %d bytes of a %d-byte journal, want at most 8 times its size", read, len(j.Bytes))
	}
	// The reader renders one entry at a time into one buffer: what it
	// allocates is far below the text it gives.
	if limit := uint64(len(want) / 8); allocated > limit {
		t.Errorf("reading a %d-byte text allocated %d bytes, want at most %d", len(want), allocated, limit)
	}
	t.Logf("%d entries, %d-byte journal, %d-byte text: %d ReadAt calls, %d bytes read, %d bytes allocated",
		n, len(j.Bytes), len(want), readCalls, counted.Load(), allocated)
}

// uniqueValueJournal writes n entries that share a hostname and an
// identifier and each have a message of 64 KiB found nowhere else.
func uniqueValueJournal(tb testing.TB, n int, compression journalfile.Compression) journalfile.Journal {
	tb.Helper()
	const valueSize = 64 << 10
	entries := make([]journalfile.Entry, n)
	state := uint32(1)
	for i := range entries {
		message := fmt.Appendf(make([]byte, 0, valueSize), "entry %d ", i)
		for len(message) < valueSize {
			state = state*1_664_525 + 1_013_904_223 // a fixed linear congruential sequence
			message = append(message, 'a'+byte(state>>24)%26)
		}
		entries[i] = journalfile.Entry{
			Realtime: caseRealtime + uint64(i), // #nosec G115 -- a loop index
			Payloads: [][]byte{
				journalfile.Payload("_HOSTNAME", []byte("unique-host")),
				journalfile.Payload("SYSLOG_IDENTIFIER", []byte("unique-app")),
				journalfile.Payload("MESSAGE", message),
			},
		}
	}
	return journalfile.Write(tb, entries, journalfile.Options{Compression: compression})
}

func TestTextReader_UniqueLargeValuesReadCorrectlyWithTheCacheFull(t *testing.T) {
	rows := []struct {
		name        string
		entries     int
		compression journalfile.Compression
		cacheBytes  int64
	}{
		{"plain values, 1 MiB cache", 300, journalfile.None, 1 << 20},
		{"zstd values, 1 MiB cache", 300, journalfile.Zstd, 1 << 20},
		{"lz4 values, the default cache", 600, journalfile.LZ4, 0},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			j := uniqueValueJournal(t, row.entries, row.compression)
			opts := Options{CacheBytes: row.cacheBytes}
			calls := &readStartRecorder{r: bytes.NewReader(j.Bytes), starts: map[int64]int{}}
			w := &compareWriter{want: expectedTextOf(j)}
			tr := streamText(t, calls, int64(len(j.Bytes)), opts, w)
			w.check(t)
			if got := tr.Report(); got != (Report{Entries: int64(row.entries), Shown: int64(row.entries)}) {
				t.Errorf("Report = %+v", got)
			}
			cache := tr.walk.cache
			bound := opts.cacheBytes()
			if cache.maxHeld > bound || cache.maxHeld < bound/2 {
				t.Errorf("the cache held at most %d bytes, want it full within %d", cache.maxHeld, bound)
			}
			// Every DATA object is read once: the two shared values stay
			// cached while the unique ones pass through.
			for i, d := range j.Data {
				if got := calls.starts[int64(d.Offset)]; got != 1 { // #nosec G115 -- a small offset
					t.Errorf("DATA object %d (%d bytes) read %d times, want once", i, len(d.Payload), got)
				}
			}
		})
	}
}

func TestTextReader_MessageOf4MiBIn200000LinesReadsInBoundedMemory(t *testing.T) {
	const (
		messageBytes = 4 << 20
		lines        = 200_000
	)
	message := longMessage(messageBytes, lines)
	e := journalfile.Entry{Realtime: caseRealtime, Payloads: [][]byte{
		journalfile.Payload("_HOSTNAME", []byte("testhost")),
		journalfile.Payload("SYSLOG_IDENTIFIER", []byte("demo")),
		journalfile.Payload("_PID", []byte("100")),
		journalfile.Payload("PRIORITY", []byte("6")),
		journalfile.Payload("MESSAGE", message),
	}}
	after := journalfile.Entry{Realtime: caseRealtime, Payloads: [][]byte{journalfile.Payload("MESSAGE", []byte("after"))}}
	want := append(expectedLines(message), caseStamp+" unknown: after\n"...)
	for _, compression := range journalfile.Compressions {
		t.Run(string(compression), func(t *testing.T) {
			j := journalfile.Write(t, []journalfile.Entry{e, after}, journalfile.Options{Compression: compression})
			w := &compareWriter{want: want}
			var tr *TextReader
			allocated := allocatedBytes(func() { tr = streamText(t, bytes.NewReader(j.Bytes), int64(len(j.Bytes)), Options{}, w) })
			w.check(t)
			// The text of the entry once, and the value stored and decoded
			// (a decoder told the value's size makes it once). xz does not
			// say the size up front: the value is read into buffers that
			// double, at most four times its size in all (this value is 8
			// bytes past 4 MiB, the worst case), the decoder allocates
			// about the value's size again as it decodes, and it reserves
			// the dictionary the stream declares (8 MiB from the test
			// writer's xz, which does not record the value's size in the
			// block header as systemd's liblzma does).
			limit := uint64(len(want) + 3*messageBytes)
			if compression == journalfile.XZ {
				limit = uint64(len(want)+6*messageBytes) + 8<<20
			}
			if allocated > limit {
				t.Errorf("allocated %d bytes for a %d-byte text, want at most %d", allocated, len(want), limit)
			}
			if c := cap(tr.buf); c > 1<<20 {
				t.Errorf("after the last entry the reader still holds a %d-byte buffer", c)
			}
			t.Logf("%s: a %d-byte text allocated %d bytes", compression, len(want), allocated)
		})
	}
}

// largeUnreadValueJournal writes n entries that each list one value of
// 4 MiB, in a field the renderer does not read, and a short message of
// their own. Compressed, the value takes a few hundred bytes: a value
// that is cheap to store and costly to decode.
func largeUnreadValueJournal(tb testing.TB, n int, compression journalfile.Compression) journalfile.Journal {
	tb.Helper()
	large := journalfile.Payload("COREDUMP", bytes.Repeat([]byte("A"), 4<<20))
	entries := make([]journalfile.Entry, n)
	for i := range entries {
		entries[i] = journalfile.Entry{
			Realtime: caseRealtime + uint64(i), // #nosec G115 -- a loop index
			Payloads: [][]byte{large, journalfile.Payload("MESSAGE", fmt.Appendf(nil, "entry %d", i))},
		}
	}
	return journalfile.Write(tb, entries, journalfile.Options{Compression: compression})
}

func TestTextReader_DecodesOnlyTheNameOfALargeValueTheRendererDoesNotRead(t *testing.T) {
	const entries = 100
	for _, compression := range []journalfile.Compression{journalfile.Zstd, journalfile.XZ} {
		j := largeUnreadValueJournal(t, entries, compression)
		if stored := j.Data[0]; stored.StoredAs != compression || stored.Size > 64<<10 {
			t.Fatalf("the large value is stored as %s in %d bytes", stored.StoredAs, stored.Size)
		}
		want := expectedTextOf(j)
		for _, cacheBytes := range []int64{1 << 20, -1} {
			t.Run(fmt.Sprintf("%s, cache %d", compression, cacheBytes), func(t *testing.T) {
				w := &compareWriter{want: want}
				var tr *TextReader
				allocated := allocatedBytes(func() {
					tr = streamText(t, bytes.NewReader(j.Bytes), int64(len(j.Bytes)), Options{CacheBytes: cacheBytes}, w)
				})
				w.check(t)
				// Each entry decodes at most the name of the large value,
				// 65 bytes; with a cache, the first entry alone does. The
				// value whole is 4 MiB per entry.
				if limit := int64(entries * namePrefixBytes); tr.walk.decoded > limit {
					t.Errorf("decoded %d bytes for %d entries, want at most %d", tr.walk.decoded, entries, limit)
				}
				// The zstd decoder holds a window as large as the value's
				// frame declares (4 MiB here), once for the whole read.
				if allocated > 16<<20 {
					t.Errorf("the read allocated %d bytes", allocated)
				}
				t.Logf("decoded %d bytes, allocated %d bytes", tr.walk.decoded, allocated)
			})
		}
	}
}
