package models

// TraceRequest represents a request to search for patterns in files
type TraceRequest struct {
	Paths          []string          `json:"paths" validate:"required,min=1"`
	Patterns       []string          `json:"patterns" validate:"required,min=1"`
	BeforeContext  int               `json:"before_context,omitempty"`
	AfterContext   int               `json:"after_context,omitempty"`
	MaxResults     int               `json:"max_results,omitempty"`
	CaseSensitive  bool              `json:"case_sensitive,omitempty"`
	RegexMode      bool              `json:"regex_mode,omitempty"`
	IncludeContext bool              `json:"include_context,omitempty"`
	UseIndex       *bool             `json:"use_index,omitempty"` // nil = auto, true = force, false = disable
	SkipBinary     bool              `json:"skip_binary,omitempty"`
	MaxFileSize    *int64            `json:"max_file_size,omitempty"`
	Hooks          *TraceHooks       `json:"hooks,omitempty"`
}

// TraceHooks specifies webhook URLs for trace events
type TraceHooks struct {
	OnFile     string `json:"on_file,omitempty"`
	OnMatch    string `json:"on_match,omitempty"`
	OnComplete string `json:"on_complete,omitempty"`
}

// TraceResponse represents the response from a trace operation
// CRITICAL: All JSON field names MUST be snake_case for rx-viewer compatibility
type TraceResponse struct {
	RequestID      string                   `json:"request_id"`
	Paths          []string                 `json:"paths"`
	Time           float64                  `json:"time"` // Seconds
	Patterns       map[string]string        `json:"patterns"`        // Pattern ID -> pattern text (p1 -> "ERROR")
	Files          map[string]string        `json:"files"`           // File ID -> file path (f1 -> "/path/to/file")
	Matches        []Match                  `json:"matches"`
	ScannedFiles   []string                 `json:"scanned_files"`
	SkippedFiles   []string                 `json:"skipped_files"`
	FileChunks     map[string]int           `json:"file_chunks"`     // File path -> chunk count
	ContextLines   map[string][]ContextLine `json:"context_lines"`   // Match key -> context lines
	BeforeContext  int                      `json:"before_context"`
	AfterContext   int                      `json:"after_context"`
	TotalMatches   int                      `json:"total_matches"`   // Total matches found (may be > len(Matches) if MaxResults applied)
	SearchTimeMs   float64                  `json:"search_time_ms"`  // Search time in milliseconds
	CacheHit       bool                     `json:"cache_hit"`       // Whether result was from cache
}

// NewTraceResponse creates a new TraceResponse with initialized maps
func NewTraceResponse() *TraceResponse {
	return &TraceResponse{
		Patterns:     make(map[string]string),
		Files:        make(map[string]string),
		Matches:      make([]Match, 0),
		ScannedFiles: make([]string, 0),
		SkippedFiles: make([]string, 0),
		FileChunks:   make(map[string]int),
		ContextLines: make(map[string][]ContextLine),
	}
}
