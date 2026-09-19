package samples

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// captureSamplesLog sends slog's default logger to a buffer for the rest
// of the test.
func captureSamplesLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// gzipLogFixture writes a gzip log and returns its path and size.
func gzipLogFixture(t *testing.T) (string, int64) {
	t.Helper()
	text, _ := variedLog(500, 3)
	path := filepath.Join(t.TempDir(), "app.log.gz")
	if err := os.WriteFile(path, compressedcopy.Encode(t, compressedcopy.Gzip, text), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return path, info.Size()
}

// unstorableCaches are the ways the line index cache can refuse a
// write: each sets RX_CACHE_DIR for the test.
var unstorableCaches = []struct {
	name  string
	setUp func(t *testing.T)
}{
	{"read-only index directory", func(t *testing.T) {
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
	}},
	{"cache directory is a regular file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Setenv("RX_CACHE_DIR", file)
	}},
}

// When the index cannot be stored, a lookup does not build one only to
// throw it away: it reads the file without an index. The operator hears
// why once per process, not once per lookup.
func TestShouldBuildIndex_NotWhenTheIndexCannotBeStored(t *testing.T) {
	for _, tc := range unstorableCaches {
		t.Run(tc.name, func(t *testing.T) {
			tc.setUp(t)
			path, size := gzipLogFixture(t)
			log := captureSamplesLog(t)

			for range 2 {
				if ShouldBuildIndex(path, size) {
					t.Fatal("ShouldBuildIndex = true for an index that cannot be stored")
				}
			}
			logged := log.String()
			if got := strings.Count(logged, "index_not_stored"); got != 1 {
				t.Errorf("index_not_stored warnings: %d, want 1:\n%s", got, logged)
			}
			if !strings.Contains(logged, "cannot store the line index") {
				t.Errorf("the warning does not say what it means:\n%s", logged)
			}
			if strings.Contains(logged, "index_unreadable") {
				t.Errorf("a missing index was reported as unreadable:\n%s", logged)
			}
		})
	}
}

// With a writable cache, a lookup that wants an index builds it.
func TestShouldBuildIndex_WhenTheIndexCanBeStored(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, size := gzipLogFixture(t)
	if !ShouldBuildIndex(path, size) {
		t.Fatal("ShouldBuildIndex = false for a gzip file without an index and a writable cache")
	}
}

// A stored index that cannot be read is rebuilt even for a plain file
// below the large-file size, which would not get a new one: otherwise
// it stays damaged and every lookup warns about it again.
func TestNeedsIndexBuild_DamagedIndexOfASmallFile(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	text, _ := variedLog(200, 5)
	path := filepath.Join(t.TempDir(), "small.log")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	size := int64(len(text))
	if NeedsIndexBuild(path, size) {
		t.Fatal("a small file without an index needs one built")
	}

	built, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cachePath, err := index.Save(built)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if NeedsIndexBuild(path, size) {
		t.Fatal("a small file with a valid index needs one built")
	}

	if err := os.WriteFile(cachePath, []byte("{"), 0o600); err != nil {
		t.Fatalf("damage the index: %v", err)
	}
	captureSamplesLog(t)
	if !NeedsIndexBuild(path, size) {
		t.Fatal("a damaged index of a small file is not rebuilt")
	}
}

// BuildIndex reports a failed save instead of returning an index nobody
// stored, and says what failed.
func TestBuildIndex_ReportsAFailedSave(t *testing.T) {
	path, _ := gzipLogFixture(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("RX_CACHE_DIR", file)

	idx, _, err := BuildIndex(path, nil)
	if err == nil || idx != nil {
		t.Fatalf("BuildIndex = %v, %v; want an error", idx, err)
	}
	if !strings.Contains(err.Error(), "cannot store the line index") {
		t.Errorf("error %q does not say the index was not stored", err)
	}
}
