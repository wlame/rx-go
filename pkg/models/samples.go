package models

// SamplesRequest represents a request to extract samples from a file
type SamplesRequest struct {
	Path        string  `json:"path" validate:"required"`
	Lines       []int   `json:"lines,omitempty"`        // Specific line numbers to extract
	LineRanges  [][]int `json:"line_ranges,omitempty"`  // Ranges: [[start, end], ...]
	ByteOffsets []int64 `json:"byte_offsets,omitempty"` // Specific byte offsets
}

// SamplesResponse represents the response from a samples operation
type SamplesResponse struct {
	Path    string        `json:"path"`
	Samples []SampleLine  `json:"samples"`
	Index   *FileIndex    `json:"index,omitempty"` // Index used (if any)
}

// SampleLine represents a single line extracted from a file
type SampleLine struct {
	LineNumber int    `json:"line_number"`
	ByteOffset int64  `json:"byte_offset"`
	Text       string `json:"text"`
}
