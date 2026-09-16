package samples

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// A lookup wants an index for every compressed file and for a plain file
// of the large-file size or more; a smaller plain file is read directly.
func TestIndexWanted_ByKindAndSize(t *testing.T) {
	t.Setenv("RX_LARGE_FILE_MB", "1")
	const megabyte = 1024 * 1024
	cases := []struct {
		name string
		path string
		size int64
		want bool
	}{
		{"plain below the large-file size", "/logs/app.log", megabyte - 1, false},
		{"plain at the large-file size", "/logs/app.log", megabyte, true},
		{"small gzip", "/logs/app.log.gz", 10, true},
		{"small bzip2", "/logs/app.log.bz2", 10, true},
		{"small xz", "/logs/app.log.xz", 10, true},
		{"small zstd", "/logs/app.log.zst", 10, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IndexWanted(tc.path, tc.size); got != tc.want {
				t.Fatalf("IndexWanted(%s, %d) = %v, want %v", tc.path, tc.size, got, tc.want)
			}
		})
	}
}

// A wanted index needs building until one is stored, and not after.
func TestNeedsIndexBuild_UntilTheIndexIsStored(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	text, _ := variedLog(500, 3)
	path := filepath.Join(t.TempDir(), "app.log.gz")
	if err := os.WriteFile(path, compressedcopy.Encode(t, compressedcopy.Gzip, text), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !NeedsIndexBuild(path, info.Size()) {
		t.Fatal("a gzip file without an index does not need one built")
	}

	progress := &index.Progress{}
	idx, cachePath, err := BuildIndex(path, progress)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if idx == nil || cachePath == "" {
		t.Fatalf("BuildIndex returned index %v at %q", idx, cachePath)
	}
	if fraction, known := progress.Fraction(); !known || fraction != 1 {
		t.Errorf("progress after the build = %v (known %v), want 1", fraction, known)
	}
	if NeedsIndexBuild(path, info.Size()) {
		t.Fatal("the index just stored still needs building")
	}
}
