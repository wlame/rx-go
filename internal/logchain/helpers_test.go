package logchain

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
)

// fakeInfo is the os.FileInfo of an entry that exists only in a test:
// Group reads nothing of an entry but its name, its type, its size and
// its modification time, and the classifier decides the rest.
type fakeInfo struct {
	name  string
	size  int64
	mtime time.Time
	dir   bool
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) ModTime() time.Time { return f.mtime }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }
func (f fakeInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

// testDir is the directory the name-only tests pretend to list.
const testDir = "/logs"

// baseTime is the modification time of every fake entry unless a test
// sets its own.
var baseTime = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// fakeEntries makes one non-empty file entry per name, all with the same
// modification time, so only the names decide.
func fakeEntries(names ...string) []Entry {
	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		entries = append(entries, Entry{
			Name: name,
			Path: filepath.Join(testDir, name),
			Info: fakeInfo{name: name, size: 100, mtime: baseTime},
		})
	}
	return entries
}

// allText classifies every entry as a plain text file without reading
// anything.
func allText(Entry) (filekind.Kind, error) { return filekind.Kind{}, nil }

// partNames returns the names of a candidate's parts in their order.
func partNames(c Candidate) []string {
	names := make([]string, 0, len(c.Parts))
	for _, p := range c.Parts {
		names = append(names, p.Name)
	}
	return names
}

// chainsByName maps each candidate's name to its parts' names.
func chainsByName(cands []Candidate) map[string][]string {
	out := map[string][]string{}
	for _, c := range cands {
		out[c.Name] = partNames(c)
	}
	return out
}

// writeFiles creates each file of files in dir, with its content.
func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// listedEntries lists dir the way the callers do: pinned, through
// paths.ListDir, refused entries dropped.
func listedEntries(t *testing.T, dir string) []Entry {
	t.Helper()
	pinned, err := paths.Pin(dir)
	if err != nil {
		t.Fatalf("pin %s: %v", dir, err)
	}
	listed, err := paths.ListDir(pinned)
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	return EntriesOf(listed)
}

// textLines is a small log text whose lines name their file.
func textLines(name string) []byte {
	return []byte("2026-10-01 00:00:00 LINE 1 " + name + "\n2026-10-01 00:00:01 LINE 2 " + name + "\n")
}

// binaryBytes is a file the text check refuses: a NUL byte in its head.
var binaryBytes = []byte{'u', 't', 0x00, 0x00, 0x07, 'x', 0x00, '\n'}
