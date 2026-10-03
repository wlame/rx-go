package samples

import (
	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

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
// (IndexWanted) and none that still describes the file is stored.
func NeedsIndexBuild(path string, size int64) bool {
	if !IndexWanted(path, size) {
		return false
	}
	existing, err := index.LoadForSource(path)
	return err != nil || existing == nil
}

// BuildIndex builds the line index of path that a lookup uses and
// stores it, returning the index and where it was stored. progress, when
// not nil, counts how far the build has read.
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
		return nil, "", err
	}
	return idx, cachePath, nil
}
