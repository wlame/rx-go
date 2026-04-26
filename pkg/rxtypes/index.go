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
	// Frames: nullable schema field per Python model. Stage 9 Round 2 S2
	// rule — documented schema fields must emit null (or their value),
	// never be absent. Using a typed `*[]FrameLineInfo` pointer gives us
	// three distinct states (nil = "null", empty = "[]", populated = [...])
	// matching Python's Optional[list[FrameLineInfo]] = None semantics.
	Frames *[]FrameLineInfo `json:"frames"`

	// Analysis flag
	AnalysisPerformed bool `json:"analysis_performed"`

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
// The Go port ships without built-in analyzers (per user instructions),
// so this type will almost always be empty at v1. It's defined here so
// cached files written by Python deserialise cleanly.
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
// AnalyzeWindowLines is the optional sliding-window size used by the
// analyzer coordinator when Analyze=true. Zero / missing means "not
// set" — the server falls through to analyzer.ResolveWindowLines
// precedence (CLI flag → env var → compiled-in default).
type IndexRequest struct {
	Path               string `json:"path"`
	Force              bool   `json:"force,omitempty"`
	Analyze            bool   `json:"analyze,omitempty"`
	Threshold          *int   `json:"threshold,omitempty"`            // MB; nil = use env default
	AnalyzeWindowLines int    `json:"analyze_window_lines,omitempty"` // 0 = use resolver default
}

// IndexResponse is the body returned by GET /v1/index (synchronous)
// and the terminal payload of POST /v1/index tasks.
type IndexResponse struct {
	Success         bool     `json:"success"`
	Path            string   `json:"path"`
	IndexPath       *string  `json:"index_path"`
	LineCount       *int64   `json:"line_count"`
	FileSize        *int64   `json:"file_size"`
	CheckpointCount *int     `json:"checkpoint_count"`
	TimeSeconds     *float64 `json:"time_seconds"`
	Error           *string  `json:"error"`
}
