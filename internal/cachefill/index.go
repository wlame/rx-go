package cachefill

import (
	"context"
	"errors"
	"fmt"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
)

// IndexSettings are the settings of an index run.
type IndexSettings struct {
	// ThresholdBytes is the smallest file indexed; a smaller one is
	// skipped. 0 indexes every file.
	ThresholdBytes int64
	// Force builds every index again, current ones too.
	Force bool
	// CheckStorable says whether the index cache can store an index now
	// (index.CheckStorable); it is asked before each build. A run asks
	// the cache once and keeps the answer (sync.OnceValue), so a cache
	// that cannot store fails every file with the same cause without
	// probing again.
	CheckStorable func() error
}

// BelowThresholdReason words the skip of a file of size bytes, smaller
// than the index threshold of thresholdBytes. POST /v1/index refuses a
// single such file with the same numbers.
func BelowThresholdReason(size, thresholdBytes int64) string {
	return fmt.Sprintf("file size %d bytes is below threshold %d bytes", size, thresholdBytes)
}

// errNotRegular is the error of a path that leads to a named pipe, a
// socket or a device, which no command reads.
func errNotRegular(path string) error {
	return fmt.Errorf("%w: %s", paths.ErrNotRegularFile, path)
}

// IndexFile builds and stores the line index of the entry's file, as
// `rx index` builds it, or decides not to:
//
//   - the file is pinned (checked and tied to what its path leads to
//     now); one that cannot be pinned, and one that is not a regular
//     file, is unreachable (a skip when walked, a failure when named);
//   - a file below the threshold is skipped, and so is one whose text is
//     not text (filekind), with the reason;
//   - without Force, a stored index that still describes the file
//     (index.LoadForPinned) makes it Current;
//   - a cache that cannot store an index fails the file with the cause;
//   - otherwise the index is built (index.BuildContext, counting its
//     reads into build when that is not nil) and stored: Filled. A file
//     rx refuses to decompress is skipped with the reason; a build or
//     store that fails fails the file.
//
// Canceling ctx stops the build at its next read of the file's text
// (index.BuildContext); ctx is checked again before the index is
// stored, so a canceled run stores nothing for the file. The result is
// then Failed with ctx's error.
func IndexFile(ctx context.Context, e Entry, s IndexSettings, build *index.Progress) FileResult {
	src, err := paths.Pin(e.Path)
	if err != nil {
		return unreachable(e, err)
	}
	info := src.Info()
	if !info.Mode().IsRegular() {
		return unreachable(e, errNotRegular(e.Path))
	}
	if info.Size() < s.ThresholdBytes {
		return skipped(e.Path, BelowThresholdReason(info.Size(), s.ThresholdBytes))
	}
	kind, err := filekind.OfPinnedForReading(src)
	if err != nil {
		return unreachable(e, err)
	}
	if !kind.IsText() {
		return skipped(e.Path, kind.NotText)
	}
	if !s.Force {
		// A stored index that is missing, stale, of another file or
		// unreadable means a build, not a failure.
		if idx, loadErr := index.LoadForPinned(src); loadErr == nil && idx != nil {
			return FileResult{Path: e.Path, Outcome: Current, Index: idx, IndexPath: index.GetCachePath(e.Path)}
		}
	}
	// An index the cache cannot store is not built: the build would read
	// the whole file and the save would then fail.
	if storeErr := s.CheckStorable(); storeErr != nil {
		return failed(e.Path, fmt.Errorf("cannot store the line index: %w", storeErr))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return failed(e.Path, ctxErr)
	}
	idx, err := index.BuildContext(ctx, e.Path, index.BuildOptions{Progress: build})
	if errors.Is(err, compression.ErrTooLargeToDecode) {
		return skipped(e.Path, compression.TooLargeToDecodeReason)
	}
	if err != nil {
		return failed(e.Path, err)
	}
	// INVARIANT: a canceled run stores nothing for the file it was on.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return failed(e.Path, ctxErr)
	}
	indexPath, err := index.Save(idx)
	if err != nil {
		return failed(e.Path, err)
	}
	return FileResult{Path: e.Path, Outcome: Filled, Index: idx, IndexPath: indexPath}
}
