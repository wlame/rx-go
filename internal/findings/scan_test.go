package findings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// allDefs are the signatures of the cache tests: every kind, a measure
// with a series, a redacting signature, one that finds nothing and one
// opt-in.
var allDefs = []string{defErrLine, defWarnLine, defTrace, defBlank, defKey, defBurst, defNever, defOptIn}

// A second scan of the same selection is answered from the cache: equal
// findings field by field (times, excerpts, value spans, masks and the
// density series), no scan, and no read of the file beyond the two
// 64 KiB windows of the identity's fingerprint.
func TestScan_ColdThenWarmGiveEqualAnswers(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	path := writeLog(t, t.TempDir(), "app.log", sampleLog())
	file := classify(t, path)
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	ids := []string{"err-line", "py-trace", "blank-lines", "api-key", "line-burst", "never-found"}

	cold := mustScan(t, s, file, cat, pick(t, cat, ids...), opts)
	if got := rec.scanned(); len(got) != 1 {
		t.Fatalf("cold scans %v, want one", got)
	}
	coldSource := rec.sourceBytes.Swap(0)

	warm := mustScan(t, s, file, cat, pick(t, cat, ids...), opts)
	if got := rec.scanned(); len(got) != 0 {
		t.Fatalf("warm call scanned %v", got)
	}
	requireSameFindings(t, warm, cold)
	for id, at := range cold.ScannedAt {
		if !warm.ScannedAt[id].Equal(at) {
			t.Errorf("%s scanned_at: warm %v, cold %v", id, warm.ScannedAt[id], at)
		}
	}
	// The identity reads the head and the tail window of the file once;
	// the cold call read them twice (before and after the scan).
	const fingerprintBytes = 2 * 64 << 10
	if got := rec.sourceBytes.Load(); got != fingerprintBytes {
		t.Errorf("warm call read %d bytes of the file, want the %d of the fingerprint", got, fingerprintBytes)
	}
	if coldSource != 2*fingerprintBytes {
		t.Errorf("cold call read %d bytes outside the scan, want %d", coldSource, 2*fingerprintBytes)
	}
	// The fixture exercises what the cache must keep exactly.
	key := cold.BySignature["api-key"]
	if len(key.Findings) == 0 || !key.Findings[0].Masked || key.Findings[0].ValueStart == nil || key.Findings[0].TimeMS == nil {
		t.Errorf("api-key findings lack a mask, a value span or a time: %+v", key.Findings)
	}
	if strings.Contains(key.Findings[0].Excerpt, "ABCDEFGHIJ000501") {
		t.Errorf("the excerpt holds the key: %q", key.Findings[0].Excerpt)
	}
	if cold.BySignature["line-burst"].Series == nil {
		t.Error("the density series is missing")
	}
	if n := cold.BySignature["never-found"]; n.Count != 0 || n.Findings == nil {
		t.Errorf("never-found: %+v", n)
	}
	if !slices.Equal(warm.Checked, ids) || len(warm.Missing) != 0 {
		t.Errorf("checked %v missing %v", warm.Checked, warm.Missing)
	}
}

// {A, B} then {B, C}: the second scan runs C alone, and its answer is a
// cold scan's.
func TestScan_SecondSelectionScansOnlyWhatIsMissing(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()
	file := classify(t, writeLog(t, t.TempDir(), "app.log", text))
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)

	mustScan(t, s, file, cat, pick(t, cat, "err-line", "py-trace"), opts)
	rec.scanned()
	got := mustScan(t, s, file, cat, pick(t, cat, "py-trace", "api-key"), opts)
	if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{{"api-key"}}) {
		t.Fatalf("second call scanned %v, want [[api-key]]", scans)
	}
	requireSameFindings(t, got, coldAnswer(t, text, cat, "py-trace", "api-key"))
}

// A line appended to the file makes every entry stale: the next scan
// rescans the whole selection.
func TestScan_FileChangedMakesEveryEntryStale(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()
	path := writeLog(t, t.TempDir(), "app.log", text)
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	ids := []string{"err-line", "blank-lines"}

	mustScan(t, s, classify(t, path), cat, pick(t, cat, ids...), opts)
	rec.scanned()
	line := "2026-10-10 15:00:00.000 ERROR request 9999 failed"
	appendLine(t, path, line)
	got := mustScan(t, s, classify(t, path), cat, pick(t, cat, ids...), opts)
	if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{ids}) {
		t.Fatalf("after the change scanned %v, want %v", scans, [][]string{ids})
	}
	requireSameFindings(t, got, coldAnswer(t, text+line+"\n", cat, ids...))
}

// A changed regex makes only its signature's entry stale; a changed
// description changes nothing.
func TestScan_ChangedRegexRescansOnlyThatSignature(t *testing.T) {
	text := sampleLog()
	path := writeLog(t, t.TempDir(), "app.log", text)
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	before := loadCatalogue(t, defErrLine, defWarnLine)
	mustScan(t, s, classify(t, path), before, pick(t, before, "err-line", "warn-line"), opts)
	rec.scanned()

	described := loadCatalogue(t, defErrLine, strings.Replace(defWarnLine, "A line that says WARN.", "Any warning.", 1))
	mustScan(t, s, classify(t, path), described, pick(t, described, "err-line", "warn-line"), opts)
	if scans := rec.scanned(); len(scans) != 0 {
		t.Fatalf("a new description rescanned %v", scans)
	}

	changed := loadCatalogue(t, strings.Replace(defErrLine, "'ERROR'", "'ERROR|FATAL'", 1), defWarnLine)
	got := mustScan(t, s, classify(t, path), changed, pick(t, changed, "err-line", "warn-line"), opts)
	if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{{"err-line"}}) {
		t.Fatalf("a new regex rescanned %v, want [[err-line]]", scans)
	}
	requireSameFindings(t, got, coldAnswer(t, text, changed, "err-line", "warn-line"))
}

// The state of a file: none before any scan, partial with only an
// opt-in entry or with part of the default set, full with all of the
// default set. A signature with no findings is an entry.
func TestScan_StateFullPartialNone(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defNever, defOptIn)
	path := writeLog(t, t.TempDir(), "app.log", sampleLog())
	file := classify(t, path)
	s := newScheduler(1)
	opts := testOptions(t)
	pin := file.Pinned()

	read, err := s.read(file, cat, pick(t, cat, "err-line"), opts)
	if err != nil || read.State != StateNone || !slices.Equal(read.Missing, []string{"err-line"}) {
		t.Fatalf("before any scan: %+v, %v", read, err)
	}
	if sum, err := s.summary(pin, cat, opts); err != nil || sum != nil {
		t.Fatalf("summary before any scan: %+v, %v", sum, err)
	}

	if got := mustScan(t, s, file, cat, pick(t, cat, "opt-in-sql"), opts); got.State != StatePartial {
		t.Errorf("only the opt-in signature: %s, want partial", got.State)
	}
	if got := mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts); got.State != StatePartial {
		t.Errorf("one of two default signatures: %s, want partial", got.State)
	}
	got := mustScan(t, s, file, cat, pick(t, cat, "never-found"), opts)
	if got.State != StateFull {
		t.Errorf("every default signature, never-found with no findings: %s, want full", got.State)
	}
	sum, err := s.summary(pin, cat, opts)
	if err != nil || sum == nil {
		t.Fatalf("summary: %+v, %v", sum, err)
	}
	all := mustScan(t, s, file, cat, pick(t, cat, "err-line", "never-found", "opt-in-sql"), opts)
	want := all.BySignature["err-line"].Count + all.BySignature["opt-in-sql"].Count
	if sum.State != StateFull || sum.Count != want {
		t.Errorf("summary %+v, want full with %d findings", sum, want)
	}
}

// A file with a current entry for every signature of the default set is
// full though no opt-in signature was scanned: a missing opt-in
// signature never keeps a file from full.
func TestScan_DefaultSetWithoutOptInIsFull(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defNever, defOptIn)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	s := newScheduler(1)
	opts := testOptions(t)
	got := mustScan(t, s, file, cat, pick(t, cat, "err-line", "never-found"), opts)
	if got.State != StateFull {
		t.Errorf("state %s, want full", got.State)
	}
	sum, err := s.summary(file.Pinned(), cat, opts)
	if err != nil || sum == nil || sum.State != StateFull || sum.Count != got.BySignature["err-line"].Count {
		t.Errorf("summary %+v (%v), want full with err-line's count", sum, err)
	}
	read, err := s.read(file, cat, pick(t, cat, "opt-in-sql"), opts)
	if err != nil || read.State != StateFull || !slices.Equal(read.Missing, []string{"opt-in-sql"}) {
		t.Errorf("read of the opt-in signature: %+v (%v), want full with it missing", read, err)
	}
}

// A crash after the entry files and before meta.json leaves the old
// answer readable, and the next write removes the file meta.json does
// not name.
func TestScan_CrashBeforeMetaKeepsTheOldAnswer(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	old := mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)

	crash := errors.New("crash")
	s.beforeMeta = func() error { return crash }
	mustScan(t, s, file, cat, pick(t, cat, "warn-line"), opts)
	forced := opts
	forced.Force = true
	mustScan(t, s, file, cat, pick(t, cat, "err-line"), forced)
	s.beforeMeta = nil
	dir := cacheDir(opts.Dir, file.Path())
	if names := entryFiles(t, dir); !slices.Contains(names, "warn-line.json") {
		t.Fatalf("the crash left no orphan: %v", names)
	}

	read, err := s.read(file, cat, pick(t, cat, "err-line", "warn-line"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(read.Checked, []string{"err-line"}) || !slices.Equal(read.Missing, []string{"warn-line"}) {
		t.Fatalf("after the crash: checked %v missing %v", read.Checked, read.Missing)
	}
	requireSameFindings(t, read, old)

	rec.scanned()
	mustScan(t, s, file, cat, pick(t, cat, "blank-lines"), opts)
	if names := entryFiles(t, dir); slices.Contains(names, "warn-line.json") {
		t.Errorf("the next write kept the orphan: %v", names)
	}
}

// Two processes (two schedulers, each with its own in-process locks)
// that write different signatures of one file at once keep both.
func TestScan_TwoProcessesWritingDifferentSignaturesKeepBoth(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	opts := testOptions(t)
	first, rec1 := newRecordedScheduler(1)
	second, rec2 := newRecordedScheduler(1)
	// Both scans wait for each other, so they end, and write, together.
	var meet sync.WaitGroup
	meet.Add(2)
	rec1.hold = func([]string) { meet.Done(); meet.Wait() }
	rec2.hold = rec1.hold

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, run := range []struct {
		s  *scheduler
		id string
	}{{first, "err-line"}, {second, "warn-line"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = run.s.scan(context.Background(), file, cat, pick(t, cat, run.id), opts)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	read, err := newScheduler(1).read(file, cat, pick(t, cat, "err-line", "warn-line"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(read.Checked, []string{"err-line", "warn-line"}) {
		t.Fatalf("checked %v, missing %v", read.Checked, read.Missing)
	}
}

// Another process that commits between this scan's read of meta.json
// and its own write is kept: the writer reads meta.json again.
func TestScan_KeepsAnotherWritersEntriesCommittedDuringTheScan(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	opts := testOptions(t)
	first := newScheduler(1)
	other := newScheduler(1)
	first.afterScan = func() {
		mustScan(t, other, file, cat, pick(t, cat, "warn-line"), opts)
	}
	mustScan(t, first, file, cat, pick(t, cat, "err-line"), opts)
	read, err := newScheduler(1).read(file, cat, pick(t, cat, "err-line", "warn-line"), opts)
	if err != nil || !slices.Equal(read.Checked, []string{"err-line", "warn-line"}) {
		t.Fatalf("checked %v missing %v (%v)", read.Checked, read.Missing, err)
	}
}

// Within a process a second caller for the same file waits for the
// first, then scans only what is still missing.
func TestScan_SecondCallerForOneFileWaitsThenScansWhatIsMissing(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	opts := testOptions(t)
	s, rec := newRecordedScheduler(4)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rec.hold = func([]string) {
		once.Do(func() {
			close(started)
			<-release
		})
	}
	done := make(chan error, 2)
	go func() {
		_, err := s.scan(context.Background(), file, cat, pick(t, cat, "err-line"), opts)
		done <- err
	}()
	<-started
	waiting := make(chan struct{})
	s.files.onWait = func() { close(waiting) }
	go func() {
		_, err := s.scan(context.Background(), file, cat, pick(t, cat, "err-line", "warn-line"), opts)
		done <- err
	}()
	<-waiting
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{{"err-line"}, {"warn-line"}}) {
		t.Fatalf("scans %v, want [[err-line] [warn-line]]", scans)
	}
}

// A file that changes after the scan read it is answered, and nothing
// is cached.
func TestScan_FileChangedDuringTheScanIsAnsweredAndNotCached(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()
	path := writeLog(t, t.TempDir(), "app.log", text)
	file := classify(t, path)
	s := newScheduler(1)
	opts := testOptions(t)
	s.afterScan = func() { appendLine(t, path, "2026-10-10 15:00:00.000 ERROR late") }

	got := mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)
	requireSameFindings(t, got, coldAnswer(t, text, cat, "err-line"))
	if names := entryFiles(t, cacheDir(opts.Dir, path)); slices.Contains(names, metaName) || slices.Contains(names, "err-line.json") {
		t.Errorf("a changed file was cached: %v", names)
	}
}

// Force rescans and rewrites; NoCache and Spooled read and write
// nothing; a canceled scan writes nothing.
func TestScan_ForceNoCacheSpooledAndCancel(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()

	t.Run("force", func(t *testing.T) {
		file := classify(t, writeLog(t, t.TempDir(), "app.log", text))
		s, rec := newRecordedScheduler(1)
		opts := testOptions(t)
		mustScan(t, s, file, cat, pick(t, cat, "err-line", "warn-line"), opts)
		rec.scanned()
		opts.Force = true
		got := mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)
		if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{{"err-line"}}) {
			t.Fatalf("force scanned %v", scans)
		}
		requireSameFindings(t, got, coldAnswer(t, text, cat, "err-line"))
		opts.Force = false
		read, err := s.read(file, cat, pick(t, cat, "err-line", "warn-line"), opts)
		if err != nil || len(read.Missing) != 0 {
			t.Fatalf("after force: missing %v (%v)", read.Missing, err)
		}
	})

	for _, mode := range []string{"no-cache", "spooled"} {
		t.Run(mode, func(t *testing.T) {
			file := classify(t, writeLog(t, t.TempDir(), "app.log", text))
			s, rec := newRecordedScheduler(1)
			opts := testOptions(t)
			mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)
			rec.scanned()
			opts.NoCache, opts.Spooled = mode == "no-cache", mode == "spooled"
			got := mustScan(t, s, file, cat, pick(t, cat, "err-line", "warn-line"), opts)
			if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{{"err-line", "warn-line"}}) {
				t.Fatalf("%s scanned %v, want both (the cache is not read)", mode, scans)
			}
			requireSameFindings(t, got, coldAnswer(t, text, cat, "err-line", "warn-line"))
			if got.State != StatePartial {
				t.Errorf("state %s, want partial (two of the default set)", got.State)
			}
			opts.NoCache, opts.Spooled = false, false
			read, err := s.read(file, cat, pick(t, cat, "warn-line"), opts)
			if err != nil || len(read.Checked) != 0 {
				t.Fatalf("%s wrote warn-line: %+v (%v)", mode, read, err)
			}
		})
	}

	t.Run("cancel", func(t *testing.T) {
		path := writeLog(t, t.TempDir(), "app.log", text)
		file := classify(t, path)
		s, rec := newRecordedScheduler(1)
		opts := testOptions(t)
		ctx, cancel := context.WithCancel(context.Background())
		rec.hold = func([]string) { cancel() }
		if _, err := s.scan(ctx, file, cat, pick(t, cat, "err-line"), opts); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled scan: %v", err)
		}
		if names := entryFiles(t, cacheDir(opts.Dir, path)); len(names) > 0 {
			t.Errorf("a canceled scan wrote %v", names)
		}
	})

	t.Run("cancel after the scan", func(t *testing.T) {
		path := writeLog(t, t.TempDir(), "app.log", text)
		s := newScheduler(1)
		opts := testOptions(t)
		ctx, cancel := context.WithCancel(context.Background())
		s.afterScan = cancel
		if _, err := s.scan(ctx, classify(t, path), cat, pick(t, cat, "err-line"), opts); !errors.Is(err, context.Canceled) {
			t.Fatalf("scan canceled after its read: %v", err)
		}
		if names := entryFiles(t, cacheDir(opts.Dir, path)); len(names) > 0 {
			t.Errorf("a canceled scan wrote %v", names)
		}
	})
}

// A file truncated while it is scanned (copytruncate) ends the scan
// with an error that names it, or with what it read; either way no
// offset lies past the new end and nothing is cached, and the next scan
// of the new file starts clean.
func TestScan_FileTruncatedDuringTheScan(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	// Several chunks for one worker, so reads remain after the cut.
	var b strings.Builder
	for b.Len() < 12<<20 {
		b.WriteString(sampleLog())
	}
	text := b.String()
	path := writeLog(t, t.TempDir(), "app.log", text)
	file := classify(t, path)
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	opts.Scan.Workers = 1
	half := int64(len(text) / 2)
	var once sync.Once
	opts.Progress = func(read, total int64) {
		once.Do(func() {
			if err := os.Truncate(path, half); err != nil {
				t.Error(err)
			}
		})
	}
	ids := []string{"err-line", "blank-lines"}
	ans, err := s.scan(context.Background(), file, cat, pick(t, cat, ids...), opts)
	switch {
	case err != nil:
		if !strings.Contains(err.Error(), path) {
			t.Errorf("the error does not name the file: %v", err)
		}
	default:
		for id, r := range ans.BySignature {
			for _, f := range r.Findings {
				if f.EndOffset > half {
					t.Errorf("%s: offset %d past the new end %d", id, f.EndOffset, half)
				}
			}
		}
	}
	dir := cacheDir(opts.Dir, path)
	if names := entryFiles(t, dir); slices.Contains(names, metaName) {
		t.Errorf("a truncated scan was cached: %v", names)
	}

	rec.scanned()
	opts.Progress = nil
	got := mustScan(t, s, classify(t, path), cat, pick(t, cat, ids...), opts)
	if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{ids}) {
		t.Fatalf("the next scan ran %v, want %v", scans, [][]string{ids})
	}
	requireSameFindings(t, got, coldAnswer(t, text[:half], cat, ids...))
}

// A file truncated after the scan read it (and before the end check)
// ends the scan with an error that names it: the answer could hold
// offsets past the new end. Nothing is cached.
func TestScan_FileTruncatedAfterTheRead(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()
	path := writeLog(t, t.TempDir(), "app.log", text)
	s := newScheduler(1)
	opts := testOptions(t)
	s.afterScan = func() {
		if err := os.Truncate(path, int64(len(text)/2)); err != nil {
			t.Error(err)
		}
	}
	_, err := s.scan(context.Background(), classify(t, path), cat, pick(t, cat, "err-line"), opts)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "shorter") {
		t.Fatalf("got %v, want an error naming the file", err)
	}
	if names := entryFiles(t, cacheDir(opts.Dir, path)); slices.Contains(names, metaName) {
		t.Errorf("a truncated file was cached: %v", names)
	}
}

// Every excerpt is masked by every redacting signature of the
// catalogue, selected or not, in the answer and in the cache.
func TestScan_MasksEveryExcerptWithEveryRedactingSignature(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defKey)
	secret := "KEY-ABCDEFGHIJ123456"
	path := writeLog(t, t.TempDir(), "app.log", "INFO start\nERROR token "+secret+" refused\n")
	s := newScheduler(1)
	opts := testOptions(t)
	got := mustScan(t, s, classify(t, path), cat, pick(t, cat, "err-line"), opts)
	f := got.BySignature["err-line"].Findings
	if len(f) != 1 || !f[0].Masked || strings.Contains(f[0].Excerpt, "ABCDEFGHIJ123456") {
		t.Fatalf("err-line findings %+v: the key is not masked", f)
	}
	data, err := os.ReadFile(filepath.Join(cacheDir(opts.Dir, path), "err-line.json"))
	if err != nil || strings.Contains(string(data), "ABCDEFGHIJ123456") {
		t.Errorf("the cache holds the key (%v): %s", err, data)
	}
}

// A signature whose id is "meta" gets an entry file that does not
// overwrite meta.json.
func TestScan_SignatureNamedMeta(t *testing.T) {
	cat := loadCatalogue(t, strings.Replace(defErrLine, "id: err-line", "id: meta", 1), defWarnLine)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	cold := mustScan(t, s, file, cat, pick(t, cat, "meta", "warn-line"), opts)
	rec.scanned()
	warm := mustScan(t, s, file, cat, pick(t, cat, "meta", "warn-line"), opts)
	if scans := rec.scanned(); len(scans) != 0 {
		t.Fatalf("warm call scanned %v", scans)
	}
	requireSameFindings(t, warm, cold)
	names := entryFiles(t, cacheDir(opts.Dir, file.Path()))
	if !slices.Contains(names, metaEntryName) || !slices.Contains(names, metaName) {
		t.Errorf("files %v", names)
	}
}

// Read answers from the cache alone and never scans; a signature the
// cache does not hold is missing.
func TestRead_AnswersFromTheCacheOnly(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	scanned := mustScan(t, s, file, cat, pick(t, cat, "err-line", "py-trace"), opts)
	rec.scanned()

	read, err := s.read(file, cat, pick(t, cat, "py-trace", "warn-line", "err-line"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if scans := rec.scanned(); len(scans) != 0 {
		t.Fatalf("read scanned %v", scans)
	}
	if !slices.Equal(read.Checked, []string{"py-trace", "err-line"}) || !slices.Equal(read.Missing, []string{"warn-line"}) {
		t.Fatalf("checked %v missing %v", read.Checked, read.Missing)
	}
	requireSameFindings(t, read, scanned)
	if read.State != StatePartial {
		t.Errorf("state %s", read.State)
	}

	appendLine(t, file.Path(), "one more line")
	stale, err := s.read(classify(t, file.Path()), cat, pick(t, cat, "err-line"), opts)
	if err != nil || stale.State != StateNone || len(stale.Checked) != 0 {
		t.Errorf("read of a changed file: %+v (%v)", stale, err)
	}
}

// An entry file that is damaged, or whose key differs from the one
// meta.json names, counts as missing and is scanned again.
func TestScan_DamagedEntryIsMissing(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	text := sampleLog()
	file := classify(t, writeLog(t, t.TempDir(), "app.log", text))
	opts := testOptions(t)
	dir := cacheDir(opts.Dir, file.Path())
	for name, damage := range map[string]func(string){
		"truncated": func(p string) { _ = os.Truncate(p, 10) },
		"other key": func(p string) {
			data, _ := os.ReadFile(p)
			var e map[string]any
			_ = json.Unmarshal(data, &e)
			e["key"] = strings.Repeat("0", 64)
			data, _ = json.Marshal(e)
			_ = os.WriteFile(p, data, 0o600)
		},
		"removed": func(p string) { _ = os.Remove(p) },
	} {
		t.Run(name, func(t *testing.T) {
			s, rec := newRecordedScheduler(1)
			mustScan(t, s, file, cat, pick(t, cat, "err-line", "warn-line"), opts)
			rec.scanned()
			damage(filepath.Join(dir, "err-line.json"))
			got := mustScan(t, s, file, cat, pick(t, cat, "err-line", "warn-line"), opts)
			if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{{"err-line"}}) {
				t.Fatalf("scanned %v, want [[err-line]]", scans)
			}
			requireSameFindings(t, got, coldAnswer(t, text, cat, "err-line", "warn-line"))
		})
	}
}

// The directory and the files have the layout and the fields the
// documentation describes.
func TestScan_CacheLayout(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	path := writeLog(t, t.TempDir(), "app server.log", sampleLog())
	file := classify(t, path)
	s := newScheduler(1)
	opts := testOptions(t)
	mustScan(t, s, file, cat, pick(t, cat, "err-line", "line-burst"), opts)

	dirs := entryFiles(t, opts.Dir)
	if len(dirs) != 1 || !strings.HasPrefix(dirs[0], "app_server.log_") || len(dirs[0]) != len("app_server.log_")+16 {
		t.Fatalf("cache directories %v", dirs)
	}
	dir := filepath.Join(opts.Dir, dirs[0])
	if names := entryFiles(t, dir); !slices.Equal(names, []string{".lock", "err-line.json", "line-burst.json", "meta.json"}) {
		t.Fatalf("files %v", names)
	}
	var meta map[string]any
	readJSONFile(t, filepath.Join(dir, "meta.json"), &meta)
	for _, field := range []string{"version", "source_path", "source_size_bytes", "source_mtime_ns", "source_inode",
		"source_device", "source_ctime_ns", "source_fingerprint", "signatures"} {
		if _, ok := meta[field]; !ok {
			t.Errorf("meta.json lacks %s: %v", field, meta)
		}
	}
	if meta["version"] != float64(CacheVersion) {
		t.Errorf("version %v", meta["version"])
	}
	entry := meta["signatures"].(map[string]any)["err-line"].(map[string]any)
	for _, field := range []string{"key", "count", "stored", "scanned_at"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("meta entry lacks %s: %v", field, entry)
		}
	}
	if _, err := time.Parse(time.RFC3339, entry["scanned_at"].(string)); err != nil {
		t.Errorf("scanned_at: %v", err)
	}
	var sig map[string]any
	readJSONFile(t, filepath.Join(dir, "err-line.json"), &sig)
	if sig["key"] != entry["key"] || sig["series"] != nil {
		t.Errorf("entry key %v series %v", sig["key"], sig["series"])
	}
	finding := sig["findings"].([]any)[0].(map[string]any)
	for _, field := range []string{"start_line", "end_line", "start_offset", "end_offset", "time_ms", "excerpt", "value_start", "value_end", "masked"} {
		if _, ok := finding[field]; !ok {
			t.Errorf("finding lacks %s: %v", field, finding)
		}
	}
	var burst map[string]any
	readJSONFile(t, filepath.Join(dir, "line-burst.json"), &burst)
	series, ok := burst["series"].(map[string]any)
	if !ok || series["bucket_ms"] == nil || series["start_ms"] == nil || series["counts"] == nil {
		t.Errorf("series %v", burst["series"])
	}
}

// readJSONFile decodes the JSON file at path into v.
func readJSONFile(t testing.TB, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

// Summary reads meta.json and nothing else: no entry file and no byte
// of the scanned file. A file changed since is stale.
func TestSummary_ReadsOnlyMetaJSON(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defWarnLine)
	path := writeLog(t, t.TempDir(), "app.log", sampleLog())
	file := classify(t, path)
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	ans := mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)
	rec.sourceBytes.Store(0)

	var opened []string
	s.openCache = func(root *os.Root, name string) (io.ReadCloser, error) {
		opened = append(opened, name)
		return openCacheFile(root, name)
	}
	sum, err := s.summary(file.Pinned(), cat, opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := (&FileSummary{State: StatePartial, Count: ans.BySignature["err-line"].Count}); !reflect.DeepEqual(sum, want) {
		t.Errorf("summary %+v, want %+v", sum, want)
	}
	if !slices.Equal(opened, []string{metaName}) || rec.sourceBytes.Load() != 0 {
		t.Errorf("summary opened %v and read %d bytes of the file", opened, rec.sourceBytes.Load())
	}

	appendLine(t, path, "one more line")
	stale, err := s.summary(classify(t, path).Pinned(), cat, opts)
	if err != nil || stale != nil {
		t.Errorf("summary of a changed file: %+v, %v", stale, err)
	}
}

// A signature given twice is scanned and answered once.
func TestScan_DuplicateSelection(t *testing.T) {
	cat := loadCatalogue(t, allDefs...)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	s, rec := newRecordedScheduler(1)
	got := mustScan(t, s, file, cat, pick(t, cat, "err-line", "err-line"), testOptions(t))
	if scans := rec.scanned(); !reflect.DeepEqual(scans, [][]string{{"err-line"}}) || !slices.Equal(got.Checked, []string{"err-line"}) {
		t.Errorf("scans %v checked %v", scans, got.Checked)
	}
}

// No cache directory is a programming error the call reports.
func TestScan_WithoutACacheDirectory(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", sampleLog()))
	opts := testOptions(t)
	opts.Dir = ""
	if _, err := newScheduler(1).scan(context.Background(), file, cat, pick(t, cat, "err-line"), opts); err == nil {
		t.Error("scan without a cache directory succeeded")
	}
	opts.NoCache = true
	if _, err := newScheduler(1).scan(context.Background(), file, cat, pick(t, cat, "err-line"), opts); err != nil {
		t.Errorf("a scan that uses no cache needs no directory: %v", err)
	}
}
