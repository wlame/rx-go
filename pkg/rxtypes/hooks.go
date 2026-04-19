package rxtypes

// Hook event types. These match the exact strings emitted by Python
// in the 'event' field of each webhook payload.
const (
	HookEventFileScanned   = "file_scanned"
	HookEventMatchFound    = "match_found"
	HookEventTraceComplete = "trace_complete"
)

// FileScannedPayload is the body POSTed to the on_file_scanned webhook.
type FileScannedPayload struct {
	Event         string `json:"event"`
	RequestID     string `json:"request_id"`
	FilePath      string `json:"file_path"`
	FileSizeBytes int64  `json:"file_size_bytes"`
	ScanTimeMS    int    `json:"scan_time_ms"`
	MatchesCount  int    `json:"matches_count"`
}

// MatchFoundPayload is the body POSTed to the on_match_found webhook.
// Fired once per match when the hook is enabled (expensive — typically
// gated by --max-results to cap volume).
type MatchFoundPayload struct {
	Event      string `json:"event"`
	RequestID  string `json:"request_id"`
	FilePath   string `json:"file_path"`
	Pattern    string `json:"pattern"`
	Offset     int64  `json:"offset"`
	LineNumber *int64 `json:"line_number"`
}

// TraceCompletePayload is the body POSTed to on_trace_complete — once
// per full trace request when the hook is enabled.
type TraceCompletePayload struct {
	Event             string `json:"event"`
	RequestID         string `json:"request_id"`
	Paths             string `json:"paths"`    // comma-joined
	Patterns          string `json:"patterns"` // comma-joined
	TotalFilesScanned int    `json:"total_files_scanned"`
	TotalFilesSkipped int    `json:"total_files_skipped"`
	TotalMatches      int    `json:"total_matches"`
	TotalTimeMS       int    `json:"total_time_ms"`
}
