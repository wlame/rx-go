package models

// Match represents a single match found during trace operation
// JSON field names MUST be snake_case for rx-viewer compatibility
type Match struct {
	Pattern            string `json:"pattern"`              // Pattern ID (e.g., "p1")
	File               string `json:"file"`                 // File ID (e.g., "f1")
	Offset             int64  `json:"offset"`               // Byte offset in file
	AbsoluteLineNumber int    `json:"absolute_line_number"` // Line number from file start (-1 if unknown)
	RelativeLineNumber int    `json:"relative_line_number"` // Line number within chunk
	ChunkID            *int   `json:"chunk_id,omitempty"`   // Chunk ID if file was chunked
}

// ContextLine represents a line of context around a match
type ContextLine struct {
	LineNumber int    `json:"line_number"` // Absolute line number
	ByteOffset int64  `json:"byte_offset"` // Byte offset in file
	Text       string `json:"text"`        // Line content
}
