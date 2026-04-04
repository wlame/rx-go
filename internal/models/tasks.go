package models

// TaskResponse is returned when a background task (index, compress) is started.
type TaskResponse struct {
	TaskID    string  `json:"task_id"`
	Status    string  `json:"status"`  // "queued", "running", "completed", "failed"
	Message   string  `json:"message"`
	Path      string  `json:"path"`
	StartedAt *string `json:"started_at"` // ISO 8601, nil when not yet started.
}

// TaskStatusResponse is returned when querying the status of a background task.
type TaskStatusResponse struct {
	TaskID      string                  `json:"task_id"`
	Status      string                  `json:"status"`       // "queued", "running", "completed", "failed"
	Path        string                  `json:"path"`
	Operation   string                  `json:"operation"`    // "compress" or "index"
	StartedAt   *string                 `json:"started_at"`   // ISO 8601, nil when not yet started.
	CompletedAt *string                 `json:"completed_at"` // ISO 8601, nil when not yet completed.
	Error       *string                 `json:"error"`        // nil when no error occurred.
	Result      map[string]interface{}  `json:"result"`       // nil when not yet completed.
}

// RequestInfo tracks a trace request for monitoring and statistics.
type RequestInfo struct {
	RequestID          string   `json:"request_id"`
	Paths              []string `json:"paths"`
	Patterns           []string `json:"patterns"`
	MaxResults         *int     `json:"max_results"`
	StartedAt          string   `json:"started_at"`           // ISO 8601
	CompletedAt        *string  `json:"completed_at"`         // ISO 8601, nil when not yet completed.
	TotalMatches       int      `json:"total_matches"`
	TotalFilesScanned  int      `json:"total_files_scanned"`
	TotalFilesSkipped  int      `json:"total_files_skipped"`
	TotalTimeMS        int      `json:"total_time_ms"`

	// Hook counters.
	HookOnFileSuccess     int `json:"hook_on_file_success"`
	HookOnFileFailed      int `json:"hook_on_file_failed"`
	HookOnMatchSuccess    int `json:"hook_on_match_success"`
	HookOnMatchFailed     int `json:"hook_on_match_failed"`
	HookOnCompleteSuccess int `json:"hook_on_complete_success"`
	HookOnCompleteFailed  int `json:"hook_on_complete_failed"`
}

// NewRequestInfo returns a RequestInfo with slice fields initialized.
func NewRequestInfo(requestID string, paths, patterns []string) RequestInfo {
	return RequestInfo{
		RequestID: requestID,
		Paths:     paths,
		Patterns:  patterns,
	}
}
