package index

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wlame/rx-go/internal/config"
)

// The name of a cache file is at most 255 bytes, the longest name most
// filesystems accept, however long the source's base name is. A name
// that already fits keeps the exact name it always had, so no stored
// index moves.
func TestCacheFilename_FitsInOneFileName(t *testing.T) {
	cases := []struct {
		name     string
		basename string
	}{
		{"longest name kept whole", strings.Repeat("a", maxCacheNamePartBytes)},
		{"one byte too long", strings.Repeat("a", maxCacheNamePartBytes+1)},
		{"240 bytes", strings.Repeat("b", 240)},
		{"multibyte letters", strings.Repeat("ж", 200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheFilename("/var/log/" + tc.basename)
			if len(got) > MaxCacheFileNameBytes {
				t.Errorf("cache file name is %d bytes, want at most %d", len(got), MaxCacheFileNameBytes)
			}
			if !utf8.ValidString(got) {
				t.Errorf("cache file name %q is cut inside a character", got)
			}
			if !strings.HasSuffix(got, ".json") {
				t.Errorf("cache file name %q lost its suffix", got)
			}
		})
	}

	fits := strings.Repeat("a", maxCacheNamePartBytes)
	if got, want := cacheFilename("/var/log/"+fits), fits+"_"; !strings.HasPrefix(got, want) {
		t.Errorf("a base name that fits was changed: %q", got)
	}
}

// Two long base names that differ only after the cut still get two
// cache files: the hash of the whole path tells them apart.
func TestCacheFilename_LongNamesStayDistinct(t *testing.T) {
	prefix := strings.Repeat("c", 300)
	a := cacheFilename("/var/log/" + prefix + "-a.log")
	b := cacheFilename("/var/log/" + prefix + "-b.log")
	if a == b {
		t.Fatalf("two long names share the cache file %q", a)
	}
}

// A log whose base name is too long to carry whole into a cache file
// name still gets a stored index.
func TestSave_LongBaseNameIsStored(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	source := filepath.Join(t.TempDir(), strings.Repeat("d", 240)+".log")
	writePlain(t, source, numberedText(200, "LINE"))
	built, err := Build(source, BuildOptions{StepBytes: 512})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := Save(built); err != nil {
		t.Fatalf("Save: %v", err)
	}
	stored, err := LoadForSource(source)
	if err != nil || stored == nil {
		t.Fatalf("LoadForSource = %v, %v; want the stored index", stored, err)
	}
}

// CheckStorable says whether an index can be written to the cache
// before anyone spends a build on it, and names the cause when it
// cannot.
func TestCheckStorable(t *testing.T) {
	t.Run("writable", func(t *testing.T) {
		t.Setenv("RX_CACHE_DIR", t.TempDir())
		if err := CheckStorable(); err != nil {
			t.Fatalf("CheckStorable = %v, want nil", err)
		}
		left, err := os.ReadDir(config.GetIndexCacheDir())
		if err != nil {
			t.Fatalf("read index dir: %v", err)
		}
		if len(left) != 0 {
			t.Errorf("the probe left %d files behind", len(left))
		}
	})
	t.Run("read-only directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes into a directory whatever its permissions")
		}
		t.Setenv("RX_CACHE_DIR", t.TempDir())
		dir := config.GetIndexCacheDir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		err := CheckStorable()
		if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), dir) {
			t.Fatalf("CheckStorable = %v, want a permission error naming %s", err, dir)
		}
	})
	t.Run("cache directory is a regular file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Setenv("RX_CACHE_DIR", file)

		err := CheckStorable()
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("CheckStorable = %v, want an error saying a path is not a directory", err)
		}
	})
}

// When RX_CACHE_DIR names a regular file, no index can be stored under
// it, so none can be read either: a lookup is an ordinary miss, with no
// warning that would read as a damaged index.
func TestLoad_CacheDirectoryIsAFileIsAQuietMiss(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("RX_CACHE_DIR", file)
	log := captureIndexLog(t)

	idx, err := Load("/var/log/app.log")
	if idx != nil || !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("Load = %v, %v; want no index and ErrIndexNotFound", idx, err)
	}
	if errors.Is(err, ErrIndexUnreadable) {
		t.Errorf("Load reported a missing index as unreadable: %v", err)
	}
	if log.Len() != 0 {
		t.Errorf("a miss was logged:\n%s", log.String())
	}
}

// A stored index that cannot be parsed is told apart from a missing one,
// so a caller can replace it.
func TestLoadFromPath_DamagedIndexIsMarkedUnreadable(t *testing.T) {
	_, cachePath := storedIndexFixture(t)
	if err := os.WriteFile(cachePath, []byte("{"), 0o600); err != nil {
		t.Fatalf("truncate index: %v", err)
	}
	captureIndexLog(t)

	_, err := LoadFromPath(cachePath)
	if !errors.Is(err, ErrIndexUnreadable) || !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("LoadFromPath = %v; want ErrIndexUnreadable and ErrIndexNotFound", err)
	}
}
