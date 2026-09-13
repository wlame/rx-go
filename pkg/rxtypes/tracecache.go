package rxtypes

// TraceCacheMatch is a single cached match entry on disk.
//
// Minimal representation: enough information to reconstruct a full Match
// on cache hit by re-reading the source line (via the index) and re-running
// the patterns against LineText.
//
// FrameIndex is present only for seekable-zstd caches; for regular
// files it's omitted from the JSON entirely (pointer + ,omitempty).
type TraceCacheMatch struct {
	PatternIndex int   `json:"pattern_index"`
	Offset       int64 `json:"offset"`
	LineNumber   int64 `json:"line_number"`
	FrameIndex   *int  `json:"frame_index,omitempty"`
}

// TraceCacheData is the full on-disk schema for a trace-cache file.
//
// Filename scheme:
//
//	~/.cache/rx/trace_cache/<patterns_hash>/<path_hash>_<filename>.json
//
// The format version is trace.TraceCacheVersion. A cache of another
// version, including those rx-python writes, is treated as absent.
// Fields are in the order rx-python emits, with the identity fields
// after the size.
//
// CompressionFormat + FramesWithMatches are only present for caches
// of compressed files — they're omitted entirely for regular-file
// caches, matching Python's write behavior.
type TraceCacheData struct {
	Version          int    `json:"version"`
	SourcePath       string `json:"source_path"`
	SourceModifiedAt string `json:"source_modified_at"`
	SourceSizeBytes  int64  `json:"source_size_bytes"`
	// Source identity, taken when the scan was planned, with the same
	// meaning as the fields of the same names in UnifiedFileIndex: the
	// inode, the inode-change time in the SourceModifiedAt layout, and a
	// digest of the size plus the first and last 64 KiB. Each is null
	// when it could not be read, and a null field is not compared.
	SourceInode       *uint64  `json:"source_inode"`
	SourceChangedAt   *string  `json:"source_changed_at"`
	SourceFingerprint *string  `json:"source_fingerprint"`
	Patterns          []string `json:"patterns"`
	PatternsHash      string   `json:"patterns_hash"`
	RgFlags           []string `json:"rg_flags"`
	CreatedAt         string   `json:"created_at"`
	// ChunkCount is the file_chunks value of the scan that wrote the
	// cache: the number of chunks a plain file was split into, or the
	// number of frames of a seekable-zstd file. A cache hit reports it,
	// so the answer is the scan's. A scan always has at least one.
	ChunkCount        int               `json:"chunk_count"`
	Matches           []TraceCacheMatch `json:"matches"`
	CompressionFormat string            `json:"compression_format,omitempty"`
	FramesWithMatches []int             `json:"frames_with_matches,omitempty"`
}
