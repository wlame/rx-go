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
// Filename scheme (per spec §5.2):
//
//	~/.cache/rx/trace_cache/<patterns_hash>/<path_hash>_<filename>.json
//
// Cross-format compatibility: a file written by Python v2 loads in Go
// and vice versa. Fields are in the same order Python emits.
//
// CompressionFormat + FramesWithMatches are only present for caches
// of compressed files — they're omitted entirely for regular-file
// caches, matching Python's write behavior.
type TraceCacheData struct {
	Version           int               `json:"version"`
	SourcePath        string            `json:"source_path"`
	SourceModifiedAt  string            `json:"source_modified_at"`
	SourceSizeBytes   int64             `json:"source_size_bytes"`
	Patterns          []string          `json:"patterns"`
	PatternsHash      string            `json:"patterns_hash"`
	RgFlags           []string          `json:"rg_flags"`
	CreatedAt         string            `json:"created_at"`
	Matches           []TraceCacheMatch `json:"matches"`
	CompressionFormat string            `json:"compression_format,omitempty"`
	FramesWithMatches []int             `json:"frames_with_matches,omitempty"`
}
