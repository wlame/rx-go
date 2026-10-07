package logchain

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// List answers one directory's chains with every listing field, sorted
// by name.
func TestList_EntryFields(t *testing.T) {
	_, logs := sandbox(t)
	gz := compressedcopy.Encode(t, compressedcopy.Gzip, textLines("syslog.2"))
	xz := compressedcopy.Encode(t, compressedcopy.Xz, textLines("syslog.4"))
	writeFiles(t, logs, map[string][]byte{
		"syslog":      textLines("syslog"),
		"syslog.1":    textLines("syslog.1"),
		"syslog.2.gz": gz,
		"syslog.4.xz": xz,
		"App.log.1":   textLines("App.log.1"),
		"App.log.2":   textLines("App.log.2"),
		"lonely.log":  textLines("lonely"),
	})

	resp, err := List(logs)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Path != logs || len(resp.Chains) != 2 {
		t.Fatalf("answer %+v", resp)
	}
	app, sys := resp.Chains[0], resp.Chains[1]
	wantApp := rxtypes.ChainEntry{
		Path: filepath.Join(logs, "App.log"), Name: "App.log", Parts: []string{"App.log.2", "App.log.1"},
		HasActive: false, Missing: []string{}, Size: int64(len(textLines("App.log.1")) + len(textLines("App.log.2"))),
		CompressionFormats: []string{}, IsIndexed: false, Unreadable: []string{},
	}
	if !equalEntries(app, wantApp) {
		t.Fatalf("App.log entry %+v\nwant %+v", app, wantApp)
	}
	wantSys := rxtypes.ChainEntry{
		Path: filepath.Join(logs, "syslog"), Name: "syslog",
		Parts:     []string{"syslog.4.xz", "syslog.2.gz", "syslog.1", "syslog"},
		HasActive: true, Missing: []string{"syslog.3"}, MissingCount: 1,
		Size:               int64(len(textLines("syslog")) + len(textLines("syslog.1")) + len(gz) + len(xz)),
		CompressionFormats: []string{"gzip", "xz"}, IsIndexed: false, Unreadable: []string{},
	}
	if !equalEntries(sys, wantSys) {
		t.Fatalf("syslog entry %+v\nwant %+v", sys, wantSys)
	}
}

// is_indexed is true when every frozen part has a current line index;
// the active part does not count.
func TestList_IsIndexedWhenEveryFrozenPartHasAnIndex(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	_, logs := sandbox(t)
	writeFiles(t, logs, map[string][]byte{
		"app.log":      textLines("app.log"),
		"app.log.1":    textLines("app.log.1"),
		"app.log.2.gz": compressedcopy.Encode(t, compressedcopy.Gzip, textLines("app.log.2")),
	})
	indexed := func() bool {
		t.Helper()
		resp, err := List(logs)
		if err != nil || len(resp.Chains) != 1 {
			t.Fatalf("list: %+v %v", resp, err)
		}
		return resp.Chains[0].IsIndexed
	}
	build := func(name string) {
		t.Helper()
		idx, err := index.Build(filepath.Join(logs, name), index.BuildOptions{})
		if err != nil {
			t.Fatalf("build %s: %v", name, err)
		}
		if _, err := index.Save(idx); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	if indexed() {
		t.Fatal("indexed before any index was built")
	}
	build("app.log.1")
	if indexed() {
		t.Fatal("indexed with one frozen part left")
	}
	build("app.log.2.gz")
	if !indexed() {
		t.Fatal("not indexed after every frozen part was")
	}
	// A frozen part that changes after its index was built makes the
	// index stale, and the chain is no longer indexed.
	if err := os.WriteFile(filepath.Join(logs, "app.log.1"), textLines("app.log.1 changed, longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if indexed() {
		t.Fatal("indexed with a stale index")
	}
}

// The errors List gives for a path that is not a directory it may
// list: the callers map them to statuses and exit codes.
func TestList_Errors(t *testing.T) {
	root, logs := sandbox(t)
	writeFiles(t, logs, map[string][]byte{"file.log": textLines("f")})
	if err := os.Mkdir(filepath.Join(root, ".secret"), 0o750); err != nil {
		t.Fatal(err)
	}

	var outside *paths.ErrPathOutsideRoots
	if _, err := List(t.TempDir()); !errors.As(err, &outside) {
		t.Fatalf("outside: %v", err)
	}
	var hidden *paths.ErrHiddenPath
	if _, err := List(filepath.Join(root, ".secret")); !errors.As(err, &hidden) {
		t.Fatalf("hidden: %v", err)
	}
	if _, err := List(filepath.Join(root, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := List(filepath.Join(logs, "file.log")); !errors.Is(err, ErrNotADirectory) {
		t.Fatalf("a file: %v", err)
	}
	// A directory without chains answers an empty list, never null.
	resp, err := List(root)
	if err != nil || resp.Chains == nil || len(resp.Chains) != 0 {
		t.Fatalf("no chains: %+v %v", resp, err)
	}
}

// A directory the process may not read is an access error.
func TestList_UnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	_, logs := sandbox(t)
	locked := filepath.Join(logs, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
	if _, err := List(locked); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("unreadable: %v", err)
	}
}

// equalEntries compares two listing entries field by field.
func equalEntries(a, b rxtypes.ChainEntry) bool {
	return a.Path == b.Path && a.Name == b.Name && slices.Equal(a.Parts, b.Parts) && a.HasActive == b.HasActive &&
		a.Missing != nil && slices.Equal(a.Missing, b.Missing) && a.MissingCount == b.MissingCount && a.Size == b.Size &&
		a.CompressionFormats != nil && slices.Equal(a.CompressionFormats, b.CompressionFormats) &&
		a.IsIndexed == b.IsIndexed && a.Parts != nil && a.Unreadable != nil && slices.Equal(a.Unreadable, b.Unreadable)
}

// is_indexed holds a stored index to the file the listing pinned: a
// part another file replaced after the listing (renamed into its name,
// as a rotation does) is not indexed, even though the file now at its
// path has a current index of its own.
func TestListingEntry_IsIndexedOnlyForThePinnedFile(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{"app.log": textLines("app.log"), "app.log.1": textLines("app.log.1")})
	listed := Group(dir, listedEntries(t, dir), ClassifyPinned)
	if len(listed) != 1 {
		t.Fatalf("chains %v", chainsByName(listed))
	}

	swapped := filepath.Join(dir, "swapped")
	if err := os.WriteFile(swapped, textLines("another app.log.1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swapped, filepath.Join(dir, "app.log.1")); err != nil {
		t.Fatal(err)
	}
	storeIndexes(t, dir, "app.log.1")
	if listingEntry(listed[0]).IsIndexed {
		t.Fatal("the index of the file now at the part's path counted for the pinned part")
	}

	// A new listing pins the new file, which its index describes.
	relisted := Group(dir, listedEntries(t, dir), ClassifyPinned)
	if !listingEntry(relisted[0]).IsIndexed {
		t.Fatal("not indexed after a new listing")
	}
}

// The listing entry of a chain of more than MaxParts parts lists no
// part, names no missing part and looks at no stored index: such a
// chain is not read as one text.
func TestListingEntry_TooManyPartsIsNotRead(t *testing.T) {
	c := Group(testDir, fakeEntries(fakeNames("big.log", MaxParts)...), allText)[0]
	peeks := 0
	peek := peekPartIndex
	t.Cleanup(func() { peekPartIndex = peek })
	peekPartIndex = func(path string) (*rxtypes.UnifiedFileIndex, error) {
		peeks++
		return peek(path)
	}
	entry := listingEntry(c)
	if !entry.TooManyParts || entry.Parts == nil || len(entry.Parts) != 0 || len(entry.Missing) != 0 ||
		entry.MissingCount != 0 || entry.IsIndexed || entry.HasActive != true {
		t.Fatalf("entry %+v", entry)
	}
	if peeks != 0 {
		t.Fatalf("%d stored indexes looked at", peeks)
	}
}

// A part the process may not read is listed, and named in unreadable;
// the chain is not indexed.
func TestList_UnreadablePartIsListedAndNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its permissions")
	}
	_, logs := sandbox(t)
	writeFiles(t, logs, map[string][]byte{
		"syslog": textLines("syslog"), "syslog.1": textLines("syslog.1"), "syslog.2": textLines("syslog.2"),
	})
	locked := filepath.Join(logs, "syslog.2")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	resp, err := List(logs)
	if err != nil || len(resp.Chains) != 1 {
		t.Fatalf("list: %+v %v", resp, err)
	}
	entry := resp.Chains[0]
	if !slices.Equal(entry.Parts, []string{"syslog.2", "syslog.1", "syslog"}) ||
		!slices.Equal(entry.Unreadable, []string{"syslog.2"}) || len(entry.Missing) != 0 || entry.IsIndexed {
		t.Fatalf("entry %+v", entry)
	}
}
