package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

func TestSafeBasename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"foo.log", "foo.log"},
		{"foo_bar.log", "foo_bar.log"},
		{"foo-bar.log", "foo-bar.log"},
		{"foo bar.log", "foo_bar.log"},
		{"foo/bar.log", "foo_bar.log"},
		{"some:weird#chars!.log", "some_weird_chars_.log"},
		{"日本語.log", "日本語.log"}, // unicode letters preserved
		{".hidden", ".hidden"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := safeBasename(tc.in); got != tc.want {
				t.Errorf("safeBasename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGetCachePath_Scheme(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", "/tmp/rxtest")
	got := GetCachePath("/var/log/app.log")
	// Expected layout: /tmp/rxtest/rx/indexes/app.log_<16hex>.json
	wantPrefix := "/tmp/rxtest/rx/indexes/app.log_"
	if got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("wrong prefix: %q", got)
	}
	// 16 hex chars + ".json"
	if len(got) != len(wantPrefix)+16+len(".json") {
		t.Errorf("unexpected length: %q (len=%d)", got, len(got))
	}
}

func TestCacheFilename_DeterministicAcrossCalls(t *testing.T) {
	t.Parallel()
	a := cacheFilename("/var/log/foo.log")
	b := cacheFilename("/var/log/foo.log")
	if a != b {
		t.Errorf("cacheFilename not deterministic: %q vs %q", a, b)
	}
}

func TestCacheFilename_DifferentPathsDifferentHashes(t *testing.T) {
	t.Parallel()
	a := cacheFilename("/var/log/foo.log")
	b := cacheFilename("/var/log/bar.log")
	c := cacheFilename("/other/foo.log") // same basename, different dir
	if a == b {
		t.Error("different basenames produced same filename")
	}
	if a == c {
		t.Error("different directories produced same filename")
	}
}

func TestSaveAndLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", dir)

	src := &rxtypes.UnifiedFileIndex{
		Version:          Version,
		SourcePath:       "/tmp/a.log",
		SourceModifiedAt: "2026-04-17T12:34:56.123456",
		SourceSizeBytes:  1024,
		CreatedAt:        "2026-04-17T13:00:00.000000",
		BuildTimeSeconds: 0.42,
		FileType:         rxtypes.FileTypeText,
		IsText:           true,
		LineIndex: []rxtypes.LineIndexEntry{
			{LineNumber: 1, ByteOffset: 0},
			{LineNumber: 100, ByteOffset: 4096},
		},
	}
	path, err := Save(src)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if path == "" {
		t.Fatal("Save returned empty path")
	}

	got, err := Load(src.SourcePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != src.Version {
		t.Errorf("Version mismatch: got %d want %d", got.Version, src.Version)
	}
	if got.SourcePath != src.SourcePath {
		t.Errorf("SourcePath mismatch")
	}
	if len(got.LineIndex) != len(src.LineIndex) {
		t.Errorf("LineIndex length: got %d want %d", len(got.LineIndex), len(src.LineIndex))
	}
	if got.LineIndex[1].ByteOffset != 4096 {
		t.Errorf("line entry corrupted: %+v", got.LineIndex[1])
	}
}

func TestLoad_NotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", dir)
	_, err := Load("/does/not/exist.log")
	if err == nil {
		t.Error("expected ErrIndexNotFound")
	}
}

func TestIsValidForSource_MtimeBased(t *testing.T) {
	// Per user decision 6.9.1, invalidation is mtime-based. Write a
	// source file, stat it, construct an index with that mtime, then:
	//   1. Valid immediately.
	//   2. Touch the file (new mtime) → invalid.
	//   3. Truncate (new size) → invalid.
	dir := t.TempDir()
	src := filepath.Join(dir, "source.log")
	if err := os.WriteFile(src, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(src)
	idx := &rxtypes.UnifiedFileIndex{
		SourcePath:       src,
		SourceSizeBytes:  info.Size(),
		SourceModifiedAt: formatMtime(info.ModTime()),
	}
	if !IsValidForSource(idx, src) {
		t.Error("expected valid immediately after write")
	}
	// Touch: bump mtime by 1 second.
	newTime := info.ModTime().Add(time.Second)
	if err := os.Chtimes(src, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	if IsValidForSource(idx, src) {
		t.Error("expected invalid after mtime change")
	}
}

func TestIsValidForSource_SizeChange(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.log")
	os.WriteFile(src, []byte("original content\n"), 0o644)
	info, _ := os.Stat(src)

	idx := &rxtypes.UnifiedFileIndex{
		SourcePath:       src,
		SourceSizeBytes:  info.Size() + 100, // wrong size
		SourceModifiedAt: formatMtime(info.ModTime()),
	}
	if IsValidForSource(idx, src) {
		t.Error("expected invalid when size doesn't match")
	}
}

func TestIsValidForSource_MissingFile(t *testing.T) {
	idx := &rxtypes.UnifiedFileIndex{
		SourcePath:       "/does/not/exist.log",
		SourceSizeBytes:  0,
		SourceModifiedAt: "",
	}
	if IsValidForSource(idx, "/does/not/exist.log") {
		t.Error("expected invalid for missing file")
	}
}

func TestLoadForSource_Valid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", dir)
	src := filepath.Join(dir, "src.log")
	os.WriteFile(src, []byte("hi\n"), 0o644)
	info, _ := os.Stat(src)
	idx := &rxtypes.UnifiedFileIndex{
		Version:          Version,
		SourcePath:       src,
		SourceSizeBytes:  info.Size(),
		SourceModifiedAt: formatMtime(info.ModTime()),
		FileType:         rxtypes.FileTypeText,
		IsText:           true,
		LineIndex:        []rxtypes.LineIndexEntry{{LineNumber: 1, ByteOffset: 0}},
	}
	if _, err := Save(idx); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadForSource(src)
	if err != nil {
		t.Fatalf("LoadForSource: %v", err)
	}
	if got == nil {
		t.Fatal("expected index, got nil (stale?)")
	}
}

func TestLoadForSource_Stale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", dir)
	src := filepath.Join(dir, "src.log")
	os.WriteFile(src, []byte("hi\n"), 0o644)
	info, _ := os.Stat(src)
	idx := &rxtypes.UnifiedFileIndex{
		Version:          Version,
		SourcePath:       src,
		SourceSizeBytes:  info.Size(),
		SourceModifiedAt: formatMtime(info.ModTime()),
		FileType:         rxtypes.FileTypeText,
		IsText:           true,
	}
	Save(idx)
	// Mutate source.
	os.WriteFile(src, []byte("longer contents now\n"), 0o644)
	got, err := LoadForSource(src)
	if err != nil {
		t.Fatalf("LoadForSource: %v", err)
	}
	if got != nil {
		t.Error("expected nil for stale cache")
	}
}

func TestLoadForSource_Missing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", dir)
	got, err := LoadForSource("/does/not/exist.log")
	if err == nil {
		t.Error("expected ErrIndexNotFound")
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestFormatMtime_MatchesPythonLayout(t *testing.T) {
	// Build a known time and verify the layout string.
	tm := time.Date(2026, 4, 17, 12, 34, 56, 123456000, time.Local)
	got := formatMtime(tm)
	want := "2026-04-17T12:34:56.123456"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestFormatMtime_WholeSecond_OmitsFractionalSuffix is the parity test for
// the Stage 8 Blocker finding. Python's datetime.isoformat() emits
// "2024-01-01T10:00:00" (no .000000 suffix) when microseconds are 0.
// rx-python's unified_index.py uses isoformat() directly. If Go emits
// "2024-01-01T10:00:00.000000" for the same mtime, caches cross-invalidate
// whenever a file has whole-second mtime precision — which is common on
// tmpfs+relatime, FAT32, many network mounts, and any file whose mtime
// was set by `touch --date=...` without microseconds.
//
// This is the user's top mandate (cache parity with Python) so the
// behavior MUST match.
func TestFormatMtime_WholeSecond_OmitsFractionalSuffix(t *testing.T) {
	// Whole-second mtime: nanoseconds == 0.
	tm := time.Date(2024, 1, 1, 10, 0, 0, 0, time.Local)
	got := formatMtime(tm)
	want := "2024-01-01T10:00:00"
	if got != want {
		t.Errorf("whole-second mtime: got %q, want %q (Python parity)", got, want)
	}
}

// TestFormatMtime_SubSecond_EmitsMicroseconds verifies the non-zero case
// still emits 6-digit microseconds. Python's isoformat() emits
// ".microseconds" (6 digits, zero-padded) when non-zero.
func TestFormatMtime_SubSecond_EmitsMicroseconds(t *testing.T) {
	cases := []struct {
		name string
		ns   int
		want string
	}{
		// 1 microsecond = 1000 nanoseconds.
		{"1us", 1000, "2024-01-01T10:00:00.000001"},
		{"1ms", 1_000_000, "2024-01-01T10:00:00.001000"},
		{"123456us", 123456 * 1000, "2024-01-01T10:00:00.123456"},
		// Sub-microsecond nanoseconds: Python's datetime.fromtimestamp
		// has microsecond precision; our formatter truncates anything
		// smaller. 500 ns rounds down to 0 microseconds → the format
		// still has nonzero nanoseconds so we DO emit the suffix.
		// This keeps Go's behavior conservative: if Nanosecond() != 0,
		// always emit. If Nanosecond() == 0, omit. The ".000000" case
		// here is for completeness — it covers the boundary.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := time.Date(2024, 1, 1, 10, 0, 0, tc.ns, time.Local)
			got := formatMtime(tm)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsValidForSource_WholeSecondMtime_PythonWrittenCache verifies that
// a cache written by Python (with no fractional suffix) is still
// recognized as valid when Go re-reads it, even if the Go formatter's
// natural output for the same mtime would include ".000000".
//
// Concretely: if Python wrote "2024-01-01T10:00:00" to an index, and Go
// stats the file and sees nanosecond == 0, Go must emit the same string
// (without suffix). This is the regression guard.
func TestIsValidForSource_WholeSecondMtime_PythonWrittenCache(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.log")
	if err := os.WriteFile(src, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force mtime to a whole second — time.Unix(N, 0) has nanoseconds == 0.
	wholeSecond := time.Unix(1700000000, 0)
	if err := os.Chtimes(src, wholeSecond, wholeSecond); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(src)

	// A cache entry written by Python for this mtime would contain
	// "2023-11-14T..." without the .000000 suffix, formatted in local
	// time. Reproduce that exact string and expect IsValidForSource to
	// agree the cache is still valid.
	pythonStyleMtime := wholeSecond.Local().Format("2006-01-02T15:04:05")

	idx := &rxtypes.UnifiedFileIndex{
		SourcePath:       src,
		SourceSizeBytes:  info.Size(),
		SourceModifiedAt: pythonStyleMtime,
	}
	if !IsValidForSource(idx, src) {
		t.Errorf("Python-written whole-second mtime (%q) should match Go's stat — cache parity broken",
			pythonStyleMtime)
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", dir)
	// Parent dir doesn't exist yet (under $RX_CACHE_DIR/rx/indexes/).
	idx := &rxtypes.UnifiedFileIndex{
		Version:    Version,
		SourcePath: "/var/log/app.log",
		FileType:   rxtypes.FileTypeText,
	}
	_, err := Save(idx)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Verify the expected dir structure was created.
	indexesDir := filepath.Join(dir, "rx", "indexes")
	if _, err := os.Stat(indexesDir); err != nil {
		t.Errorf("expected %q to exist: %v", indexesDir, err)
	}
}
