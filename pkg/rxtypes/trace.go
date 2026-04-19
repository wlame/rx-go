package rxtypes

// TraceRequest is the request body for POST /v1/trace and the input to
// the CLI `rx trace` command.
//
// Field semantics mirror Python's TraceRequest/parse_paths signature
// (rx-python/src/rx/trace.py). Optional flags use pointer types so that
// the zero value ("not set") is distinguishable from an explicit 0.
type TraceRequest struct {
	Patterns      []string `json:"patterns"`
	Path          []string `json:"path"`
	RgFlags       []string `json:"rg_flags,omitempty"`
	MaxResults    *int     `json:"max_results,omitempty"`
	BeforeContext *int     `json:"before_context,omitempty"`
	AfterContext  *int     `json:"after_context,omitempty"`
	NoCache       bool     `json:"no_cache,omitempty"`
	NoIndex       bool     `json:"no_index,omitempty"`

	// Optional webhook URL overrides. A nil pointer means "use the
	// environment-configured URL"; an empty string disables the hook
	// entirely for this request.
	HookOnFileURL     *string `json:"hook_on_file_url,omitempty"`
	HookOnMatchURL    *string `json:"hook_on_match_url,omitempty"`
	HookOnCompleteURL *string `json:"hook_on_complete_url,omitempty"`
}

// Submatch is a single regex match within a matched line.
//
// Submatches are derived from ripgrep's --json output and represent
// byte positions (NOT rune indices) into Match.LineText. This matches
// Python re.Match.start()/end() semantics on a bytes object.
type Submatch struct {
	Text  string `json:"text"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// ContextLine is a non-matching line shown around a match.
//
// file_chunks == 1: RelativeLineNumber IS the absolute line number.
// file_chunks >  1: RelativeLineNumber may be local to the chunk; consult
//
//	AbsoluteLineNumber (which is -1 if unknown).
type ContextLine struct {
	RelativeLineNumber int    `json:"relative_line_number"`
	AbsoluteLineNumber int    `json:"absolute_line_number"`
	LineText           string `json:"line_text"`
	AbsoluteOffset     int64  `json:"absolute_offset"`
}

// Match is a single matched line returned by the trace engine.
//
// Pattern and File are ID strings (e.g. "p1", "f1") that index into
// TraceResponse.Patterns and TraceResponse.Files respectively. This
// indirection matches Python's design and keeps the response compact
// when the same pattern/file pair is reported many times.
type Match struct {
	Pattern            string     `json:"pattern"`
	File               string     `json:"file"`
	Offset             int64      `json:"offset"`
	RelativeLineNumber *int       `json:"relative_line_number"`
	AbsoluteLineNumber int        `json:"absolute_line_number"`
	LineText           *string    `json:"line_text"`
	Submatches         []Submatch `json:"submatches"`
}

// TraceResponse is the full response shape for GET /v1/trace.
//
// Stage 9 Round 2 S2 user rule: every schema-documented field must emit
// an explicit null when unset — omitempty is only acceptable for fields
// that are "extensions" NOT part of the advertised schema. All fields
// below are documented in rx-python/src/rx/models.py::TraceResponse and
// Python emits each as either its typed value or null.
//
// CLICommand is typed as *string because Go's string zero value is the
// empty string, which JSON marshals as "" (not null). Python emits null
// for an unset CLICommand, so we use a pointer to preserve that
// distinction.
type TraceResponse struct {
	RequestID     string                   `json:"request_id"`
	Path          []string                 `json:"path"`
	Time          float64                  `json:"time"`
	Patterns      map[string]string        `json:"patterns"`
	Files         map[string]string        `json:"files"`
	Matches       []Match                  `json:"matches"`
	ScannedFiles  []string                 `json:"scanned_files"`
	SkippedFiles  []string                 `json:"skipped_files"`
	MaxResults    *int                     `json:"max_results"`
	FileChunks    map[string]int           `json:"file_chunks"`
	ContextLines  map[string][]ContextLine `json:"context_lines"`
	BeforeContext *int                     `json:"before_context"`
	AfterContext  *int                     `json:"after_context"`
	CLICommand    *string                  `json:"cli_command"`
}
