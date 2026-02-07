package models

// FileIndex represents an index for a file
// JSON field names MUST be snake_case for rx-viewer compatibility
type FileIndex struct {
	Version          int             `json:"version"`
	IndexType        IndexType       `json:"index_type"`
	SourcePath       string          `json:"source_path"`
	SourceModifiedAt string          `json:"source_modified_at"` // RFC3339 format
	SourceSizeBytes  int64           `json:"source_size_bytes"`
	CreatedAt        string          `json:"created_at"` // RFC3339 format
	BuildTimeSeconds *float64        `json:"build_time_seconds,omitempty"`
	IndexStepBytes   *int64          `json:"index_step_bytes,omitempty"`
	Analysis         *IndexAnalysis  `json:"analysis,omitempty"`
	LineIndex        [][]int64       `json:"line_index"`                      // [[line_number, byte_offset], ...]
	CompressionFormat *string        `json:"compression_format,omitempty"`    // "gzip", "zstd", etc.
	DecompressedSize  *int64         `json:"decompressed_size_bytes,omitempty"`
	TotalLines       *int            `json:"total_lines,omitempty"`
	FrameCount       *int            `json:"frame_count,omitempty"`           // For seekable zstd
	Frames           []FrameLineInfo `json:"frames,omitempty"`                // For seekable zstd
}

// IndexAnalysis contains statistical analysis of a file
type IndexAnalysis struct {
	LineCount                int        `json:"line_count"`
	EmptyLineCount           int        `json:"empty_line_count"`
	LineLengthMax            int        `json:"line_length_max"`
	LineLengthAvg            float64    `json:"line_length_avg"`
	LineLengthMedian         float64    `json:"line_length_median"`
	LineLengthP95            float64    `json:"line_length_p95"`
	LineLengthP99            float64    `json:"line_length_p99"`
	LineLengthStddev         float64    `json:"line_length_stddev"`
	LineLengthMaxLineNumber  int        `json:"line_length_max_line_number"`
	LineLengthMaxByteOffset  int64      `json:"line_length_max_byte_offset"`
	LineEnding               LineEnding `json:"line_ending"` // "LF", "CRLF", "CR", "mixed"
	AnomalyRanges            []AnomalyRange `json:"anomaly_ranges,omitempty"`
}

// FrameLineInfo represents line information for a seekable zstd frame
type FrameLineInfo struct {
	FrameIndex        int   `json:"frame_index"`
	CompressedOffset  int64 `json:"compressed_offset"`
	DecompressedOffset int64 `json:"decompressed_offset"`
	StartLine         int   `json:"start_line"`
	EndLine           int   `json:"end_line"`
}

// IndexBuildRequest represents a request to build an index
type IndexBuildRequest struct {
	Path        string  `json:"path" validate:"required"`
	Analyze     bool    `json:"analyze,omitempty"`
	Force       bool    `json:"force,omitempty"`       // Force rebuild even if cache exists
	StepBytes   *int64  `json:"step_bytes,omitempty"`  // Custom step size for sparse index
}

// IndexGetRequest represents a request to get index information
type IndexGetRequest struct {
	Path string `json:"path" validate:"required"`
}

// IndexResponse represents the response from an index operation
type IndexResponse struct {
	Success bool        `json:"success"`
	Index   *FileIndex  `json:"index,omitempty"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
}
