package models

// IndexType classifies the kind of file index.
type IndexType string

const (
	IndexTypeRegular     IndexType = "regular"
	IndexTypeCompressed  IndexType = "compressed"
	IndexTypeSeekableZstd IndexType = "seekable_zstd"
)

// FileType classifies a file for unified indexing.
type FileType string

const (
	FileTypeText         FileType = "text"
	FileTypeBinary       FileType = "binary"
	FileTypeCompressed   FileType = "compressed"
	FileTypeSeekableZstd FileType = "seekable_zstd"
)

// IndexAnalysis holds analysis statistics computed during index creation for text files.
// All fields are pointers because they may not be computed (nullable in the JSON contract).
type IndexAnalysis struct {
	LineCount            *int     `json:"line_count"`
	EmptyLineCount       *int     `json:"empty_line_count"`
	LineLengthMax        *int     `json:"line_length_max"`
	LineLengthAvg        *float64 `json:"line_length_avg"`
	LineLengthMedian     *float64 `json:"line_length_median"`
	LineLengthP95        *float64 `json:"line_length_p95"`
	LineLengthP99        *float64 `json:"line_length_p99"`
	LineLengthStddev     *float64 `json:"line_length_stddev"`
	LineLengthMaxLineNum *int     `json:"line_length_max_line_number"`
	LineLengthMaxOffset  *int     `json:"line_length_max_byte_offset"`
	LineEnding           *string  `json:"line_ending"`
}

// FrameLineInfo describes a single frame in a seekable zstd file with its line mapping.
type FrameLineInfo struct {
	Index              int `json:"index"`               // 0-based frame index.
	CompressedOffset   int `json:"compressed_offset"`   // Byte offset in compressed file.
	CompressedSize     int `json:"compressed_size"`     // Compressed frame size in bytes.
	DecompressedOffset int `json:"decompressed_offset"` // Byte offset in decompressed stream.
	DecompressedSize   int `json:"decompressed_size"`   // Decompressed frame size in bytes.
	FirstLine          int `json:"first_line"`          // First line number in frame (1-based).
	LastLine           int `json:"last_line"`           // Last line number in frame (1-based).
	LineCount          int `json:"line_count"`          // Number of lines in frame.
}

// FileIndex is the unified file index for all file types.
// It stores line-offset mappings, optional frame info for seekable zstd, and analysis stats.
type FileIndex struct {
	// Version and type
	Version   int       `json:"version"`
	IndexType IndexType `json:"index_type"`

	// Source file information
	SourcePath       string `json:"source_path"`
	SourceModifiedAt string `json:"source_modified_at"` // ISO 8601
	SourceSizeBytes  int    `json:"source_size_bytes"`

	// Creation metadata
	CreatedAt        string   `json:"created_at"`
	BuildTimeSeconds *float64 `json:"build_time_seconds"`

	// Regular file index fields
	IndexStepBytes *int           `json:"index_step_bytes"`
	Analysis       *IndexAnalysis `json:"analysis"`

	// Line index: each entry is [line_number, byte_offset] or [line_number, byte_offset, frame_index].
	LineIndex [][]int `json:"line_index"`

	// Compressed file fields
	CompressionFormat    *string `json:"compression_format"`
	DecompressedSizeBytes *int   `json:"decompressed_size_bytes"`
	TotalLines           *int    `json:"total_lines"`
	LineSampleInterval   *int    `json:"line_sample_interval"`

	// Seekable zstd specific
	FrameCount      *int            `json:"frame_count"`
	FrameSizeTarget *int            `json:"frame_size_target"`
	Frames          *[]FrameLineInfo `json:"frames"` // pointer so nil serializes as null

	// Python compatibility: Python stores analysis fields at the top level rather
	// than nested inside an "analysis" object. These fields capture Python's flat
	// layout so Go can read Python-built indexes.
	PyLineCount *int `json:"line_count,omitempty"`
}

// GetLineCount returns the line count from whichever location it's stored:
// Go's nested Analysis struct or Python's top-level field.
func (f *FileIndex) GetLineCount() *int {
	if f.Analysis != nil && f.Analysis.LineCount != nil {
		return f.Analysis.LineCount
	}
	return f.PyLineCount
}

// NewFileIndex returns a FileIndex with the line_index slice initialized to a non-nil
// empty value so it serializes as [] rather than null.
func NewFileIndex(version int, indexType IndexType, sourcePath, modifiedAt string, sizeBytes int) FileIndex {
	return FileIndex{
		Version:          version,
		IndexType:        indexType,
		SourcePath:       sourcePath,
		SourceModifiedAt: modifiedAt,
		SourceSizeBytes:  sizeBytes,
		LineIndex:        [][]int{},
	}
}
