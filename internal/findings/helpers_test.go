package findings

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/trace"
)

// The signatures of the tests below, one YAML definition each. Every
// built-in is disabled in the test catalogue, so the default set is
// exactly the definitions given without `default: false`.
const (
	defErrLine = `
  - id: err-line
    name: Error line
    group: severity
    description: A line that says ERROR.
    regex: 'ERROR'
`
	defWarnLine = `
  - id: warn-line
    name: Warning line
    group: severity
    description: A line that says WARN.
    regex: 'WARN'
`
	defTrace = `
  - id: py-trace
    name: Python traceback
    group: traceback
    description: A traceback with its frames.
    block:
      start: '^Traceback \(most recent call last\):'
      continue: '^\s'
      end: '^[A-Za-z]+Error: '
`
	defBlank = `
  - id: blank-lines
    name: Blank lines
    group: structure
    description: Two or more blank lines.
    run: {blank: true, min: 2}
`
	defKey = `
  - id: api-key
    name: API key
    group: secret
    description: An invented API key.
    redact: true
    regex: 'KEY-([A-Z0-9]{16})'
    capture: 1
`
	defBurst = `
  - id: line-burst
    name: Burst of lines
    group: time
    description: Many lines in one bucket.
    measure: {name: density}
`
	defNever = `
  - id: never-found
    name: Never found
    group: system
    description: A text no test line holds.
    regex: 'kernel panic'
`
	defOptIn = `
  - id: opt-in-sql
    name: SQL select
    group: sql
    description: A SELECT statement.
    default: false
    regex: '(?i)select\s'
`
)

// builtinIDs are the ids of the built-in signatures, which every test
// catalogue disables.
func builtinIDs(t testing.TB) []string {
	t.Helper()
	var ids []string
	for _, s := range signatures.Load(signatures.LoadOptions{Paths: []string{}}).Signatures() {
		ids = append(ids, s.ID)
	}
	return ids
}

// loadCatalogue loads the given definitions with every built-in
// disabled. Any load error fails the test.
func loadCatalogue(t testing.TB, defs ...string) *signatures.Catalogue {
	t.Helper()
	body := "disable: [" + strings.Join(builtinIDs(t), ", ") + "]\nsignatures:\n" + strings.Join(defs, "")
	path := filepath.Join(t.TempDir(), "team.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{path}})
	if errs := cat.Errors(); len(errs) > 0 {
		t.Fatalf("load errors: %v", errs)
	}
	return cat
}

// pick returns the catalogue's signatures with the given ids, in order.
func pick(t testing.TB, cat *signatures.Catalogue, ids ...string) []*signatures.Signature {
	t.Helper()
	out := make([]*signatures.Signature, 0, len(ids))
	for _, id := range ids {
		sig, ok := cat.Lookup(id)
		if !ok {
			t.Fatalf("no signature %q", id)
		}
		out = append(out, sig)
	}
	return out
}

// sampleLog is a log of about 200 KiB (more than the two 64 KiB windows
// of a fingerprint) with timestamps on most lines, errors, warnings, a
// traceback, blank runs and lines holding an invented API key.
func sampleLog() string {
	var b strings.Builder
	for i := range 3000 {
		ts := fmt.Sprintf("2026-10-10 14:%02d:%02d.%03d", (i/60)%60, i%60, i%1000)
		switch {
		case i%97 == 0:
			fmt.Fprintf(&b, "%s ERROR request %d failed: upstream timeout\n", ts, i)
		case i%89 == 0:
			fmt.Fprintf(&b, "%s WARN request %d slow\n", ts, i)
		case i%501 == 0:
			fmt.Fprintf(&b, "%s INFO token KEY-ABCDEFGHIJ%06d accepted for worker %d\n", ts, i, i)
		case i == 1500:
			b.WriteString("Traceback (most recent call last):\n  File \"app.py\", line 3, in <module>\nValueError: bad input\n")
		case i%700 == 0:
			b.WriteString("\n\n\n")
		default:
			fmt.Fprintf(&b, "%s INFO request %d served in %d ms by the application worker\n", ts, i, i%300)
		}
	}
	return b.String()
}

// writeLog writes text to name in dir and returns the path.
func writeLog(t testing.TB, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// appendLine appends one line to the file at path.
func appendLine(t testing.TB, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// classify pins and classifies path, as the commands do before a scan.
func classify(t testing.TB, path string) trace.SearchFile {
	t.Helper()
	pin, err := paths.Pin(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := trace.ClassifyForSearch(pin)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// testOptions are options with a fresh cache directory and two workers.
func testOptions(t testing.TB) ScanOptions {
	t.Helper()
	return ScanOptions{Dir: t.TempDir(), LogZone: "UTC", Scan: sigscan.Options{Workers: 2, LookbackBytes: 64 << 10}}
}

// recorder wraps sigscan.Scan for a scheduler: it records the ids of
// every scan it runs and the bytes those scans read, and counts the
// bytes the scheduler reads of the source file itself (the identity
// checks), through the wrapSource seam.
type recorder struct {
	mu          sync.Mutex
	scans       [][]string
	scanBytes   int64
	sourceBytes atomic.Int64
	// hold, when set, runs inside each scan before it reads the file
	// (tests that make two scans meet).
	hold func(ids []string)
}

// install sets the recorder's seams on s.
func (r *recorder) install(s *scheduler) {
	s.scanFile = r.scan
	s.wrapSource = func(ra io.ReaderAt) io.ReaderAt {
		return countInto{ReaderAt: ra, total: &r.sourceBytes}
	}
}

func (r *recorder) scan(ctx context.Context, file trace.SearchFile, sigs []*signatures.Signature, opts sigscan.Options) (*sigscan.Result, error) {
	ids := make([]string, len(sigs))
	for i, s := range sigs {
		ids[i] = s.ID
	}
	if r.hold != nil {
		r.hold(ids)
	}
	res, err := sigscan.Scan(ctx, file, sigs, opts)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scans = append(r.scans, ids)
	if res != nil {
		r.scanBytes += res.BytesRead
	}
	return res, err
}

// scanned returns the ids of every scan so far, and forgets them.
func (r *recorder) scanned() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.scans
	r.scans = nil
	return out
}

// countInto counts every byte read through it into a shared total.
type countInto struct {
	io.ReaderAt
	total *atomic.Int64
}

func (c countInto) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.ReaderAt.ReadAt(p, off)
	c.total.Add(int64(n))
	return n, err
}

// newRecordedScheduler returns a scheduler of maxScans slots with a
// recorder installed.
func newRecordedScheduler(maxScans int) (*scheduler, *recorder) {
	s := newScheduler(maxScans)
	r := &recorder{}
	r.install(s)
	return s, r
}

// requireSameFindings fails unless two answers hold equal findings,
// field by field (nil and empty told apart), for the same signatures,
// and the same masked ranges.
func requireSameFindings(t testing.TB, got, want *Answer) {
	t.Helper()
	if !slices.Equal(got.MaskedRanges, want.MaskedRanges) {
		t.Fatalf("masked ranges differ: got %v, want %v", got.MaskedRanges, want.MaskedRanges)
	}
	if !reflect.DeepEqual(got.BySignature, want.BySignature) {
		for id, w := range want.BySignature {
			if g := got.BySignature[id]; !reflect.DeepEqual(g, w) {
				t.Errorf("%s: got %+v\nwant %+v", id, g, w)
			}
		}
		t.Fatalf("findings differ: got ids %v, want %v", sortedKeys(got.BySignature), sortedKeys(want.BySignature))
	}
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// entryFiles lists the files of a cache directory, in name order.
func entryFiles(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// mustScan runs s.scan and fails the test on an error.
func mustScan(t testing.TB, s *scheduler, file trace.SearchFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts ScanOptions) *Answer {
	t.Helper()
	ans, err := s.scan(context.Background(), file, cat, sel, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ans
}

// coldAnswer scans text as a new file with a fresh cache and returns
// the answer: what any cached answer must equal.
func coldAnswer(t testing.TB, text string, cat *signatures.Catalogue, ids ...string) *Answer {
	t.Helper()
	path := writeLog(t, t.TempDir(), "app.log", text)
	s := newScheduler(1)
	return mustScan(t, s, classify(t, path), cat, pick(t, cat, ids...), testOptions(t))
}
