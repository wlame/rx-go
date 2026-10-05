package rxtypes

// TaskResponse is returned by POST /v1/index, POST /v1/compress and
// POST /v1/findings when a background task is accepted. The client polls GET /v1/tasks/{id}
// with the returned TaskID until completion.
//
// CLICommand is the rx command that does what the task does, for an
// index or findings task (TaskStatusResponse.CLICommand); "" for a
// compress task.
type TaskResponse struct {
	TaskID     string  `json:"task_id"`
	Status     string  `json:"status"`
	Message    string  `json:"message"`
	Path       string  `json:"path"`
	StartedAt  *string `json:"started_at"`
	CLICommand string  `json:"cli_command" doc:"The rx command that does what the task does: rx index for a task of POST /v1/index, rx scan for a task of POST /v1/findings, with --recursive for a directory (rx index DIR --recursive, rx scan DIR --recursive --signatures=SEL), --hidden when the server serves hidden entries, --force when the request forced, and --mask-secrets when the server masks secrets. Empty for a compress task. A request that joined a running task gets that task's command."`
}

// TaskStatusResponse is returned by GET /v1/tasks/{task_id} and by
// DELETE /v1/tasks/{task_id}.
//
// Operation is "compress", "index", "chain_index" or "findings". Status
// transitions: "queued" → "running" → ("completed" | "failed"), and a
// findings task may end "cancelled" from either of the first two. A
// task over a directory goes from either to "cancelling" on a cancel,
// and to "cancelled" once its worker has stopped. Result is nil until Status ==
// "completed"; Error is nil unless Status == "failed". Progress,
// BytesRead, BytesTotal, FilesDone and FilesTotal are nil for a task
// that does not report them; FilesDone and FilesTotal are set for a
// task over a directory only.
type TaskStatusResponse struct {
	TaskID      string     `json:"task_id"`
	Status      string     `json:"status" doc:"queued, running, completed, failed, cancelled (a findings task, or a task over a directory, stopped by DELETE /v1/tasks/{task_id}), or cancelling (a task over a directory that DELETE cancelled, whose worker has not stopped yet: it still holds the directory, a POST for it answers 409, and it becomes cancelled once the worker stops; not an end)."`
	Path        string     `json:"path"`
	Operation   string     `json:"operation" doc:"index, compress, chain_index or findings."`
	StartedAt   *string    `json:"started_at"`
	CompletedAt *string    `json:"completed_at"`
	Error       *string    `json:"error"`
	Progress    *float64   `json:"progress" doc:"Share of the task's input read so far, from 0 to 1. For a task over a directory: the files ended plus the share of the current file read, over files_total. Null for a task that does not report it: a compress task, an index task that reused a stored index, or one that has not started reading."`
	BytesRead   *int64     `json:"bytes_read" doc:"How far into the file a findings task has read so far: the highest byte of the file as stored on disk it has read, not the sum of its reads; for a findings task over a directory, of the file it reads now (0 between files). Null for a task of another operation (an index task over a directory too), and for a findings task over one file that has not started reading."`
	BytesTotal  *int64     `json:"bytes_total" doc:"How far into the file a findings task reads to reach the end of its text: the end of the last object of a systemd journal (a live journal is preallocated, and the zeros after its objects are never read), and the size of the file as stored on disk for every other format; for a findings task over a directory, of the file it reads now (0 between files). Null when bytes_read is."`
	FilesDone   *int64     `json:"files_done" doc:"For a task over a directory: the entries of files_total the task has ended (indexed or scanned, skipped, or failed). Null for a task over one file, and while the task lists the directory."`
	FilesTotal  *int64     `json:"files_total" doc:"For a task over a directory: the entries its walk listed (every file under the directory by the file tree's rules, and the entries the walk passed over). Null when files_done is."`
	CLICommand  string     `json:"cli_command" doc:"The rx command that does what the task does: rx index for an index task started by POST /v1/index, rx scan for a findings task, with --recursive for a directory, --hidden when the server serves hidden entries (rx serve --hidden, RX_HIDDEN), --force when the request forced, and --mask-secrets when the server masks secrets. Empty for other tasks: compress, chain_index, and the index builds GET /v1/samples and the log chains start."`
	Result      TaskResult `json:"result"`
}

// TaskResult is the result of a background task: an IndexTaskResult
// for an "index" task, a CompressTaskResult for a "compress" task, a
// ChainIndexTaskResult for a "chain_index" task, a FindingsResponse for
// a "findings" task, a FolderTaskResult for an "index" or "findings"
// task over a directory, and nil (JSON null) until the task completes.
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
	OutputPath       *string `json:"output_path,omitempty" required:"false" doc:"Path for the output .zst file (default: beside the input, named like it with a compression suffix .gz, .gzip, .bz2, .bzip2, .xz, .zst or .zstd replaced by .zst, or with .zst appended: app.log.gz gives app.log.zst). Must be inside a configured --search-root."`
	FrameSize        string  `json:"frame_size,omitempty" required:"false" default:"4M" doc:"Target frame size: bytes, or a number with B, K, KB, M, MB, G or GB."`
	CompressionLevel int     `json:"compression_level,omitempty" required:"false" default:"3" minimum:"1" maximum:"22" doc:"zstd compression level."`
	BuildIndex       *bool   `json:"build_index,omitempty" required:"false" default:"true" doc:"Build the line index of the compressed file after compressing it."`
	Force            bool    `json:"force,omitempty" required:"false" default:"false" doc:"Overwrite the output file if it exists, and re-encode an input that is already seekable zstd (refused otherwise)."`
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

// FolderTaskResult is the result of an index or findings task over a
// directory: POST /v1/index or POST /v1/findings given a directory's
// path. The task takes every entry its walk lists under the directory,
// one at a time, and each entry ends in exactly one of Done, Skipped and
// Failed: FilesTotal = Done + Skipped + Failed.
//
// SkippedFiles and FailedFiles list at most MaxFolderListedFiles entries
// each, in the walk's order; the counts are exact.
type FolderTaskResult struct {
	Path         string        `json:"path" doc:"The directory, as validated."`
	FilesTotal   int           `json:"files_total" doc:"The entries the walk listed under the directory: every file the file tree lists below it, recursively, and the entries the walk passed over (a refused link, a subdirectory it could not list)."`
	Done         int           `json:"done" doc:"The files indexed (an index task) or scanned (a findings task) by the task."`
	Skipped      int           `json:"skipped" doc:"The entries passed over: those whose index or findings were current (current counts them; force passes none over that way), and those skipped_files lists."`
	Current      int           `json:"current" doc:"How many of skipped were passed over because their index, or the findings of every selected signature, were current."`
	Failed       int           `json:"failed" doc:"The files whose index build or scan failed; the task went on with the next file."`
	Findings     *int64        `json:"findings" doc:"A findings task: the findings of every file scanned or current, summed over the selected signatures. Null for an index task."`
	SkippedFiles []SkippedFile `json:"skipped_files" nullable:"false" doc:"The entries skipped for another reason than being current, each with the reason (not a text file, below the index threshold, a refused link, cannot be read), in the walk's order; the first 1000 of skipped - current."`
	FailedFiles  []FailedFile  `json:"failed_files" nullable:"false" doc:"The files that failed, each with its error, in the walk's order; the first 1000 of failed."`
	CLICommand   string        `json:"cli_command" doc:"The rx command that does what the task did: rx index DIR --recursive, or rx scan DIR --recursive --signatures=SEL."`
}

// MaxFolderListedFiles is how many entries FolderTaskResult.SkippedFiles
// and FolderTaskResult.FailedFiles list at most each. A finished task's
// result stays in memory until it is swept, so its size is bounded
// whatever the number of files under the directory.
const MaxFolderListedFiles = 1000

// FailedFile is one file a task over a directory could not index or
// scan, and the error.
type FailedFile struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}
