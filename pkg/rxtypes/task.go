package rxtypes

// TaskResponse is returned by POST /v1/index and POST /v1/compress when
// a background task is accepted. The client polls GET /v1/tasks/{id}
// with the returned TaskID until completion.
type TaskResponse struct {
	TaskID    string  `json:"task_id"`
	Status    string  `json:"status"`
	Message   string  `json:"message"`
	Path      string  `json:"path"`
	StartedAt *string `json:"started_at"`
}

// TaskStatusResponse is returned by GET /v1/tasks/{task_id}.
//
// Operation is "compress" or "index". Status transitions:
// "queued" → "running" → ("completed" | "failed"). Result is nil until
// Status == "completed"; Error is nil unless Status == "failed".
type TaskStatusResponse struct {
	TaskID      string         `json:"task_id"`
	Status      string         `json:"status"`
	Path        string         `json:"path"`
	Operation   string         `json:"operation"`
	StartedAt   *string        `json:"started_at"`
	CompletedAt *string        `json:"completed_at"`
	Error       *string        `json:"error"`
	Result      map[string]any `json:"result"`
}

// CompressRequest is the body for POST /v1/compress (background task).
//
// FrameSize is a human-readable size string (e.g. "4M", "16MB") parsed
// by internal/clicommand. CompressionLevel is 1..22 per zstd convention.
//
// Stage 9 Round 2 S2 rule: schema fields never omit. Python's
// CompressRequest emits output_path as null and force as false on default
// input, so Go preserves both keys in the JSON output.
type CompressRequest struct {
	InputPath        string  `json:"input_path"`
	OutputPath       *string `json:"output_path"`
	FrameSize        string  `json:"frame_size"`
	CompressionLevel int     `json:"compression_level"`
	BuildIndex       bool    `json:"build_index"`
	Force            bool    `json:"force"`
}

// CompressResponse is the terminal payload of POST /v1/compress tasks.
type CompressResponse struct {
	Success          bool     `json:"success"`
	InputPath        string   `json:"input_path"`
	OutputPath       *string  `json:"output_path"`
	CompressedSize   *int64   `json:"compressed_size"`
	DecompressedSize *int64   `json:"decompressed_size"`
	CompressionRatio *float64 `json:"compression_ratio"`
	FrameCount       *int     `json:"frame_count"`
	TotalLines       *int64   `json:"total_lines"`
	IndexBuilt       bool     `json:"index_built"`
	TimeSeconds      *float64 `json:"time_seconds"`
	Error            *string  `json:"error"`
}
