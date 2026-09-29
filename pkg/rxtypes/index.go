package rxtypes

// FileType mirrors Python's FileType enum (rx-python/src/rx/models.py).
// The string values are emitted verbatim in JSON.
type FileType string

// FileType constants used in UnifiedFileIndex.
const (
	FileTypeText         FileType = "text"
	FileTypeBinary       FileType = "binary"
	FileTypeCompressed   FileType = "compressed"
	FileTypeSeekableZstd FileType = "seekable_zstd"
)

// FrameLineInfo describes a single frame inside a seekable-zstd file:
// where it lives in the compressed stream, where its decompressed
// bytes land, and which lines it contains.
//
// 1-based line numbers match Python; 0-based frame index matches the
// zstd frame ordering.
type FrameLineInfo struct {
	Index              int   `json:"index"`
	CompressedOffset   int64 `json:"compressed_offset"`
	CompressedSize     int64 `json:"compressed_size"`
	DecompressedOffset int64 `json:"decompressed_offset"`
	DecompressedSize   int64 `json:"decompressed_size"`
	FirstLine          int64 `json:"first_line"`
	LastLine           int64 `json:"last_line"`
	LineCount          int64 `json:"line_count"`
}

// UnifiedFileIndex is the single cache schema used for all indexable
// files (text, compressed, seekable-zstd). Fields are nullable (pointer
// types) when they only apply to a subset of file types; the Python
// Pydantic model uses Optional[...] for exactly the same reason.
//
// Field order and JSON tag names match
// rx-python/src/rx/models.py::UnifiedFileIndex exactly. Do NOT reorder
// without updating the cross-format cache compatibility tests.
type UnifiedFileIndex struct {
	// Version & identification
	Version          int     `json:"version"`
	SourcePath       string  `json:"source_path"`
	SourceModifiedAt string  `json:"source_modified_at"`
	SourceSizeBytes  int64   `json:"source_size_bytes"`
	CreatedAt        string  `json:"created_at"`
	BuildTimeSeconds float64 `json:"build_time_seconds"`

	// Source identity. SourceModifiedAt and SourceSizeBytes alone
	// cannot tell a rewritten file from an untouched one: a copy that
	// restores the mtime, or an in-place edit that keeps the byte
	// count, leaves both unchanged. The inode number and the
	// inode-change time close that gap. Nothing can set ctime through
	// utime, so any write to the file moves it.
	//
	// Both are pointers because a filesystem may not report them and
	// because rx-python writes null when it cannot.
	SourceInode     *uint64 `json:"source_inode"`
	SourceChangedAt *string `json:"source_changed_at"`

	// SourceFingerprint is a digest of the file size plus the first and
	// last 64 KiB. It is what catches a rewrite on a filesystem whose
	// ctime does not move, which is the common case inside containers
	// and on some network mounts.
	SourceFingerprint *string `json:"source_fingerprint"`

	// File type information
	FileType          FileType `json:"file_type"`
	CompressionFormat *string  `json:"compression_format"`
	IsText            bool     `json:"is_text"`

	// Basic metadata
	Permissions *string `json:"permissions"`
	Owner       *string `json:"owner"`

	// Line indexing
	LineIndex      []LineIndexEntry `json:"line_index"`
	IndexStepBytes *int64           `json:"index_step_bytes"`

	// Compression-specific
	DecompressedSizeBytes *int64   `json:"decompressed_size_bytes"`
	CompressionRatio      *float64 `json:"compression_ratio"`

	// Seekable-zstd specific
	FrameCount      *int   `json:"frame_count"`
	FrameSizeTarget *int64 `json:"frame_size_target"`
	// Frames: nullable schema field per Python model. // rule — documented schema fields must emit null (or their value),
	// never be absent. Using a typed `*[]FrameLineInfo` pointer gives us
	// three distinct states (nil = "null", empty = "[]", populated = [...])
	// matching Python's Optional[list[FrameLineInfo]] = None semantics.
	Frames *[]FrameLineInfo `json:"frames"`

	// Analysis flag
	AnalysisPerformed bool `json:"analysis_performed"`

	// AnalysisWindowLines and AnalysisDetectorSet record what the
	// analysis ran with: the sliding-window size, and every detector as
	// "name@version", sorted and comma-joined. A cached analysis answers
	// a later request only when both match it. Both are null when no
	// analysis was performed, and in an index written before they
	// existed.
	AnalysisWindowLines *int    `json:"analysis_window_lines"`
	AnalysisDetectorSet *string `json:"analysis_detector_set"`

	// Analysis results (only populated when AnalysisPerformed == true)
	LineCount               *int64   `json:"line_count"`
	EmptyLineCount          *int64   `json:"empty_line_count"`
	LineLengthMax           *int64   `json:"line_length_max"`
	LineLengthAvg           *float64 `json:"line_length_avg"`
	LineLengthMedian        *float64 `json:"line_length_median"`
	LineLengthP95           *float64 `json:"line_length_p95"`
	LineLengthP99           *float64 `json:"line_length_p99"`
	LineLengthStddev        *float64 `json:"line_length_stddev"`
	LineLengthMaxLineNumber *int64   `json:"line_length_max_line_number"`
	LineLengthMaxByteOffset *int64   `json:"line_length_max_byte_offset"`
	LineEnding              *string  `json:"line_ending"`

	// Anomaly detection (populated only when analysis_performed=true).
	// Python emits null when analysis hasn't happened — Go matches via
	// pointer-typed slice/map so nil → JSON null, empty → JSON [] / {}.
	Anomalies      *[]AnomalyRangeResult `json:"anomalies"`
	AnomalySummary map[string]int        `json:"anomaly_summary"`

	// Prefix pattern detection (only when analysis_performed=true)
	PrefixPattern  *string  `json:"prefix_pattern"`
	PrefixRegex    *string  `json:"prefix_regex"`
	PrefixCoverage *float64 `json:"prefix_coverage"`
	PrefixLength   *int     `json:"prefix_length"`
}

// AnomalyRangeResult is a single anomaly entry inside UnifiedFileIndex.
//
// Entries come from the analyzer detectors when an index is built with
// analysis on. The same type lets cached files written by Python
// deserialize cleanly.
type AnomalyRangeResult struct {
	StartLine   int64   `json:"start_line"`
	EndLine     int64   `json:"end_line"`
	StartOffset int64   `json:"start_offset"`
	EndOffset   int64   `json:"end_offset"`
	Severity    float64 `json:"severity"`
	Category    string  `json:"category"`
	Description string  `json:"description"`
	Detector    string  `json:"detector"`
}

// IndexRequest is the body for POST /v1/index (background task).
//
// AnalyzeWindowLines is the optional sliding-window size the analyzer's
// detectors see when Analyze=true. Null or 0 means "not set" — the
// server falls through to analyzer.ResolveWindowLines precedence (env
// var → compiled-in default). A negative value is a mistake the caller
// made rather than a way to spell "not set", so it is refused.
//
// It is a pointer with omitempty, the shape Threshold beside it uses:
// on a request body that is how a field says "may be omitted", and huma
// marks it optional in the schema accordingly. The design contract's ban
// on omitempty is about response fields, where a reader has to be able
// to tell an absent key from a null one; nothing reads a request back.
// rx-python declares the same field as `int | None`.
type IndexRequest struct {
	Path               string `json:"path"`
	Force              bool   `json:"force,omitempty"`
	Analyze            bool   `json:"analyze,omitempty"`
	Threshold          *int   `json:"threshold,omitempty"` // MB; nil = use env default
	AnalyzeWindowLines *int   `json:"analyze_window_lines,omitempty"`
}

// IndexResponse is the body of GET /v1/index: the cached index of one
// file, projected for a client. It is a view of UnifiedFileIndex with
// client-facing names (path, size_bytes), the line-length statistics
// grouped, and counts precomputed. IndexTaskResult extends it with the
// fields a finished POST /v1/index task adds.
//
// Every field is always present; a value that does not apply is null.
// The shape follows rx-python's _unified_index_to_dict, except that
// rx-python leaves longest_line out when it has no line number.
type IndexResponse struct {
	Path              string   `json:"path" doc:"The indexed file."`
	FileType          FileType `json:"file_type" enum:"text,binary,compressed,seekable_zstd"`
	SizeBytes         int64    `json:"size_bytes" doc:"Size of the file on disk."`
	CreatedAt         string   `json:"created_at" doc:"When the index was built (ISO 8601)."`
	BuildTimeSeconds  float64  `json:"build_time_seconds"`
	AnalysisPerformed bool     `json:"analysis_performed" doc:"Whether the line statistics and anomalies were computed."`

	// LineIndex is never null: an index without checkpoints answers [].
	LineIndex    []LineIndexEntry `json:"line_index" nullable:"false"`
	IndexEntries int              `json:"index_entries" doc:"Number of line_index entries."`

	LineCount      *int64           `json:"line_count"`
	EmptyLineCount *int64           `json:"empty_line_count"`
	LineEnding     *string          `json:"line_ending" doc:"LF, CRLF, CR or mixed; null without analysis."`
	LineLength     *LineLengthStats `json:"line_length" doc:"Line-length statistics; null without analysis."`
	LongestLine    *LongestLine     `json:"longest_line" doc:"Where the longest line is; null without analysis."`

	CompressionFormat     *string  `json:"compression_format"`
	DecompressedSizeBytes *int64   `json:"decompressed_size_bytes"`
	CompressionRatio      *float64 `json:"compression_ratio"`

	AnomalyCount int `json:"anomaly_count"`
	// AnomalySummary counts anomalies per detector name; null without
	// analysis. Tagged nullable because huma declares a map as a
	// non-null object otherwise.
	AnomalySummary map[string]int        `json:"anomaly_summary" nullable:"true"`
	Anomalies      *[]AnomalyRangeResult `json:"anomalies"`

	CLICommand string `json:"cli_command" doc:"The rx command that gives this answer."`
}

// IndexTaskResult is the result of a completed POST /v1/index task: the
// index it built or reused, plus where it is stored.
//
// IndexResponse is embedded, so its fields sit at the top level of the
// JSON object (encoding/json and huma both flatten an embedded struct).
type IndexTaskResult struct {
	IndexResponse
	Success   bool   `json:"success"`
	IndexPath string `json:"index_path" doc:"Where the index is stored in the cache."`
}

// LineLengthStats groups the line-length statistics of an analyzed
// index, in bytes.
//
// The `_` field makes the schema itself nullable: huma cannot mark a
// field that refers to a named object as nullable, so the object
// declares it once for every field that refers to it.
type LineLengthStats struct {
	_      struct{} `nullable:"true"`
	Max    int64    `json:"max"`
	Avg    *float64 `json:"avg"`
	Median *float64 `json:"median"`
	P95    *float64 `json:"p95"`
	P99    *float64 `json:"p99"`
	Stddev *float64 `json:"stddev"`
}

// LongestLine locates the longest line of an analyzed index. Nullable
// for the reason LineLengthStats gives.
type LongestLine struct {
	_          struct{} `nullable:"true"`
	LineNumber int64    `json:"line_number"`
	ByteOffset int64    `json:"byte_offset"`
}
