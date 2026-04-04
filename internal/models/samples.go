package models

// SamplesResponse is returned by the samples endpoint / CLI.
// It provides context lines around byte offsets or line numbers in a file.
type SamplesResponse struct {
	Path              string              `json:"path"`
	Offsets           map[string]int      `json:"offsets"`            // {"byte_offset": line_number, ...}
	Lines             map[string]int      `json:"lines"`              // {"line_number": byte_offset, ...}
	BeforeCtx         int                 `json:"before_context"`
	AfterCtx          int                 `json:"after_context"`
	Samples           map[string][]string `json:"samples"`            // {"offset_or_line": [context_lines...], ...}
	IsCompressed      bool                `json:"is_compressed"`
	CompressionFormat *string             `json:"compression_format"` // nil serializes as null
	CLICommand        *string             `json:"cli_command"`        // nil serializes as null
}

// NewSamplesResponse creates a SamplesResponse with all map/slice fields initialized
// to non-nil empty values so they serialize as {} / [] rather than null.
func NewSamplesResponse(path string, before, after int) SamplesResponse {
	return SamplesResponse{
		Path:      path,
		Offsets:   make(map[string]int),
		Lines:     make(map[string]int),
		BeforeCtx: before,
		AfterCtx:  after,
		Samples:   make(map[string][]string),
	}
}
