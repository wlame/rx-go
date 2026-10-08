package logchain

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// sandbox makes a search root holding a log directory and installs it
// for the test.
func sandbox(t *testing.T) (root, logs string) {
	t.Helper()
	root = t.TempDir()
	logs = filepath.Join(root, "logs")
	if err := os.Mkdir(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.Reset)
	return root, logs
}

// A handle whose name has spaces, brackets or non-ASCII letters finds
// its chain like any other.
func TestResolve_FindsTheChainTheHandleNames(t *testing.T) {
	_, logs := sandbox(t)
	writeFiles(t, logs, map[string][]byte{
		"my app (1).log.1":    textLines("1"),
		"my app (1).log.2.gz": compressedcopy.Encode(t, compressedcopy.Gzip, textLines("2")),
		"журнал.log":          textLines("active"),
		"журнал.log.1":        textLines("1"),
		"syslog":              textLines("syslog"),
		"syslog.1":            textLines("syslog.1"),
	})
	cases := []struct {
		handle string
		parts  []string
	}{
		{filepath.Join(logs, "my app (1).log"), []string{"my app (1).log.2.gz", "my app (1).log.1"}},
		{filepath.Join(logs, "журнал.log"), []string{"журнал.log.1", "журнал.log"}},
		{filepath.Join(logs, "syslog"), []string{"syslog.1", "syslog"}},
	}
	for _, tc := range cases {
		c, err := Resolve(tc.handle)
		if err != nil {
			t.Fatalf("%s: %v", tc.handle, err)
		}
		if c.Handle() != tc.handle || !slices.Equal(partNames(c), tc.parts) {
			t.Fatalf("%s: handle %q parts %v, want %v", tc.handle, c.Handle(), partNames(c), tc.parts)
		}
		for _, p := range c.Parts {
			if p.File.IsZero() || p.File.Path() != p.Path {
				t.Fatalf("part %s is not pinned at its path", p.Name)
			}
		}
	}
	gz, err := Resolve(filepath.Join(logs, "my app (1).log"))
	if err != nil || gz.Parts[0].Format != compression.FormatGzip {
		t.Fatalf("gzip part format %v (%v)", gz.Parts[0].Format, err)
	}
}

// Resolve refuses a handle outside the roots, a hidden name and a name
// that is not a bare name, and a handle that names one file or nothing
// is not a chain.
func TestResolve_Refusals(t *testing.T) {
	root, logs := sandbox(t)
	writeFiles(t, logs, map[string][]byte{
		"single.log": textLines("single"),
		".hidden":    textLines("hidden"),
		".hidden.1":  textLines("hidden.1"),
	})
	outside := t.TempDir()
	writeFiles(t, outside, map[string][]byte{"syslog": textLines("a"), "syslog.1": textLines("b")})

	var outsideErr *paths.ErrPathOutsideRoots
	if _, err := Resolve(filepath.Join(outside, "syslog")); !errors.As(err, &outsideErr) {
		t.Fatalf("outside the roots: %v", err)
	}
	var hiddenErr *paths.ErrHiddenPath
	if _, err := Resolve(filepath.Join(logs, ".hidden")); !errors.As(err, &hiddenErr) {
		t.Fatalf("hidden name: %v", err)
	}
	for _, handle := range []string{logs + "/", logs + "/syslog/", filepath.Join(logs, "syslog") + "/.", logs + "/..", ""} {
		if _, err := Resolve(handle); !errors.Is(err, ErrInvalidHandle) {
			t.Fatalf("handle %q: %v, want ErrInvalidHandle", handle, err)
		}
	}
	for _, handle := range []string{
		filepath.Join(logs, "single.log"),
		filepath.Join(logs, "nothing.log"),
		filepath.Join(logs, "single.log", "x"),
	} {
		if _, err := Resolve(handle); !errors.Is(err, ErrNotAChain) {
			t.Fatalf("handle %q: %v, want ErrNotAChain", handle, err)
		}
	}
	if _, err := Resolve(filepath.Join(root, "absent", "syslog")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing directory: %v", err)
	}
}

// Without a sandbox (the CLI without --search-root) every directory is
// allowed, as for the other commands.
func TestResolve_WithoutSearchRoots(t *testing.T) {
	paths.Reset()
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{"app.log.1": textLines("1"), "app.log.2": textLines("2")})
	c, err := Resolve(filepath.Join(dir, "app.log"))
	if err != nil || !slices.Equal(partNames(c), []string{"app.log.2", "app.log.1"}) || c.HasActive() {
		t.Fatalf("chain %v, %v", partNames(c), err)
	}
}

// Resolve reads only the parts of the chain it names: the other
// rotated files of the directory are not opened.
func TestResolve_ClassifiesOnlyTheNamedChain(t *testing.T) {
	paths.Reset()
	dir := t.TempDir()
	writeFiles(t, dir, map[string][]byte{
		"a.log": textLines("a"), "a.log.1": textLines("a1"),
		"b.log": textLines("b"), "b.log.1": textLines("b1"),
	})
	var classified []string
	c, err := resolveWith(filepath.Join(dir, "a.log"), func(e Entry) (filekind.Kind, error) {
		classified = append(classified, e.Name)
		return ClassifyPinned(e)
	})
	slices.Sort(classified)
	if err != nil || c.Name != "a.log" || !slices.Equal(classified, []string{"a.log", "a.log.1"}) {
		t.Fatalf("chain %q, classified %v, %v", c.Name, classified, err)
	}
}

// Resolve keeps the stat of the directory it pinned: a handle through a
// symbolic link to the directory gives the directory's own stat, so a
// caller can tell that two handles name one chain.
func TestResolve_KeepsTheDirectorysStat(t *testing.T) {
	root, logs := sandbox(t)
	writeFiles(t, logs, map[string][]byte{"app.log": textLines("active"), "app.log.1": textLines("1")})
	link := filepath.Join(root, "link")
	if err := os.Symlink(logs, link); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(logs)
	if err != nil {
		t.Fatal(err)
	}

	for _, handle := range []string{filepath.Join(logs, "app.log"), filepath.Join(link, "app.log")} {
		c, err := Resolve(handle)
		if err != nil {
			t.Fatalf("%s: %v", handle, err)
		}
		if c.DirInfo == nil || !os.SameFile(c.DirInfo, want) || !c.DirInfo.IsDir() {
			t.Fatalf("%s: DirInfo %v, want the stat of %s", handle, c.DirInfo, logs)
		}
	}
}
