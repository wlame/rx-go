package findings

import (
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/trace"
)

// openCounter counts the cache files a scheduler opens, by name.
type openCounter struct {
	mu     sync.Mutex
	opened map[string]int
}

// install makes s open its cache files through the counter.
func (c *openCounter) install(s *scheduler) {
	c.opened = map[string]int{}
	s.openCache = func(root *os.Root, name string) (io.ReadCloser, error) {
		c.mu.Lock()
		c.opened[name]++
		c.mu.Unlock()
		return openCacheFile(root, name)
	}
}

// decoded counts the entry and ranges files opened since the last call,
// and forgets them.
func (c *openCounter) decoded() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for name, count := range c.opened {
		if name != metaName {
			n += count
		}
	}
	c.opened = map[string]int{}
	return n
}

// readCacheFixture is a scheduler with the read cache on, its counter,
// a catalogue and options.
type readCacheFixture struct {
	s     *scheduler
	opens *openCounter
	cat   *signatures.Catalogue
	opts  ScanOptions
}

func newReadCacheFixture(t *testing.T, files int, bytes int64) *readCacheFixture {
	t.Helper()
	f := &readCacheFixture{s: newScheduler(1), opens: &openCounter{}, cat: loadCatalogue(t, defErrLine, defWarnLine), opts: testOptions(t)}
	f.s.setReadCache(files, bytes)
	f.opens.install(f.s)
	return f
}

// scanned scans a new file of text for both signatures and returns it.
func (f *readCacheFixture) scanned(t *testing.T, name, text string) trace.SearchFile {
	t.Helper()
	file := classify(t, writeLog(t, t.TempDir(), name, text))
	mustScan(t, f.s, file, f.cat, pick(t, f.cat, "err-line", "warn-line"), f.opts)
	f.opens.decoded()
	return file
}

// read reads the file from the cache and returns the answer and how many
// entry and ranges files it opened.
func (f *readCacheFixture) read(t *testing.T, file trace.SearchFile) (*Answer, int) {
	t.Helper()
	ans, err := f.s.read(file, f.cat, pick(t, f.cat, "err-line", "warn-line"), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return ans, f.opens.decoded()
}

// A read answered from the read cache opens no entry or ranges file and
// gives the answer a read of the files gives; meta.json and the file's
// identity are still read each time, so a scan that changed the file's
// answer is a miss.
func TestReadCache_HitAndMissAfterARescan(t *testing.T) {
	f := newReadCacheFixture(t, 8, 64<<20)
	file := f.scanned(t, "app.log", "ERROR one\nWARN two\n")
	first, opened := f.read(t, file)
	if opened != 3 || len(first.Checked) != 2 {
		t.Fatalf("first read: %d files opened (want two entries and the ranges), %+v", opened, first)
	}
	second, opened := f.read(t, file)
	if opened != 0 || !reflect.DeepEqual(first, second) {
		t.Fatalf("second read: %d files opened, equal %v", opened, reflect.DeepEqual(first, second))
	}

	// The file grows and is scanned again: its identity changed, so the
	// next read decodes the new entries.
	appendLine(t, file.Path(), "ERROR three")
	file = classify(t, file.Path())
	mustScan(t, f.s, file, f.cat, pick(t, f.cat, "err-line", "warn-line"), f.opts)
	f.opens.decoded()
	third, opened := f.read(t, file)
	if opened != 3 || third.BySignature["err-line"].Count != 2 {
		t.Fatalf("read after the rescan: %d files opened, err-line count %d", opened, third.BySignature["err-line"].Count)
	}
	if _, opened := f.read(t, file); opened != 0 {
		t.Fatalf("read after that: %d files opened", opened)
	}

	// A signature changes and the file is scanned again: same file and
	// as many findings, a new key for err-line (its value is now a
	// capture group), so its entry is decoded again; warn-line's is
	// still held.
	f.cat = loadCatalogue(t, strings.Replace(defErrLine, "regex: 'ERROR'", "regex: 'ERR(OR)'\n    capture: 1", 1), defWarnLine)
	mustScan(t, f.s, file, f.cat, pick(t, f.cat, "err-line", "warn-line"), f.opts)
	f.opens.decoded()
	fourth, opened := f.read(t, file)
	errs := fourth.BySignature["err-line"].Findings
	if opened != 1 || len(errs) != 2 || errs[0].ValueStart == nil || *errs[0].ValueStart != 3 {
		t.Fatalf("read after the signature changed: %d files opened, err-line %+v", opened, errs)
	}
}

// The read cache holds at most its number of files, the least recently
// read one out first, and no answer past its byte cap.
func TestReadCache_EvictsTheLeastRecentlyReadFile(t *testing.T) {
	f := newReadCacheFixture(t, 2, 64<<20)
	a := f.scanned(t, "a.log", "ERROR a\n")
	b := f.scanned(t, "b.log", "ERROR b\n")
	c := f.scanned(t, "c.log", "ERROR c\n")
	for _, file := range []trace.SearchFile{a, b, c} {
		if _, opened := f.read(t, file); opened == 0 {
			t.Fatalf("%s: a first read opened nothing", file.Path())
		}
	}
	// a was read least recently: out. b and c are in.
	if _, opened := f.read(t, c); opened != 0 {
		t.Fatalf("c: %d files opened, want a hit", opened)
	}
	if _, opened := f.read(t, a); opened == 0 {
		t.Fatal("a was not evicted")
	}
	// Reading a again pushed out b, the least recently read now.
	if _, opened := f.read(t, b); opened == 0 {
		t.Fatal("b was not evicted")
	}

	small := newReadCacheFixture(t, 8, 1)
	file := small.scanned(t, "big.log", strings.Repeat("ERROR x\n", 50))
	small.read(t, file)
	if _, opened := small.read(t, file); opened == 0 {
		t.Fatal("an answer over the byte cap was kept")
	}
}

// Without the read cache (the CLI, and every scheduler until it is
// turned on) every read decodes the files.
func TestReadCache_OffByDefault(t *testing.T) {
	f := newReadCacheFixture(t, 0, 0)
	file := f.scanned(t, "app.log", "ERROR one\n")
	f.read(t, file)
	if _, opened := f.read(t, file); opened == 0 {
		t.Fatal("a read was answered from a cache that is off")
	}
}
