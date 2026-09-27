package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
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
		Description: "Creates a background task that encodes the file to .zst. Poll /v1/tasks/{id} for progress. The input path and the effective output path are both validated against --search-root.",
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
	output := validated + ".zst"
	if req.OutputPath != nil && *req.OutputPath != "" {
		output = *req.OutputPath
	}
	output, err = paths.ValidatePathWithinRoots(output)
	if err != nil {
		return nil, ClassifyPathError(err)
	}
	if !req.Force {
		if _, statErr := os.Stat(output); statErr == nil {
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

	task, isNew := s.cfg.TaskManager.Create(validated, "compress")
	if !isNew {
		return nil, ErrTaskConflict(fmt.Sprintf(
			"Compression already in progress for %s (task: %s)",
			req.InputPath, task.TaskID,
		), task.TaskID)
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
	// CLICommand is the rx command that does what the request asked.
	CLICommand string
}

// compressCLICommand renders the rx command for a compress request.
// input and output are the validated paths. The output is named only
// when the request named one: without --output, rx compress writes to
// the same input + ".zst" the request defaulted to.
func compressCLICommand(input, output string, req rxtypes.CompressRequest) string {
	params := map[string]any{
		"input_path":        input,
		"frame_size":        req.FrameSize,
		"compression_level": req.CompressionLevel,
	}
	if req.OutputPath != nil && *req.OutputPath != "" {
		params["output_path"] = output
	}
	return BuildCLICommand("compress", params)
}

// runCompressTask does the actual encoding work in the background.
// Updates the task as it progresses: running → (completed|failed).
func runCompressTask(mgr *tasks.Manager, taskID string, job compressJob) {
	mgr.MarkRunning(taskID)
	start := time.Now()

	src, err := os.Open(job.InputPath)
	if err != nil {
		mgr.Fail(taskID, fmt.Sprintf("open input: %v", err))
		return
	}
	defer func() { _ = src.Close() }()

	info, err := src.Stat()
	if err != nil {
		mgr.Fail(taskID, fmt.Sprintf("stat input: %v", err))
		return
	}

	// Remove any pre-existing output so we don't mix data with a stale
	// file. The earlier Force check already gated this.
	if _, statErr := os.Stat(job.OutputPath); statErr == nil {
		_ = os.Remove(job.OutputPath)
	}

	dst, err := os.Create(job.OutputPath)
	if err != nil {
		mgr.Fail(taskID, fmt.Sprintf("create output: %v", err))
		return
	}
	defer func() { _ = dst.Close() }()

	enc := seekable.NewEncoder(seekable.EncoderConfig{
		FrameSize: int(job.FrameSizeBytes),
		Level:     job.CompressionLevel,
		// Single-worker is safer for unconfigured backgrounds — a
		// follow-up can surface parallelism through an env knob.
		Workers: 1,
	})
	tbl, err := enc.Encode(context.Background(), src, info.Size(), dst)
	if err != nil {
		mgr.Fail(taskID, fmt.Sprintf("encode: %v", err))
		return
	}
	if syncErr := dst.Sync(); syncErr != nil {
		mgr.Fail(taskID, fmt.Sprintf("fsync output: %v", syncErr))
		return
	}

	// Stat the produced file to read the compressed size.
	outInfo, err := os.Stat(job.OutputPath)
	if err != nil {
		mgr.Fail(taskID, fmt.Sprintf("stat output: %v", err))
		return
	}
	compressedSize := outInfo.Size()
	decompressedSize := info.Size()
	// compression_ratio is decompressed/compressed, so it reads >= 1 for
	// data that actually shrank. The CLI and rx-python use the same
	// convention.
	var ratio float64
	if compressedSize > 0 {
		ratio = float64(decompressedSize) / float64(compressedSize)
		ratio = float64(int(ratio*100)) / 100
	}
	frameCount := len(tbl.Frames)
	elapsed := time.Since(start).Seconds()

	result := rxtypes.CompressTaskResult{
		Success:          true,
		InputPath:        job.InputPath,
		OutputPath:       job.OutputPath,
		CompressedSize:   compressedSize,
		DecompressedSize: decompressedSize,
		CompressionRatio: ratio,
		FrameCount:       frameCount,
		TimeSeconds:      elapsed,
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
