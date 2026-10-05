package sigscan

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/journal"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/journalfile"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// highestReadAt records the highest byte of the file read so far by the
// scan's readers of the text: the end of the furthest ReadAt, leaving
// out the read of the tail object's 16-byte header at tailOffset. That
// read is filetext.ReadExtent's, which finds the progress total through
// the file itself rather than through the scan's counted reader; it lies
// at the end of the content and is not part of reading the text.
type highestReadAt struct {
	r          io.ReaderAt
	tailOffset int64
	highest    atomic.Int64
}

func (h *highestReadAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := h.r.ReadAt(p, off)
	if off == h.tailOffset && len(p) == 16 {
		return n, err
	}
	end := off + int64(n)
	for {
		seen := h.highest.Load()
		if end <= seen || h.highest.CompareAndSwap(seen, end) {
			break
		}
	}
	return n, err
}

// progressCall is one call of Options.Progress, with the highest byte of
// the file read when it was made.
type progressCall struct{ read, total, highest int64 }

// scanJournalProgress scans the journal at path with small batches (many
// progress calls) and returns every progress call.
func scanJournalProgress(t *testing.T, path string) []progressCall {
	t.Helper()
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	var file *highestReadAt
	tune := defaultTuning()
	// Small batches: the decoder reads the text a batch at a time and
	// reports the progress after each read.
	tune.batchBytes, tune.batchLines = 16<<10, 50
	tune.open = func(pin paths.Pinned) (fileReader, int64, error) {
		f, size, err := openPinned(pin)
		if err != nil {
			return nil, 0, err
		}
		header, err := journal.ReadHeader(f, size)
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		file = &highestReadAt{r: f, tailOffset: int64(header.TailObjectOffset)} //nolint:gosec // a fixture's offsets are small
		return readerAtCloser{ReaderAt: file, Closer: f}, size, nil
	}
	// Calls never overlap (Options.Progress), so the slice needs no lock.
	var calls []progressCall
	opts := Options{Workers: 2, Progress: func(read, total int64) {
		calls = append(calls, progressCall{read: read, total: total, highest: file.highest.Load()})
	}}
	if _, err := scanFile(context.Background(), classify(t, path), sigs, opts, tune); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return calls
}

// requireProgressToContentEnd checks the calls of a finished scan of a
// journal whose content ends at contentEnd: every call reports that
// total, read never goes down and never passes it, no call but the last
// reports more than the highest byte read when it was made, and the last
// call reports exactly the total.
func requireProgressToContentEnd(t *testing.T, calls []progressCall, contentEnd int64) {
	t.Helper()
	if len(calls) < 2 {
		t.Fatalf("%d progress calls, want several", len(calls))
	}
	var previous int64
	for i, c := range calls {
		if c.total != contentEnd {
			t.Fatalf("call %d: total %d, want the journal's content end %d", i, c.total, contentEnd)
		}
		if c.read < previous || c.read > c.total {
			t.Fatalf("call %d: read %d after %d, total %d", i, c.read, previous, c.total)
		}
		if i < len(calls)-1 && c.read > c.highest {
			t.Fatalf("call %d: read %d, beyond the highest byte read so far (%d)", i, c.read, c.highest)
		}
		previous = c.read
	}
	if last := calls[len(calls)-1]; last.read != last.total {
		t.Fatalf("the last call reports %d of %d, want the whole content", last.read, last.total)
	}
}

// contentEndOf is the content end of the journal at path.
func contentEndOf(t *testing.T, path string) int64 {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	end, err := journal.ContentEnd(f, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	return end
}

// A findings scan of a preallocated journal reports its progress
// against the end of the journal's last object, not the file's size,
// and ends at exactly that total.
func TestScan_APreallocatedJournalReportsProgressAgainstItsContentEnd(t *testing.T) {
	path := journalfixture.Journal(t, "many-zstd-compact")
	contentEnd := contentEndOf(t, path)
	journalfixture.Preallocate(t, path, 8<<20)
	requireProgressToContentEnd(t, scanJournalProgress(t, path), contentEnd)
}

// The progress of a journal scan is the highest byte read, not the sum
// of the reads: the timestamp detection reads the first mebibyte of the
// text before the scan reads it again from the start, and reads of
// shared values repeat, so a sum would report the scan further on than
// it is. The journal here has more than a mebibyte of text, so the head
// read covers only part of it.
func TestScan_AJournalScanReportsTheHighestByteRead(t *testing.T) {
	var entries []journalfile.Entry
	for i := range 12000 {
		entries = append(entries, journalfile.Entry{
			Realtime: 1768464000000000 + uint64(i)*250000, //nolint:gosec // i is small
			Payloads: [][]byte{
				journalfile.Payload("_HOSTNAME", []byte("scan-host")),
				journalfile.Payload("SYSLOG_IDENTIFIER", []byte("worker")),
				journalfile.Payload("_PID", []byte("4242")),
				journalfile.Payload("MESSAGE", []byte(fmt.Sprintf(
					"job %d finished with code %d after a long description of what it did, padded to take some room %08d",
					i, i%7, i*31))),
			},
		})
	}
	// Entry arrays of a fixed capacity lie along the file, between the
	// entries they list, so the head read stays in the front of the file.
	j := journalfile.Write(t, entries, journalfile.Options{ArrayCapacity: 128})
	path := filepath.Join(t.TempDir(), "system.journal")
	if err := os.WriteFile(path, j.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	contentEnd := contentEndOf(t, path)
	journalfixture.Preallocate(t, path, (int64(len(j.Bytes))/(8<<20)+1)*(8<<20))
	requireProgressToContentEnd(t, scanJournalProgress(t, path), contentEnd)
}
