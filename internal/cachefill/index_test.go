package cachefill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
)

// indexAll is the settings of an index run that indexes every file
// (threshold 0) into a cache that can store it.
func indexAll(force bool) IndexSettings {
	return IndexSettings{Force: force, CheckStorable: func() error { return nil }}
}

// Each file is decided as `rx index` decides it: a file below the
// threshold and a file whose text is not text are skipped with the
// reason, a text file is built and stored, a second run reuses the
// stored index (current), and force builds it again.
func TestIndexFile_DecidesEachFileAsRxIndexDoes(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := folder(t)
	log, blob := filepath.Join(dir, "app.log"), filepath.Join(dir, "blob.bin")
	ctx := context.Background()

	small := IndexSettings{ThresholdBytes: 1 << 20, CheckStorable: func() error { return nil }}
	if got := IndexFile(ctx, walked(t, log), small, nil); got.Outcome != Skipped ||
		got.Reason != BelowThresholdReason(int64(len(appLog)), 1<<20) {
		t.Errorf("below the threshold: %+v", got)
	}
	if got := IndexFile(ctx, walked(t, blob), indexAll(false), nil); got.Outcome != Skipped ||
		!strings.HasPrefix(got.Reason, filekind.NotTextPrefix) {
		t.Errorf("a binary file: %+v", got)
	}

	built := IndexFile(ctx, walked(t, log), indexAll(false), &index.Progress{})
	if built.Outcome != Filled || built.Index == nil || built.IndexPath != index.GetCachePath(log) {
		t.Fatalf("first build: %+v", built)
	}
	if _, err := os.Stat(built.IndexPath); err != nil {
		t.Fatalf("the index was not stored: %v", err)
	}
	if again := IndexFile(ctx, walked(t, log), indexAll(false), nil); again.Outcome != Current ||
		again.Index == nil || again.IndexPath != built.IndexPath {
		t.Errorf("second run: %+v", again)
	}
	if forced := IndexFile(ctx, walked(t, log), indexAll(true), nil); forced.Outcome != Filled || forced.Index == nil {
		t.Errorf("forced run: %+v", forced)
	}
}

// A cache that cannot store an index fails the file with the cause, and
// nothing is built.
func TestIndexFile_AnUnstorableCacheFailsTheFile(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	log := filepath.Join(folder(t), "app.log")
	settings := IndexSettings{CheckStorable: func() error { return errors.New("read-only file system") }}
	got := IndexFile(context.Background(), walked(t, log), settings, nil)
	if got.Outcome != Failed || got.Reason != "cannot store the line index: read-only file system" || got.Unreadable {
		t.Errorf("unstorable: %+v", got)
	}
	if _, err := os.Stat(index.GetCachePath(log)); !os.IsNotExist(err) {
		t.Errorf("an index was stored: %v", err)
	}
}

// A file that cannot be reached is a skip with the walk's wording when a
// walk found it, and a failure with the error when the caller named it.
func TestIndexFile_AnUnreachableFileIsASkipWhenWalkedAndAFailureWhenNamed(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	gone := filepath.Join(t.TempDir(), "gone.log")
	if got := IndexFile(context.Background(), Entry{Path: gone}, indexAll(false), nil); got.Outcome != Skipped ||
		got.Reason != paths.FailureReason(os.ErrNotExist) {
		t.Errorf("walked: %+v", got)
	}
	got := IndexFile(context.Background(), Entry{Path: gone, Named: true}, indexAll(false), nil)
	if got.Outcome != Failed || !got.Unreadable || !errors.Is(got.Err, os.ErrNotExist) {
		t.Errorf("named: %+v", got)
	}
}

// A run canceled before a file is reached builds and stores nothing for
// it.
func TestIndexFile_CanceledStoresNothing(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	log := filepath.Join(folder(t), "app.log")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := IndexFile(ctx, walked(t, log), indexAll(false), nil)
	if got.Outcome != Failed || !errors.Is(got.Err, context.Canceled) {
		t.Errorf("canceled: %+v", got)
	}
	if _, err := os.Stat(index.GetCachePath(log)); !os.IsNotExist(err) {
		t.Errorf("an index was stored: %v", err)
	}
}
