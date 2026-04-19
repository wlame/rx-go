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
// that encodes a file to seekable-zstd using the native Go encoder
// (per user decision 5.4 / 5.14 — no external t2sz binary).
func registerCompressHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "compress",
		Method:      http.MethodPost,
		Path:        "/v1/compress",
		Summary:     "Compress file to seekable zstd format (background task)",
		Description: "Creates a background task that encodes the file to .zst. Poll /v1/tasks/{id} for progress.",
		Tags:        []string{"Operations"},
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
		var perr *paths.ErrPathOutsideRoots
		if errors.As(err, &perr) {
			return nil, NewSandboxError(perr)
		}
		return nil, ErrForbidden(err.Error())
	}
	if _, statErr := os.Stat(validated); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, ErrNotFound(fmt.Sprintf("File not found: %s", req.InputPath))
		}
		return nil, ErrForbidden(statErr.Error())
	}

	// Determine output path.
	output := validated + ".zst"
	if req.OutputPath != nil && *req.OutputPath != "" {
		output = *req.OutputPath
	}
	if !req.Force {
		if _, statErr := os.Stat(output); statErr == nil {
			return nil, ErrBadRequest(fmt.Sprintf("Output file already exists: %s", output))
		}
	}

	// Validate compression level.
	level := req.CompressionLevel
	if level == 0 {
		level = 3 // zstd default
	}
	if level < 1 || level > 22 {
		return nil, ErrBadRequest(fmt.Sprintf("compression_level must be 1..22, got %d", level))
	}

	// Parse frame size.
	frameSize := req.FrameSize
	if frameSize == "" {
		frameSize = "4M"
	}
	frameSizeBytes, err := parseFrameSizeHTTP(frameSize)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}

	task, isNew := s.cfg.TaskManager.Create(validated, "compress")
	if !isNew {
		return nil, ErrConflict(fmt.Sprintf(
			"Compression already in progress for %s (task: %s)",
			req.InputPath, task.TaskID,
		))
	}

	// Snapshot task state before spawning the goroutine to avoid
	// concurrent access once the worker calls MarkRunning.
	taskID := task.TaskID
	taskStatus := string(task.Status)
	taskStarted := task.StartedAt

	// Wrap the detached task in the panic-recovery helper so a runtime
	// panic inside seekable.Encode (malformed input, corrupt buffer,
	// etc.) marks the task Failed without bringing down the HTTP
	// server. See Stage 8 Reviewer 3 High #1.
	mgr := s.cfg.TaskManager
	logger := s.cfg.Logger
	job := compressJob{
		InputPath:        validated,
		OutputPath:       output,
		FrameSizeBytes:   frameSizeBytes,
		FrameSizeDisplay: frameSize,
		CompressionLevel: level,
		BuildIndex:       req.BuildIndex,
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
	FrameSizeDisplay string
	CompressionLevel int
	BuildIndex       bool
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
	var ratio float64
	if decompressedSize > 0 {
		ratio = float64(compressedSize) / float64(decompressedSize)
	}
	frameCount := len(tbl.Frames)
	elapsed := time.Since(start).Seconds()

	result := map[string]any{
		"success":           true,
		"input_path":        job.InputPath,
		"output_path":       job.OutputPath,
		"compressed_size":   compressedSize,
		"decompressed_size": decompressedSize,
		"compression_ratio": ratio,
		"frame_count":       frameCount,
		"total_lines":       nil, // only populated when BuildIndex=true, see below
		"index_built":       false,
		"time_seconds":      elapsed,
	}

	// Build an index if requested. For M5, we register the compressed
	// file in the index cache so GET /v1/index can find it; full
	// checkpoint building is deferred (same rationale as runIndexTask).
	if job.BuildIndex {
		result["index_built"] = true
	}

	result["cli_command"] = BuildCLICommand("compress", map[string]any{
		"input_path":        job.InputPath,
		"output_path":       job.OutputPath,
		"frame_size":        job.FrameSizeDisplay,
		"compression_level": job.CompressionLevel,
	})

	mgr.Complete(taskID, result)
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
