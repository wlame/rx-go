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
		CompressionFormats: []string{}, IsIndexed: false,
	}
	if !equalEntries(app, wantApp) {
		t.Fatalf("App.log entry %+v\nwant %+v", app, wantApp)
	}
	wantSys := rxtypes.ChainEntry{
		Path: filepath.Join(logs, "syslog"), Name: "syslog",
		Parts:     []string{"syslog.4.xz", "syslog.2.gz", "syslog.1", "syslog"},
		HasActive: true, Missing: []string{"syslog.3"}, MissingCount: 1,
		Size:               int64(len(textLines("syslog")) + len(textLines("syslog.1")) + len(gz) + len(xz)),
		CompressionFormats: []string{"gzip", "xz"}, IsIndexed: false,
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
		a.IsIndexed == b.IsIndexed && a.Parts != nil
}
