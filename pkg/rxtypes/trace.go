package rxtypes

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
// Both numbers are the line's position in the file whenever the scan
// could work it out, which is every completed scan: the workers count
// newlines as they read, so a file split across chunks is numbered as
// one file. A scan cut short by a max_results cap can leave a line
// unnumbered, and AbsoluteLineNumber is then -1 while
// RelativeLineNumber holds the number ripgrep gave it inside its chunk.
type ContextLine struct {
	RelativeLineNumber int    `json:"relative_line_number"`
	AbsoluteLineNumber int    `json:"absolute_line_number"`
	LineText           string `json:"line_text"`
	AbsoluteOffset     int64  `json:"absolute_offset"`
}

// Match is a single matched line returned by the trace engine.
//
// RelativeLineNumber and AbsoluteLineNumber follow the same rule as
// ContextLine: both hold the line's position in the file for a
// completed scan, and only a scan a cap cut short can leave
// AbsoluteLineNumber at -1.
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
// every schema-documented field must emit
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
	Path          []string                 `json:"path" nullable:"false"`
	Time          float64                  `json:"time"`
	Patterns      map[string]string        `json:"patterns"`
	Files         map[string]string        `json:"files"`
	Matches       []Match                  `json:"matches" nullable:"false"`
	ScannedFiles  []string                 `json:"scanned_files" nullable:"false"`
	SkippedFiles  []string                 `json:"skipped_files" nullable:"false"`
	MaxResults    *int                     `json:"max_results"`
	FileChunks    map[string]int           `json:"file_chunks"`
	ContextLines  map[string][]ContextLine `json:"context_lines"`
	BeforeContext *int                     `json:"before_context"`
	AfterContext  *int                     `json:"after_context"`
	CLICommand    *string                  `json:"cli_command"`
}
