// Package rgjson parses ripgrep JSON output (rg --json) into structured Go types.
//
// Ripgrep's --json flag emits one newline-delimited JSON object per event. Event types
// are: "begin" (file started), "match" (pattern hit), "context" (surrounding line),
// "end" (file finished with stats), and "summary" (overall stats). This package maps
// each event type to a concrete Go struct so the rest of the codebase works with typed
// data instead of raw JSON.
package rgjson

// MessageType enumerates the event types emitted by rg --json.
type MessageType string

const (
	TypeBegin   MessageType = "begin"
	TypeMatch   MessageType = "match"
	TypeContext MessageType = "context"
	TypeEnd     MessageType = "end"
	TypeSummary MessageType = "summary"
)

// RgMessage is the envelope for every rg --json line. Callers inspect Type and then
// read the corresponding typed field (Begin, Match, Context, End, or Summary).
type RgMessage struct {
	Type    MessageType  `json:"type"`
	Begin   *RgBegin     `json:"-"` // populated when Type == "begin"
	Match   *RgMatch     `json:"-"` // populated when Type == "match"
	Context *RgContext    `json:"-"` // populated when Type == "context"
	End     *RgEnd       `json:"-"` // populated when Type == "end"
	Summary *RgSummary   `json:"-"` // populated when Type == "summary"
}

// --- text/path wrappers (rg wraps strings in {"text": "..."}) ---

// RgText wraps a text field in rg JSON output.
type RgText struct {
	Text string `json:"text"`
}

// RgPath wraps a file path in rg JSON output. Text may be empty for stdin.
type RgPath struct {
	Text string `json:"text"`
}

// --- submatch ---

// RgSubmatch represents a single regex match within a line.
type RgSubmatch struct {
	Match RgText `json:"match"` // The matched text, wrapped as {"text": "..."}.
	Start int    `json:"start"` // Byte offset from start of line where match begins.
	End   int    `json:"end"`   // Byte offset from start of line where match ends.
}

// --- per-event data payloads ---

// RgBegin is the data payload for "begin" events, emitted when rg starts a file.
type RgBegin struct {
	Path RgPath `json:"path"`
}

// RgMatch is the data payload for "match" events.
type RgMatch struct {
	Path           RgPath       `json:"path"`
	Lines          RgText       `json:"lines"`           // The line(s) containing the match.
	LineNumber     int          `json:"line_number"`      // 1-indexed line number.
	AbsoluteOffset int          `json:"absolute_offset"` // Byte offset from start of file to this line.
	Submatches     []RgSubmatch `json:"submatches"`
}

// RgContext is the data payload for "context" events (non-matching surrounding lines).
type RgContext struct {
	Path           RgPath       `json:"path"`
	Lines          RgText       `json:"lines"`
	LineNumber     int          `json:"line_number"`
	AbsoluteOffset int          `json:"absolute_offset"`
	Submatches     []RgSubmatch `json:"submatches"` // Always empty for context lines.
}

// RgStats holds per-file or overall search statistics.
type RgStats struct {
	Elapsed            map[string]interface{} `json:"elapsed"` // {"secs": N, "nanos": N, "human": "..."}
	Searches           int                    `json:"searches"`
	SearchesWithMatch  int                    `json:"searches_with_match"`
	BytesSearched      int                    `json:"bytes_searched"`
	BytesPrinted       int                    `json:"bytes_printed"`
	MatchedLines       int                    `json:"matched_lines"`
	Matches            int                    `json:"matches"`
}

// RgEnd is the data payload for "end" events, emitted when rg finishes a file.
type RgEnd struct {
	Path         RgPath  `json:"path"`
	BinaryOffset *int    `json:"binary_offset"` // nil when file is not binary.
	Stats        RgStats `json:"stats"`
}

// RgSummary is the data payload for "summary" events with overall search statistics.
type RgSummary struct {
	ElapsedTotal map[string]interface{} `json:"elapsed_total"`
	Stats        RgStats                `json:"stats"`
}
