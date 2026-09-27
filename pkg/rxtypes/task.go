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
	TaskID      string     `json:"task_id"`
	Status      string     `json:"status"`
	Path        string     `json:"path"`
	Operation   string     `json:"operation"`
	StartedAt   *string    `json:"started_at"`
	CompletedAt *string    `json:"completed_at"`
	Error       *string    `json:"error"`
	Result      TaskResult `json:"result"`
}

// TaskResult is the result of a background task: an IndexTaskResult
// for an "index" task, a CompressTaskResult for a "compress" task, and
// nil (JSON null) until the task completes.
//
// It is a defined type over `any` rather than `any` itself so that the
// OpenAPI generator can tell this field apart from every other `any` in
// the wire types and publish it as "IndexTaskResult, CompressTaskResult
// or null" (internal/webapi registers that schema for this type).
// encoding/json treats it like `any`: it marshals whatever value it
// holds, and a Go client that decodes a TaskStatusResponse gets the
// result as a map[string]any, to decode again into the type its
// Operation names.
type TaskResult any

// CompressRequest is the body for POST /v1/compress (background task).
//
// Only InputPath is required. Every other field may be left out, and
// then takes the default of the matching `rx compress` flag, so a task
// started over HTTP compresses exactly the way the CLI does without the
// flag:
//
//	frame_size        --frame-size   "4M"
//	compression_level --level        3
//	build_index       --build-index  true
//	force             --force        false
//	output_path       --output       null, meaning input_path + ".zst"
//
// The defaults are written as `default:"..."` struct tags because that
// is where huma reads them: it fills an absent field with the tag's
// value before the handler runs, and publishes the value in the OpenAPI
// schema. `required:"false"` is what marks the field optional in that
// schema; without it huma treats every field that has no omitempty as
// required. A test in internal/clicommand compares these tags with the
// cobra flag defaults, so the two cannot drift apart.
//
// The optional fields whose Go zero value means "not set" carry
// omitempty, the convention IndexRequest documents for request bodies:
// a Go caller that leaves FrameSize or CompressionLevel at zero sends no
// key and gets the default, rather than an explicit 0 that the level
// range refuses. BuildIndex is a pointer because its default is true:
// huma fills the default into every field whose value is the zero value,
// so a plain bool could not carry an explicit false. A nil pointer is
// "not given", and huma sets it to true.
//
// FrameSize is a human-readable size string (e.g. "4M", "16MB"), parsed
// by the handler. CompressionLevel is the zstd level, 1..22; huma
// refuses a value outside that range with 422 before the handler runs.
type CompressRequest struct {
	InputPath        string  `json:"input_path" doc:"Path to the input file. Must be inside a configured --search-root."`
	OutputPath       *string `json:"output_path,omitempty" required:"false" doc:"Path for the output .zst file (default: input_path + \".zst\"). Must be inside a configured --search-root."`
	FrameSize        string  `json:"frame_size,omitempty" required:"false" default:"4M" doc:"Target frame size: bytes, or a number with B, K, KB, M, MB, G or GB."`
	CompressionLevel int     `json:"compression_level,omitempty" required:"false" default:"3" minimum:"1" maximum:"22" doc:"zstd compression level."`
	BuildIndex       *bool   `json:"build_index,omitempty" required:"false" default:"true" doc:"Build the line index of the compressed file after compressing it."`
	Force            bool    `json:"force,omitempty" required:"false" default:"false" doc:"Overwrite the output file if it exists."`
}

// CompressTaskResult is the result of a completed POST /v1/compress
// task.
//
// TotalLines is the line count of the index built after compressing,
// null when none was built. IndexError says why building that index
// failed, and is null otherwise: the compressed file is correct and
// usable either way, and POST /v1/index can build the index later.
type CompressTaskResult struct {
	Success          bool    `json:"success"`
	InputPath        string  `json:"input_path"`
	OutputPath       string  `json:"output_path"`
	CompressedSize   int64   `json:"compressed_size"`
	DecompressedSize int64   `json:"decompressed_size"`
	CompressionRatio float64 `json:"compression_ratio" doc:"decompressed_size / compressed_size, rounded down to two decimals."`
	FrameCount       int     `json:"frame_count"`
	TotalLines       *int64  `json:"total_lines"`
	IndexBuilt       bool    `json:"index_built"`
	IndexError       *string `json:"index_error"`
	TimeSeconds      float64 `json:"time_seconds"`
	CLICommand       string  `json:"cli_command" doc:"The rx command that does what this task did."`
}
