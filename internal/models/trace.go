package models

import "fmt"

// PatternID returns a pattern identifier for 1-based index i, e.g. PatternID(1) == "p1".
func PatternID(i int) string { return fmt.Sprintf("p%d", i) }

// FileID returns a file identifier for 1-based index i, e.g. FileID(1) == "f1".
func FileID(i int) string { return fmt.Sprintf("f%d", i) }

// Submatch represents a single regex match within a line.
type Submatch struct {
	Text  string `json:"text"`  // The actual matched text.
	Start int    `json:"start"` // Byte offset from start of line where match begins.
	End   int    `json:"end"`   // Byte offset from start of line where match ends.
}

// ContextLine represents a single line of context around a match.
type ContextLine struct {
	RelativeLineNumber int    `json:"relative_line_number"` // Line number relative to chunk (1-indexed).
	AbsoluteLineNumber int    `json:"absolute_line_number"` // Absolute line number from file start, or -1 if unknown.
	LineText           string `json:"line_text"`            // The actual line content.
	AbsoluteOffset     int    `json:"absolute_offset"`      // Byte offset from start of file.
}

// Match represents a single search result with pattern/file IDs and rich metadata.
type Match struct {
	Pattern            string      `json:"pattern"`                        // Pattern ID (e.g. "p1").
	File               string      `json:"file"`                           // File ID (e.g. "f1").
	Offset             int         `json:"offset"`                         // Absolute byte offset in file where the matched LINE starts.
	RelativeLineNumber *int        `json:"relative_line_number"`           // Line number relative to chunk start (1-indexed), nil if unknown.
	AbsoluteLineNumber int         `json:"absolute_line_number"`           // Absolute line number from file start, -1 if unknown.
	LineText           *string     `json:"line_text"`                      // The matched line content (nil when not available).
	Submatches         *[]Submatch `json:"submatches"`                     // Submatch positions within the line (nil when not available).
}

// NewMatch creates a Match with sensible zero-value defaults for nullable fields.
func NewMatch(pattern, file string, offset int) Match {
	return Match{
		Pattern:            pattern,
		File:               file,
		Offset:             offset,
		AbsoluteLineNumber: -1,
	}
}

// ParseResult is the internal result from the trace engine before being converted
// to an API response. It groups pattern IDs, file IDs, raw matches, and context.
type ParseResult struct {
	Patterns     map[string]string              `json:"patterns"`      // {"p1": "pattern1", ...}
	Files        map[string]string              `json:"files"`         // {"f1": "/path/to/file", ...}
	Matches      []map[string]interface{}        `json:"matches"`       // Raw match dicts.
	ScannedFiles []string                       `json:"scanned_files"` // Files that were scanned.
	SkippedFiles []string                       `json:"skipped_files"` // Files that were skipped.
	FileChunks   map[string]int                 `json:"file_chunks"`   // {"f1": chunk_count, ...}
	ContextLines map[string][]ContextLine       `json:"context_lines"` // {"p1:f1:offset": [...], ...}
	BeforeCtx    int                            `json:"before_context"`
	AfterCtx     int                            `json:"after_context"`
}

// NewParseResult returns a ParseResult with all slice/map fields initialized to
// non-nil empty values so they serialize as [] / {} rather than null.
func NewParseResult() ParseResult {
	return ParseResult{
		Patterns:     make(map[string]string),
		Files:        make(map[string]string),
		Matches:      []map[string]interface{}{},
		ScannedFiles: []string{},
		SkippedFiles: []string{},
		FileChunks:   make(map[string]int),
		ContextLines: make(map[string][]ContextLine),
	}
}

// TraceResponse is the top-level response returned by the trace endpoint / CLI.
// Field names and JSON keys match the Python API contract exactly.
type TraceResponse struct {
	RequestID    string                          `json:"request_id"`
	Path         []string                        `json:"path"`
	Time         float64                         `json:"time"`
	Patterns     map[string]string               `json:"patterns"`
	Files        map[string]string               `json:"files"`
	Matches      []Match                         `json:"matches"`
	ScannedFiles []string                        `json:"scanned_files"`
	SkippedFiles []string                        `json:"skipped_files"`
	MaxResults   *int                            `json:"max_results"`

	// File chunking metadata — how files were processed.
	FileChunks *map[string]int `json:"file_chunks"`

	// Context lines stored with composite key "p1:f1:100" -> [ContextLine, ...]
	ContextLines *map[string][]ContextLine `json:"context_lines"`
	BeforeCtx    *int                      `json:"before_context"`
	AfterCtx     *int                      `json:"after_context"`

	// CLI equivalent command.
	CLICommand *string `json:"cli_command"`
}

// NewTraceResponse creates a TraceResponse with all slice/map fields initialized
// to non-nil empty values so they serialize as [] / {} rather than null.
func NewTraceResponse(requestID string, paths []string) TraceResponse {
	return TraceResponse{
		RequestID:    requestID,
		Path:         paths,
		Patterns:     make(map[string]string),
		Files:        make(map[string]string),
		Matches:      []Match{},
		ScannedFiles: []string{},
		SkippedFiles: []string{},
	}
}
