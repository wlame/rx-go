package journal

import (
	"bytes"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"os"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/journalfile"
)

// textChecker receives a journal's text and checks, as it arrives, what
// holds for any text the renderer writes: no NUL byte, and a final '\n'.
// It keeps a hash of the text, so that two reads can be compared without
// holding either text.
type textChecker struct {
	n    int64
	last byte
	nul  bool
	hash hash.Hash64
}

func newTextChecker() *textChecker { return &textChecker{hash: fnv.New64a()} }

func (c *textChecker) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.nul = c.nul || bytes.IndexByte(p, 0) >= 0
		c.last = p[len(p)-1]
		c.n += int64(len(p))
		c.hash.Write(p) // a hash.Hash's Write never returns an error
	}
	return len(p), nil
}

// readChecked reads the text of data with opts into a textChecker. It
// returns nil when NewTextReader refuses the file, which must then be
// for a missing signature or a header rx cannot read.
func readChecked(t *testing.T, data []byte, opts Options) (*textChecker, Report) {
	t.Helper()
	tr, err := NewTextReader(bytes.NewReader(data), int64(len(data)), opts)
	if err != nil {
		if !errors.Is(err, ErrUnreadable) && !errors.Is(err, errNotJournal) {
			t.Fatalf("NewTextReader error %v is neither a refusal nor a missing signature", err)
		}
		return nil, Report{}
	}
	defer tr.Close()
	checker := newTextChecker()
	if _, err := io.Copy(checker, tr); err != nil {
		t.Fatalf("read: %v", err)
	}
	return checker, tr.Report()
}

// fuzzSeedJournals are small journals of every compression and mode,
// with values large enough to be compressed, an entry without a message
// and a multi-line message: inputs short enough for the fuzzer to
// change usefully.
func fuzzSeedJournals(tb testing.TB) [][]byte {
	tb.Helper()
	entries := []journalfile.Entry{
		{Realtime: caseRealtime, Payloads: [][]byte{
			journalfile.Payload("_HOSTNAME", []byte("fuzz-host")),
			journalfile.Payload("MESSAGE", []byte("first\nsecond")),
		}},
		{Realtime: caseRealtime + 1, Payloads: [][]byte{journalfile.Payload("PRIORITY", []byte("6"))}},
		{Realtime: caseRealtime + 2, Payloads: [][]byte{
			journalfile.Payload("_HOSTNAME", []byte("fuzz-host")),
			journalfile.Payload("MESSAGE", bytes.Repeat([]byte("compressed "), 60)),
		}},
	}
	var seeds [][]byte
	for _, compression := range journalfile.Compressions {
		for _, compact := range []bool{false, true} {
			j := journalfile.Write(tb, entries, journalfile.Options{Compression: compression, Compact: compact, ArrayCapacity: 2})
			seeds = append(seeds, j.Bytes)
		}
	}
	return seeds
}

// FuzzTextReader reads changed journals: whatever the bytes, the reader
// does not panic, gives no error but the header's refusal, keeps its
// counts consistent and writes only text the renderer can write; and the
// DATA cache is an accelerator, so a read without it gives the same text
// and the same report. Memory stays bounded by the reader's own bounds
// (the DATA cache, 128 MiB per value, 256 MiB per entry), and every read
// of the walk moves forward.
func FuzzTextReader(f *testing.F) {
	for _, name := range []string{"basic-zstd-compact", "many-lz4-regular", "stamps-xz-compact"} {
		data, err := os.ReadFile(fixtureJournal(f, name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	for _, seed := range fuzzSeedJournals(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cached, report := readChecked(t, data, Options{CacheBytes: 1 << 20})
		if cached == nil {
			return
		}
		if cached.nul {
			t.Error("the text holds a NUL byte")
		}
		if cached.n > 0 && cached.last != '\n' {
			t.Error("the text does not end with a newline")
		}
		checkReport(t, report)
		uncached, reportUncached := readChecked(t, data, Options{CacheBytes: -1})
		if uncached == nil {
			t.Fatal("the header was read with the cache and refused without it")
		}
		if uncached.n != cached.n || uncached.hash.Sum64() != cached.hash.Sum64() || reportUncached != report {
			t.Errorf("without the cache: %d bytes, report %+v; with it: %d bytes, report %+v", uncached.n, reportUncached, cached.n, report)
		}
	})
}

// checkReport fails the test unless the report's counts add up and its
// first damaged entry and reason are set exactly when there is damage.
func checkReport(t *testing.T, r Report) {
	t.Helper()
	if r.Entries != r.Shown+r.WithoutMessage+r.LeftOut+r.NotReached {
		t.Errorf("counts do not add up: %+v", r)
	}
	for name, count := range map[string]int64{"shown": r.Shown, "without message": r.WithoutMessage, "left out": r.LeftOut, "not reached": r.NotReached} {
		if count < 0 {
			t.Errorf("%s is %d", name, count)
		}
	}
	damaged := r.LeftOut > 0 || r.NotReached > 0
	if r.Damaged() != damaged || (r.FirstDamaged > 0) != damaged || (r.DamageReason != "") != damaged {
		t.Errorf("damage reported inconsistently: %+v", r)
	}
	if r.FirstDamaged < 0 || r.FirstDamaged > r.Entries {
		t.Errorf("first damaged entry %d of %d", r.FirstDamaged, r.Entries)
	}
	if damaged && r.FirstDamaged > r.Shown+r.WithoutMessage+r.LeftOut+1 {
		t.Errorf("first damaged entry %d after the %d entries read: %s", r.FirstDamaged, r.Shown+r.WithoutMessage+r.LeftOut, fmt.Sprint(r))
	}
}
