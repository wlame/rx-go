package rgjson

// EventType represents the type of ripgrep JSON event
type EventType string

const (
	EventTypeBegin   EventType = "begin"
	EventTypeMatch   EventType = "match"
	EventTypeContext EventType = "context"
	EventTypeEnd     EventType = "end"
	EventTypeSummary EventType = "summary"
)

// Event represents a ripgrep JSON event
type Event struct {
	Type EventType `json:"type"`
	Data EventData `json:"data"`
}

// EventData contains the event data
type EventData struct {
	Path           *PathData      `json:"path,omitempty"`
	Lines          *LinesData     `json:"lines,omitempty"`
	LineNumber     *int64         `json:"line_number,omitempty"`
	AbsoluteOffset int64          `json:"absolute_offset"`
	Submatches     []SubmatchData `json:"submatches,omitempty"`
}

// PathData contains file path information
type PathData struct {
	Text string `json:"text"`
}

// LinesData contains line content
type LinesData struct {
	Text string `json:"text"`
}

// SubmatchData contains submatch information
type SubmatchData struct {
	Match MatchData `json:"match"`
	Start int64     `json:"start"`
	End   int64     `json:"end"`
}

// MatchData contains matched text
type MatchData struct {
	Text string `json:"text"`
}
