package index

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// writeNumberedFile writes lines that name their own line number, so a
// test can tell which line an offset really belongs to.
func writeNumberedFile(t *testing.T, path string, lines int) {
	t.Helper()
	var buf []byte
	for i := 1; i <= lines; i++ {
		buf = append(buf, []byte("LINE ")...)
		buf = append(buf, []byte(itoa(i))...)
		buf = append(buf, []byte(" padding to give the line some width\n")...)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// buildIndexFor builds and saves an index the way `rx index` would.
func buildIndexFor(t *testing.T, path string) *rxtypes.UnifiedFileIndex {
	t.Helper()
	idx, err := Build(path, BuildOptions{})
	if err != nil {
		t.Fatalf("Build(%s): %v", path, err)
	}
	if _, err := Save(idx); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return idx
}

// TestLoadRefusesAnIndexWrittenByAnotherVersion is the regression test
// for an index left behind by an older rx being read with today's rules.
// Version 2 checkpoints named the line before the recorded offset, so
// trusting one returned a line number that was off by one.
func TestLoadRefusesAnIndexWrittenByAnotherVersion(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	writeNumberedFile(t, src, 200)
	idx := buildIndexFor(t, src)

	for _, version := range []int{0, 1, 2, 3, 4, 6, 999} {
		idx.Version = version
		raw, err := json.Marshal(idx)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(GetCachePath(src), raw, 0o600); err != nil {
			t.Fatalf("write cache: %v", err)
		}

		got, err := Load(src)
		if !errors.Is(err, ErrIndexNotFound) {
			t.Errorf("version %d: Load err = %v, want ErrIndexNotFound", version, err)
		}
		if got != nil {
			t.Errorf("version %d: Load returned an index", version)
		}
		if _, err := LoadForSource(src); !errors.Is(err, ErrIndexNotFound) {
			t.Errorf("version %d: LoadForSource err = %v, want ErrIndexNotFound", version, err)
		}
	}
}

// TestLoadAcceptsTheCurrentVersion is the other half of the gate: the
// version check must not reject an index rx just wrote.
func TestLoadAcceptsTheCurrentVersion(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	writeNumberedFile(t, src, 200)
	buildIndexFor(t, src)

	got, err := LoadForSource(src)
	if err != nil {
		t.Fatalf("LoadForSource: %v", err)
	}
	if got == nil {
		t.Fatal("LoadForSource returned no index for a freshly built cache")
	}
	if got.Version != Version {
		t.Errorf("Version = %d, want %d", got.Version, Version)
	}
}

// TestIndexRecordsSourceIdentity checks that a fresh index carries the
// inode and ctime the staleness check depends on.
func TestIndexRecordsSourceIdentity(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	writeNumberedFile(t, src, 50)
	idx := buildIndexFor(t, src)

	if idx.SourceInode == nil {
		t.Error("source_inode was not recorded")
	}
	if idx.SourceChangedAt == nil {
		t.Error("source_changed_at was not recorded")
	}
}

// TestIndexGoesStaleWhenTheFileIsRewrittenInPlace is the regression test
// for the case size and mtime cannot see: the file keeps its byte count,
// the mtime is put back, and only the content moved. Before the inode
// and ctime checks, `rx samples` answered from the old checkpoints and
// named the wrong line.
func TestIndexGoesStaleWhenTheFileIsRewrittenInPlace(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	writeNumberedFile(t, src, 5000)

	before, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	idx := buildIndexFor(t, src)
	if !IsValidForSource(idx, src) {
		t.Fatal("a freshly built index is already stale")
	}

	// Turn one space into a newline. The file keeps its size; every
	// later line number shifts by one.
	raw, err := os.ReadFile(src) //nolint:gosec // fixture path from t.TempDir
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	raw[len("LINE 1")] = '\n'
	if err := os.WriteFile(src, raw, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(src, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}

	after, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("the rewrite changed the size (%d -> %d); the test proves nothing",
			before.Size(), after.Size())
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the rewrite changed the mtime; the test proves nothing")
	}

	if IsValidForSource(idx, src) {
		t.Error("the index still looks valid after the file was rewritten in place")
	}
	got, err := LoadForSource(src)
	if err != nil {
		t.Fatalf("LoadForSource: %v", err)
	}
	if got != nil {
		t.Error("LoadForSource handed back a stale index")
	}
}

// TestIndexGoesStaleWhenTheFileIsReplacedByRename covers the other way a
// file changes without its size or mtime moving: a new file is renamed
// over the old path, so the inode changes.
func TestIndexGoesStaleWhenTheFileIsReplacedByRename(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	writeNumberedFile(t, src, 500)
	before, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	idx := buildIndexFor(t, src)

	replacement := filepath.Join(dir, "replacement.log")
	writeNumberedFile(t, replacement, 500)
	raw, err := os.ReadFile(replacement) //nolint:gosec // fixture path from t.TempDir
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	raw[len("LINE 1")] = '\n'
	if err := os.WriteFile(replacement, raw, 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, src); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Chtimes(src, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}

	if IsValidForSource(idx, src) {
		t.Error("the index still looks valid after the file was replaced by rename")
	}
}

// TestIdentityFieldsAreOptional keeps an index that carries no inode or
// ctime usable, so a cache written by a backend that cannot report them
// still validates on size and mtime alone.
func TestIdentityFieldsAreOptional(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	writeNumberedFile(t, src, 100)
	idx := buildIndexFor(t, src)

	idx.SourceInode = nil
	idx.SourceChangedAt = nil
	if !IsValidForSource(idx, src) {
		t.Error("an index without identity fields was treated as stale")
	}
}

// TestStaleIndexIsRebuiltRatherThanTrusted walks the whole path a caller
// takes: a rewritten file, a stale cache, and a rebuild that agrees with
// the file as it now stands.
func TestStaleIndexIsRebuiltRatherThanTrusted(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	writeNumberedFile(t, src, 2000)
	before, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	buildIndexFor(t, src)

	raw, err := os.ReadFile(src) //nolint:gosec // fixture path from t.TempDir
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	raw[len("LINE 1")] = '\n'
	if err := os.WriteFile(src, raw, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(src, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}

	stale, err := LoadForSource(src)
	if err != nil || stale != nil {
		t.Fatalf("stale index survived: idx=%v err=%v", stale, err)
	}
	rebuilt := buildIndexFor(t, src)
	if rebuilt.LineCount == nil {
		t.Fatal("rebuilt index has no line count")
	}
	// One space became a newline, so the file gained a line.
	if *rebuilt.LineCount != 2001 {
		t.Errorf("line_count = %d, want 2001", *rebuilt.LineCount)
	}
	fresh, err := LoadForSource(src)
	if err != nil {
		t.Fatalf("LoadForSource after rebuild: %v", err)
	}
	if fresh == nil {
		t.Fatal("the rebuilt index does not validate against its own source")
	}
	_ = time.Now
}

// TestConcurrentSavesLeaveOneIntactIndex covers several writers racing on
// one cache path. Save writes to a temp file and renames it into place,
// so a reader sees either the previous index or the new one, never half
// of one.
func TestConcurrentSavesLeaveOneIntactIndex(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	dir := t.TempDir()
	src := filepath.Join(dir, "busy.log")
	writeNumberedFile(t, src, 20000)

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx, err := Build(src, BuildOptions{})
			if err != nil {
				t.Errorf("Build: %v", err)
				return
			}
			if _, err := Save(idx); err != nil {
				t.Errorf("Save: %v", err)
			}
		}()
	}
	wg.Wait()

	// No temp files survive a completed race.
	leftovers, err := filepath.Glob(filepath.Join(cacheDir, "rx", "indexes", ".tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}

	got, err := LoadForSource(src)
	if err != nil {
		t.Fatalf("LoadForSource after the race: %v", err)
	}
	if got == nil {
		t.Fatal("no usable index after concurrent saves")
	}
	if got.LineCount == nil || *got.LineCount != 20000 {
		t.Errorf("line_count = %v, want 20000", got.LineCount)
	}
	if len(got.LineIndex) == 0 {
		t.Error("the surviving index has no checkpoints")
	}
}
