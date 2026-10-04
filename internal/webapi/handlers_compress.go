package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/compressfile"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

type postCompressInput struct {
	Body rxtypes.CompressRequest
}

type postCompressOutput struct {
	Body rxtypes.TaskResponse
}

// registerCompressHandlers mounts POST /v1/compress.
//
// Matches rx-python/src/rx/web.py:1716-1787. Creates a background task
// that encodes a file to seekable-zstd using the native Go encoder, so
// no external tool is needed.
func registerCompressHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "compress",
		Method:      http.MethodPost,
		Path:        "/v1/compress",
		Summary:     "Compress file to seekable zstd format (background task)",
		Description: "Creates a background task that encodes the file to .zst. The output holds the input's text: a gzip, bzip2, xz or zstd input is decompressed first. A compound archive (.tar.gz and its kin), a seekable zstd input without force, and an output path that is the input file are refused with 400. Poll /v1/tasks/{id} for progress. The input path and the effective output path are both validated against --search-root.",
		Tags:        []string{"Operations"},
		Responses: errorResponses(api, http.StatusBadRequest, http.StatusForbidden,
			http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(_ context.Context, in *postCompressInput) (*postCompressOutput, error) {
		return createCompressTask(s, in.Body)
	})
}

// createCompressTask is the heavy half of POST /v1/compress, split out
// so tests can drive it without a full HTTP round-trip.
func createCompressTask(s *Server, req rxtypes.CompressRequest) (*postCompressOutput, error) {
	// Validate input path within search roots.
	validated, err := paths.ValidatePathWithinRoots(req.InputPath)
	if err != nil {
		return nil, ClassifyPathError(err)
	}
	if _, statErr := os.Stat(validated); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, ErrNotFound(fmt.Sprintf("File not found: %s", req.InputPath))
		}
		return nil, ErrForbidden(statErr.Error())
	}

	// Determine the output path, then put it through the same sandbox as
	// the input. The background task removes and re-creates this file, so
	// an unvalidated output_path would be an arbitrary-file-write
	// primitive on a server that has no authentication.
	// SECURITY: validate before any stat, and use the returned path for
	// the task so the checked string is the written string.
	// Without output_path the output goes beside the input under the
	// name rx compress gives it (app.log.gz → app.log.zst).
	output := filepath.Join(filepath.Dir(validated), compressfile.DefaultOutputName(validated))
	if req.OutputPath != nil && *req.OutputPath != "" {
		output = *req.OutputPath
	}
	output, err = paths.ValidatePathWithinRoots(output)
	if err != nil {
		return nil, ClassifyPathError(err)
	}
	// Refuse a compound archive, a seekable input the request did not
	// ask to re-encode, and an output that is the input, before the
	// "already exists" rule and before a task is created: the caller
	// gets the reason as a 400 rather than as a failed task.
	//
	// Check opens the input through its pin; failing to do that (the
	// path changed after the validation above) is a path error, not a
	// refusal of the request's content.
	if refusal := compressfile.Check(validated, output, req.Force); refusal != nil {
		if !isCompressRefusal(refusal) {
			return nil, ClassifyPathError(refusal)
		}
		return nil, ErrBadRequest(fmt.Sprintf("%s: %s", req.InputPath, compressRefusal(refusal)))
	}
	// Lstat: a symbolic link holds the name even when it leads nowhere.
	// The task applies the same rule again when it puts the output in
	// place, so a file created in between is not overwritten either.
	if !req.Force {
		if _, statErr := os.Lstat(output); statErr == nil {
			return nil, ErrBadRequest(fmt.Sprintf("Output file already exists: %s", output))
		}
	}

	// The fields the caller left out already hold the defaults of
	// `rx compress` here (huma fills them from the struct tags of
	// rxtypes.CompressRequest), and huma has refused a compression level
	// outside 1..22 with 422, so the request is used as it stands.
	level := req.CompressionLevel
	frameSize := req.FrameSize
	frameSizeBytes, err := parseFrameSizeHTTP(frameSize)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}

	// The task holds its output as well as its input. app.log and
	// app.log.gz both default to app.log.zst, so a lock on the input
	// alone let two tasks write one output at once. While this task
	// runs, a compression of anything into the same output, and an
	// index build of it, is refused with 409; the answer names the path
	// that is held, so the caller sees which one collided.
	task, heldPath, isNew := s.cfg.TaskManager.CreateHolding("compress", validated, output)
	if !isNew {
		conflictPath := req.InputPath
		if heldPath == output {
			conflictPath = output
		}
		return nil, runningTaskConflict(conflictPath, task)
	}

	// Snapshot task state before spawning the goroutine to avoid
	// concurrent access once the worker calls MarkRunning.
	taskID := task.TaskID
	taskStatus := string(task.Status)
	taskStarted := task.StartedAt

	// Wrap the detached task in the panic-recovery helper so a runtime
	// panic inside seekable.Encode (malformed input, corrupt buffer,
	// etc.) marks the task Failed without bringing down the HTTP
	// server..
	mgr := s.cfg.TaskManager
	logger := s.cfg.Logger
	job := compressJob{
		InputPath:        validated,
		OutputPath:       output,
		FrameSizeBytes:   frameSizeBytes,
		CompressionLevel: level,
		BuildIndex:       req.BuildIndex == nil || *req.BuildIndex,
		ReencodeSeekable: req.Force,
		Overwrite:        req.Force,
		CLICommand:       compressCLICommand(validated, output, req),
	}
	go runDetached(mgr, taskID, "compress", logger, func() {
		runCompressTask(mgr, taskID, job)
	})

	started := formatTaskTime(taskStarted)
	return &postCompressOutput{Body: rxtypes.TaskResponse{
		TaskID:    taskID,
		Status:    taskStatus,
		Message:   fmt.Sprintf("Compression task started for %s", req.InputPath),
		Path:      validated,
		StartedAt: &started,
	}}, nil
}

// compressJob bundles the resolved parameters for the background task.
// Kept private; only runCompressTask uses it.
type compressJob struct {
	InputPath        string
	OutputPath       string
	FrameSizeBytes   int64
	CompressionLevel int
	BuildIndex       bool
	// ReencodeSeekable lets a seekable zstd input through; the request's
	// force sets it.
	ReencodeSeekable bool
	// Overwrite replaces a file that holds the output name; the
	// request's force sets it.
	Overwrite bool
	// CLICommand is the rx command that does what the request asked.
	CLICommand string
}

// compressCLICommand renders the rx command for a compress request.
// input and output are the validated paths. The output is named only
// when the request named one: without --output, rx compress writes to
// the same default name the request took (compressfile.DefaultOutputName).
func compressCLICommand(input, output string, req rxtypes.CompressRequest) string {
	params := map[string]any{
		"input_path":        input,
		"frame_size":        req.FrameSize,
		"compression_level": req.CompressionLevel,
		"build_index":       req.BuildIndex,
		"force":             req.Force,
	}
	if req.OutputPath != nil && *req.OutputPath != "" {
		params["output_path"] = output
	}
	return BuildCLICommand("compress", params)
}

// compressRefusalHints names, for a compressfile refusal, the request
// field that gets past it. The first entry whose error matches
// (errors.Is) wins.
var compressRefusalHints = []struct {
	err  error
	hint string
}{
	{compressfile.ErrAlreadySeekable, ` (set "force": true to re-encode it)`},
	{compressfile.ErrOutputIsInput, ` (set "output_path" to another file)`},
	{compressfile.ErrOutputExists, ` (set "force": true to overwrite)`},
}

// compressCheckRefusals are the errors compressfile.Check returns about
// a request's content, each answered with 400. Any other error from it
// is a failure to reach the input.
var compressCheckRefusals = []error{
	compressfile.ErrCompoundArchive,
	compressfile.ErrAlreadySeekable,
	compressfile.ErrOutputIsInput,
}

// isCompressRefusal reports whether err is one of compressCheckRefusals.
func isCompressRefusal(err error) bool {
	for _, refusal := range compressCheckRefusals {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// compressRefusal words a compressfile error for the HTTP API, with the
// hint compressRefusalHints holds for it.
func compressRefusal(err error) string {
	for _, h := range compressRefusalHints {
		if errors.Is(err, h.err) {
			return err.Error() + h.hint
		}
	}
	return err.Error()
}

// runCompressTask does the actual encoding work in the background.
// Updates the task as it progresses: running → (completed|failed).
//
// The encoding goes through compressfile.Compress, the function
// `rx compress` calls, so a compressed input is written as its text
// here exactly as on the command line.
func runCompressTask(mgr *tasks.Manager, taskID string, job compressJob) {
	mgr.MarkRunning(taskID)
	start := time.Now()

	// Compress writes a temporary file and puts it under the output name
	// only when it is whole, so an existing output is replaced at the
	// end (force) or left alone (the task fails), never removed first.
	written, err := compressfile.Compress(context.Background(), compressfile.Options{
		InputPath:  job.InputPath,
		OutputPath: job.OutputPath,
		FrameSize:  int(job.FrameSizeBytes),
		Level:      job.CompressionLevel,
		// Single-worker is safer for unconfigured backgrounds — a
		// follow-up can surface parallelism through an env knob.
		Workers:          1,
		ReencodeSeekable: job.ReencodeSeekable,
		Overwrite:        job.Overwrite,
	})
	if err != nil {
		mgr.Fail(taskID, compressRefusal(err))
		return
	}

	result := rxtypes.CompressTaskResult{
		Success:          true,
		InputPath:        job.InputPath,
		OutputPath:       job.OutputPath,
		CompressedSize:   written.CompressedSize,
		DecompressedSize: written.DecompressedSize,
		// compression_ratio is decompressed/compressed, so it reads >= 1
		// for data that actually shrank. The CLI and rx-python use the
		// same convention.
		CompressionRatio: written.Ratio(),
		FrameCount:       written.FrameCount,
		TimeSeconds:      time.Since(start).Seconds(),
		CLICommand:       job.CLICommand,
	}

	// Index the file just written, so a later `samples --lines=N` on it
	// decompresses one frame instead of the whole stream. A failure is
	// reported rather than fatal: the compressed file is correct and
	// usable, and POST /v1/index can build the index later.
	if job.BuildIndex {
		lineCount, idxErr := indexCompressedOutput(job.OutputPath)
		if idxErr != nil {
			message := idxErr.Error()
			result.IndexError = &message
		} else {
			result.IndexBuilt = true
			result.TotalLines = lineCount
		}
	}

	mgr.Complete(taskID, result)
}

// indexCompressedOutput builds and saves the line index of a file the
// compress task just wrote, and returns its line count (nil when the
// builder did not count lines).
func indexCompressedOutput(path string) (*int64, error) {
	idx, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := index.Save(idx); err != nil {
		return nil, err
	}
	return idx.LineCount, nil
}

// parseFrameSizeHTTP is the HTTP-facing wrapper around the CLI's
// frame-size parser. Duplicated here (instead of importing from
// internal/clicommand) to avoid a cyclic dep — the CLI imports webapi
// to reuse the cli_command builder, so webapi must not import clicommand.
//
// Accepts: bare numbers (bytes), B, K/KB, M/MB, G/GB — case-insensitive.
func parseFrameSizeHTTP(s string) (int64, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(s))
	if trimmed == "" {
		return 0, errors.New("frame_size is empty")
	}

	type suffix struct {
		tag  string
		mult int64
	}
	// Longest suffixes first so "MB" wins over "M".
	suffixes := []suffix{
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
		{"G", 1024 * 1024 * 1024},
		{"M", 1024 * 1024},
		{"K", 1024},
		{"B", 1},
	}
	for _, sf := range suffixes {
		if strings.HasSuffix(trimmed, sf.tag) {
			num := strings.TrimSpace(strings.TrimSuffix(trimmed, sf.tag))
			if num == "" {
				return 0, fmt.Errorf("frame_size missing number before %s", sf.tag)
			}
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("frame_size %q: %w", s, err)
			}
			return int64(v * float64(sf.mult)), nil
		}
	}
	// No suffix: interpret as raw bytes.
	v, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("frame_size %q: %w", s, err)
	}
	return v, nil
}
