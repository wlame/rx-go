package samples

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrIndexNotStored is wrapped by the error of a line index that was not
// stored: BuildIndex's when the save fails, and the cause CanStoreIndex
// logs.
var ErrIndexNotStored = errors.New("cannot store the line index")

// IndexWanted reports whether a lookup in path, a file of size bytes,
// should have a line index built first. `rx samples` and GET
// /v1/samples follow the same rule, so the two surfaces leave the same
// state on disk.
//
// A second lookup in a large file is the case an index exists for, and
// every kind of file this package reads makes use of one:
//
//   - a plain file seeks to the checkpoint before the wanted line or
//     offset;
//   - a gzip, bzip2, xz or plain zstd stream still decompresses from its
//     first byte, but a lookup starts counting lines at the checkpoint
//     instead of splitting every line before it, and takes the line
//     count (or the text's length) for a position counted from the end
//     from the index instead of decompressing the whole stream first;
//   - a seekable zstd file decompresses only the frames the index's
//     frame table names.
//
// A compressed file always wants one: without it, every lookup that
// counts from the end decompresses the whole file. A plain file pays for
// its index only once it is big enough that a scan is worth avoiding,
// which is the size `rx index` starts at.
func IndexWanted(path string, size int64) bool {
	if compression.IsCompressed(path) {
		return true
	}
	return size >= int64(config.LargeFileMB())*1024*1024
}

// NeedsIndexBuild reports whether a lookup in path wants an index
// (IndexWanted) and none that still describes the file is stored, or
// whether an index stored for path cannot be read at all.
//
// The second case covers a file that would not get an index built for a
// lookup (a plain file below the large-file size) but has one, from
// `rx index --threshold=0`, that has since been damaged. Every lookup
// would warn about it again, so it is rebuilt over the damaged file
// instead; that read is bounded by the large-file size. Only the cache
// file is read to find out, never the source.
func NeedsIndexBuild(path string, size int64) bool {
	if !IndexWanted(path, size) {
		return storedIndexUnreadable(path)
	}
	existing, err := index.LoadForSource(path)
	return err != nil || existing == nil
}

// storedIndexUnreadable reports whether an index file exists for path
// that cannot be read or parsed. index.Load logs it as index_unreadable.
func storedIndexUnreadable(path string) bool {
	_, err := index.Load(path)
	return errors.Is(err, index.ErrIndexUnreadable)
}

// ShouldBuildIndex reports whether a lookup in path, a file of size
// bytes, should build its line index before it answers: the lookup needs
// one (NeedsIndexBuild) and the index cache can store it
// (CanStoreIndex). `rx samples` and GET /v1/samples both ask it.
//
// When the cache cannot store the index, building it would read the
// whole file to make an index nobody keeps, on every lookup. The lookup
// reads the file without an index instead: the same answer, at the cost
// of reading the file up to the position asked for.
func ShouldBuildIndex(path string, size int64) bool {
	return NeedsIndexBuild(path, size) && CanStoreIndex()
}

// unstorableWarned holds the index cache directories already reported
// as unable to store an index, so CanStoreIndex warns once per
// directory per process. A sync.Map is safe for the concurrent requests
// of `rx serve`; it holds one entry per directory RX_CACHE_DIR has
// named in this process, which is not something a request controls.
var unstorableWarned sync.Map

// CanStoreIndex reports whether a line index can be stored in the index
// cache directory now (index.CheckStorable). When it cannot, the first
// call in the process for that directory logs one index_not_stored
// warning that names the cause; later calls only return false, so a
// server answering many lookups, or a script calling `rx samples` in a
// loop through one process, is not flooded.
func CanStoreIndex() bool {
	err := index.CheckStorable()
	if err == nil {
		return true
	}
	dir := config.GetIndexCacheDir()
	// LoadOrStore stores the key and returns loaded=false for exactly
	// one caller, however many race here at once; that caller warns.
	if _, loaded := unstorableWarned.LoadOrStore(dir, struct{}{}); !loaded {
		slog.Default().Warn("index_not_stored",
			"dir", dir,
			"error", fmt.Errorf("%w: %w", ErrIndexNotStored, err).Error(),
			"note", "samples lookups read the file without an index; later failures are not logged")
	}
	return false
}

// BuildIndex builds the line index of path that a lookup uses and
// stores it, returning the index and where it was stored. progress, when
// not nil, counts how far the build has read. An index that was built
// but could not be stored is an error wrapping ErrIndexNotStored, never
// a success: the caller logs it or fails its task with it.
//
// Analysis is not run: nothing on the samples path reads its output, and
// a full anomaly pass to answer one line is work nobody asked for.
func BuildIndex(path string, progress *index.Progress) (*rxtypes.UnifiedFileIndex, string, error) {
	idx, err := index.Build(path, index.BuildOptions{Progress: progress})
	if err != nil {
		return nil, "", err
	}
	cachePath, err := index.Save(idx)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrIndexNotStored, err)
	}
	return idx, cachePath, nil
}
