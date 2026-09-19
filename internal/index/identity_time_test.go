package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
)

// useLocalZone makes name the process's local time zone until the test
// ends.
//
// Go note: setting TZ with t.Setenv does not work inside one process.
// Go reads TZ once, the first time anything asks for local time, and
// keeps the result in the package variable time.Local. Assigning that
// variable is what changes the zone every later t.Local() call uses. A
// test that does so must not run in parallel with others; none of the
// tests here call t.Parallel, and Go starts the parallel tests of a
// package only after every sequential one has finished.
func useLocalZone(t *testing.T, name string) {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone %s is not available: %v", name, err)
	}
	saved := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = saved })
}

// An index records the time of the file it describes, and a later run
// compares it with the file's time now. A serve started with TZ=UTC
// and a CLI in the user's zone share one cache; both must accept the
// index the other built.
func TestIndexStaysValidWhenTheLocalTimeZoneChanges(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	src := filepath.Join(t.TempDir(), "app.log")
	writeNumberedFile(t, src, 200)

	useLocalZone(t, "UTC")
	buildIndexFor(t, src)

	for _, zone := range []string{"Asia/Tokyo", "America/New_York", "UTC"} {
		useLocalZone(t, zone)
		got, err := LoadForSource(src)
		if err != nil || got == nil {
			t.Errorf("TZ=%s: LoadForSource = %v, %v; want the index built under TZ=UTC", zone, got, err)
		}
	}
}

// In the hour a daylight-saving change repeats, two instants an hour
// apart have the same local wall-clock time. A file whose mtime moves
// from one to the other has changed, and its identity must say so even
// where the ctime and the fingerprint cannot (a filesystem that reports
// no ctime, an edit in the middle of the file).
func TestIdentityTellsApartTwoMtimesInTheRepeatedHour(t *testing.T) {
	useLocalZone(t, "America/New_York")
	// 2025-11-02 01:30 happens twice in New York: first in EDT (05:30
	// UTC), then, after the clocks go back, in EST (06:30 UTC).
	first := time.Date(2025, 11, 2, 5, 30, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	if formatMtime(first) != formatMtime(second) {
		t.Fatalf("precondition: %s and %s should read the same local time, got %q and %q",
			first, second, formatMtime(first), formatMtime(second))
	}

	src := filepath.Join(t.TempDir(), "app.log")
	writeNumberedFile(t, src, 20)
	if err := os.Chtimes(src, first, first); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	recorded := withMtimeOnly(IdentityFromInfo(src, info))
	if !recorded.MatchesFile(src) {
		t.Fatal("precondition: the identity does not match the file it was taken from")
	}

	if err := os.Chtimes(src, second, second); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if recorded.MatchesFile(src) {
		t.Errorf("an mtime an hour later in the repeated hour still matches the recorded identity")
	}
}

// withMtimeOnly leaves out of id what would catch a change besides the
// size, the mtime and the inode, so a test sees what the mtime check
// alone decides.
func withMtimeOnly(id SourceIdentity) SourceIdentity {
	id.ChangedAt = nil
	id.ChangedNs = nil
	id.Fingerprint = nil
	return id
}

// An inode number is unique only on its device: two filesystems can
// each hold a file with the same one. An index records both, and a
// pinned file is the indexed one only when both agree.
func TestDescribesPinned_ComparesTheDeviceBesideTheInode(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	paths.Reset()
	src := filepath.Join(t.TempDir(), "app.log")
	writeNumberedFile(t, src, 20)
	idx := buildIndexFor(t, src)
	if idx.SourceInode == nil || idx.SourceDevice == nil {
		t.Skip("this platform reports no inode or device")
	}
	pinned, err := paths.Pin(src)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if !DescribesPinned(idx, pinned) {
		t.Fatal("precondition: the index does not describe the file it was built from")
	}

	otherDevice := *idx.SourceDevice + 1
	idx.SourceDevice = &otherDevice
	if DescribesPinned(idx, pinned) {
		t.Error("an index of the same inode on another device describes the pinned file")
	}

	idx.SourceDevice = nil
	if !DescribesPinned(idx, pinned) {
		t.Error("an index that records no device is not compared by its inode alone")
	}
}

// A fresh index records the times validation compares and the device.
func TestIndexRecordsTimesAsNanosecondsAndTheDevice(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	src := filepath.Join(t.TempDir(), "app.log")
	writeNumberedFile(t, src, 20)
	idx := buildIndexFor(t, src)
	info, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if idx.SourceMtimeNs != info.ModTime().UnixNano() {
		t.Errorf("source_mtime_ns = %d, want %d", idx.SourceMtimeNs, info.ModTime().UnixNano())
	}
	if _, ok := sourceIdentity(info); ok && (idx.SourceCtimeNs == nil || idx.SourceDevice == nil) {
		t.Errorf("source_ctime_ns = %v, source_device = %v; want both recorded", idx.SourceCtimeNs, idx.SourceDevice)
	}
}

// An index records its file by the absolute path, as its cache file
// name does, whatever spelling the caller used: a relative source_path
// depends on the directory the index was built from, and a cleanup that
// checks it from another directory deletes a valid index.
func TestIndexRecordsTheAbsoluteSourcePath(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	writeNumberedFile(t, filepath.Join(dir, "app.log"), 20)
	t.Chdir(dir)

	idx := buildIndexFor(t, "app.log")

	want, err := filepath.Abs("app.log")
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if idx.SourcePath != want {
		t.Errorf("source_path = %q, want %q", idx.SourcePath, want)
	}
}
