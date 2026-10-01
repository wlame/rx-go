package findings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readJSON takes at most its limit into memory: a file longer than the
// limit is refused even when its first bytes parse, and a missing file
// is an ordinary miss.
func TestReadJSON_RefusesAFileOverTheLimit(t *testing.T) {
	dir := t.TempDir()
	doc := `{"key":"abc"}`
	path := filepath.Join(dir, "entry.json")
	if err := os.WriteFile(path, []byte(doc+strings.Repeat(" ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var v entryFile
	if err := readJSON(openCacheFile, root, "entry.json", int64(len(doc)+5), &v); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("over the limit: %v", err)
	}
	if err := readJSON(openCacheFile, root, "entry.json", 1<<10, &v); err != nil || v.Key != "abc" {
		t.Errorf("within the limit: %v, %+v", err, v)
	}
	if err := readJSON(openCacheFile, root, "none.json", 1<<10, &v); !errors.Is(err, errAbsent) {
		t.Errorf("a missing file: %v", err)
	}
}

// A long base name is cut so the directory name fits in one path
// component, and two paths with one base name get two directories.
func TestCacheDir_Names(t *testing.T) {
	long := strings.Repeat("x", 400) + ".log"
	name := filepath.Base(cacheDir("/cache", "/var/log/"+long))
	if len(name) > 255 || !strings.HasPrefix(name, "xxx") {
		t.Errorf("long name %d bytes: %q", len(name), name)
	}
	if cacheDir("/cache", "/a/app.log") == cacheDir("/cache", "/b/app.log") {
		t.Error("two files share a directory")
	}
}
