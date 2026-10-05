package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// GET /v1/index — read cached index
// ============================================================================

type getIndexInput struct {
	Path string `query:"path" required:"true" example:"/var/log/app.log" doc:"File path to get index for"`
}

type getIndexOutput struct {
	Body rxtypes.IndexResponse
}

// ============================================================================
// POST /v1/index — kick off background indexing
// ============================================================================

type postIndexInput struct {
	Body rxtypes.IndexRequest
}

type postIndexOutput struct {
	Body rxtypes.TaskResponse
}

// registerIndexHandlers mounts GET and POST /v1/index.
//
// GET returns a cached UnifiedFileIndex projected through
// indexResponseFrom (matches rx-python/src/rx/web.py:755-823).
//
// POST validates, creates a task, launches the background goroutine
// that actually does the indexing (matches web.py:1790-1876).
func registerIndexHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "index-get",
		Method:      http.MethodGet,
		Path:        "/v1/index",
		Summary:     "Get cached index data for a file",
		Description: "Returns the cached UnifiedFileIndex, or 404 if no index exists yet.",
		Tags:        []string{"Indexing"},
		Responses: errorResponses(api, http.StatusForbidden, http.StatusNotFound,
			http.StatusUnprocessableEntity),
	}, func(_ context.Context, in *getIndexInput) (*getIndexOutput, error) {
		validated, err := paths.ValidatePathWithinRoots(in.Path)
		if err != nil {
			var perr *paths.ErrPathOutsideRoots
			if errors.As(err, &perr) {
				return nil, NewSandboxError(perr)
			}
			return nil, ErrForbidden(err.Error())
		}
		idx, err := index.LoadForSource(validated)
		if err != nil || idx == nil {
			return nil, ErrNotFound(fmt.Sprintf(
				"No index found for %s. Use POST /v1/index to create an indexing task.",
				validated,
			))
		}
		data := indexResponseFrom(idx)
		// cli_command equivalent (only on API responses; CLI fills its own).
		data.CLICommand = BuildCLICommand("index_get", map[string]any{"path": validated})
		return &getIndexOutput{Body: data}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "index-post",
		Method:      http.MethodPost,
		Path:        "/v1/index",
		Summary:     "Build the line index of a file, or of every file of a directory (background task)",
		Description: "Creates a background task that indexes the file. Poll /v1/tasks/{id} for progress. For a " +
			"directory, one task indexes every file the file tree lists under it, recursively, one at a time, " +
			"as rx index DIR --recursive does: a file below the threshold, a file that is not text and a file " +
			"whose index is current (unless force) is skipped, a file whose build fails is recorded and the " +
			"task goes on. Its status has files_done and files_total, its result is a FolderTaskResult, and " +
			"DELETE /v1/tasks/{task_id} cancels it: the build of the current file stops at its next read and " +
			"stores nothing. Until its worker has stopped the task is cancelling and a POST for the directory " +
			"answers 409.",
		Tags: []string{"Operations"},
		Responses: errorResponses(api, http.StatusBadRequest, http.StatusForbidden,
			http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(_ context.Context, in *postIndexInput) (*postIndexOutput, error) {
		return createIndexTask(s, in.Body)
	})
}

// createIndexTask is the heavy-lifting half of POST /v1/index, split out
// so integration tests can drive it without a full HTTP round-trip. The
// task runs detached from the request.
func createIndexTask(s *Server, req rxtypes.IndexRequest) (*postIndexOutput, error) {
	validated, err := paths.ValidatePathWithinRoots(req.Path)
	if err != nil {
		var perr *paths.ErrPathOutsideRoots
		if errors.As(err, &perr) {
			return nil, NewSandboxError(perr)
		}
		return nil, ErrForbidden(err.Error())
	}
	info, err := os.Stat(validated)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound(fmt.Sprintf("File not found: %s", req.Path))
		}
		return nil, ErrForbidden(err.Error())
	}
	// A directory is indexed file by file in one task, as
	// `rx index --recursive DIR` indexes it; the threshold applies to
	// each file.
	if info.IsDir() {
		dir, pinErr := paths.Pin(validated)
		if pinErr != nil {
			return nil, ErrFileAccess(req.Path, pinErr)
		}
		return s.startIndexFolder(req, dir)
	}

	// File-size threshold check. Request-provided threshold (MB) wins
	// over env default; converting MB→bytes here keeps the task payload
	// in native units.
	thresholdBytes := thresholdBytesOf(req.Threshold)
	if info.Size() < thresholdBytes {
		return nil, ErrBadRequest(fmt.Sprintf(
			"File size %d bytes is below threshold %d bytes",
			info.Size(), thresholdBytes,
		))
	}
	// A file whose text is not text (filekind) has no lines to index,
	// and rx-python refuses a binary one here too. The file is read
	// through its pin; one that cannot be opened is refused like one
	// that cannot be stated.
	src, err := paths.Pin(validated)
	if err != nil {
		return nil, ErrFileAccess(req.Path, err)
	}
	if src.Info().IsDir() {
		return nil, ErrBadRequest(fmt.Sprintf("Path is a directory, not a file: %s", req.Path))
	}
	kind, err := filekind.OfPinnedForReading(src)
	if err != nil {
		// A file the server may not read is a 403 on every route.
		return nil, ErrFileAccess(req.Path, err)
	}
	if !kind.IsText() {
		return nil, ErrBadRequest(fmt.Sprintf("%s: %s", kind.NotText, req.Path))
	}

	task, isNew := s.cfg.TaskManager.Create(validated, indexOperation)
	if !isNew {
		return nil, runningTaskConflict(req.Path, task)
	}
	cli := indexCLICommand(validated, req)
	s.cfg.TaskManager.SetCLICommand(task.TaskID, cli)

	// Snapshot task state BEFORE launching the goroutine. Once the
	// goroutine calls MarkRunning the Task's Status field is mutated
	// concurrently; reading it here afterwards is a data race.
	taskID := task.TaskID
	taskStatus := string(task.Status)
	taskStarted := task.StartedAt

	// Launch background goroutine. Detached from the request lifetime —
	// a client disconnect cannot cancel the job (matches Python's
	// asyncio.create_task semantics).
	//
	// runDetached wraps the work in a panic-recovery boundary so a
	// malformed input that triggers a runtime panic inside index.Build
	// (or any downstream helper) marks the task Failed instead of
	// crashing the whole server..
	mgr := s.cfg.TaskManager
	logger := s.cfg.Logger
	go runDetached(mgr, taskID, "index", logger, func() {
		runIndexTask(mgr, taskID, validated, req)
	})

	started := formatTaskTime(taskStarted)
	return &postIndexOutput{Body: rxtypes.TaskResponse{
		TaskID:     taskID,
		Status:     taskStatus,
		Message:    fmt.Sprintf("Indexing task started for %s", req.Path),
		Path:       validated,
		StartedAt:  &started,
		CLICommand: cli,
	}}, nil
}

// indexCLICommand is the rx command that does what the index task of
// one file does: `rx index FILE`, with --force and --threshold=N as the
// request gave them, and --hidden on a server that serves hidden
// entries.
func indexCLICommand(absPath string, req rxtypes.IndexRequest) string {
	return BuildCLICommand("index_post", map[string]any{
		"path":      absPath,
		"hidden":    paths.IncludeHidden(),
		"force":     req.Force,
		"threshold": req.Threshold,
	})
}

// runIndexTask is the background worker for POST /v1/index.
//
// Pipeline:
//  1. Mark task Running.
//  2. If --force is false and the file has a valid cached index, reuse
//     it.
//  3. Otherwise run index.Build() to produce a new line-offset index
//     and Save() it.
//  4. Mark task Completed (or Failed on error).
func runIndexTask(mgr *tasks.Manager, taskID, absPath string, req rxtypes.IndexRequest) {
	mgr.MarkRunning(taskID)
	start := time.Now()

	// The build counts its reads into progress, and a status request
	// reads it from another goroutine while the build runs.
	progress := &index.Progress{}
	buildOpts := index.BuildOptions{Progress: progress}

	// Reuse a valid cached index unless the caller asked for force.
	if !req.Force {
		if existing, err := index.LoadForSource(absPath); err == nil && existing != nil {
			mgr.Complete(taskID, indexTaskResultFrom(existing, index.GetCachePath(absPath), absPath, req))
			return
		}
	}

	// An index the cache cannot store is not worth building: the build
	// reads the whole file and the save would then fail. The task
	// fails now, naming the cause.
	if err := index.CheckStorable(); err != nil {
		mgr.Fail(taskID, fmt.Sprintf("cannot store the line index: %v", err))
		return
	}

	// Fresh build. index.Build() opens/stats the file itself, so an
	// open failure surfaces here.
	mgr.ReportProgress(taskID, progress.Fraction)
	idx, err := index.Build(absPath, buildOpts)
	if err != nil {
		mgr.Fail(taskID, fmt.Sprintf("build index: %v", err))
		return
	}

	// Stamp the build-time onto the response (Build already fills
	// BuildTimeSeconds, but we include the full round-trip time here
	// so the caller sees the task total, not just the builder time).
	idx.BuildTimeSeconds = time.Since(start).Seconds()

	cachePath, err := index.Save(idx)
	if err != nil {
		mgr.Fail(taskID, fmt.Sprintf("save index: %v", err))
		return
	}
	mgr.Complete(taskID, indexTaskResultFrom(idx, cachePath, absPath, req))
}

// indexTaskResultFrom is the result a POST /v1/index task completes
// with: the projection of the index it built or reused, where that index
// is stored, and the rx command that does the same for absPath, the
// validated path the request named.
func indexTaskResultFrom(idx *rxtypes.UnifiedFileIndex, indexPath, absPath string, req rxtypes.IndexRequest) rxtypes.IndexTaskResult {
	projection := indexResponseFrom(idx)
	projection.CLICommand = indexCLICommand(absPath, req)
	return rxtypes.IndexTaskResult{
		IndexResponse: projection,
		Success:       true,
		IndexPath:     indexPath,
	}
}

// indexResponseFrom projects a UnifiedFileIndex to the client-facing
// IndexResponse, matching rx-python/src/rx/web.py's
// _unified_index_to_dict. CLICommand is left empty: GET /v1/index and
// the index task each fill in their own.
//
// This shape is the contract for both GET /v1/index and the result of a
// POST /v1/index task (IndexTaskResult embeds it).
func indexResponseFrom(idx *rxtypes.UnifiedFileIndex) rxtypes.IndexResponse {
	lineIndex := idx.LineIndex
	if lineIndex == nil {
		lineIndex = []rxtypes.LineIndexEntry{}
	}
	out := rxtypes.IndexResponse{
		Path:                  idx.SourcePath,
		FileType:              idx.FileType,
		SizeBytes:             idx.SourceSizeBytes,
		CreatedAt:             idx.CreatedAt,
		BuildTimeSeconds:      idx.BuildTimeSeconds,
		LineIndex:             lineIndex,
		IndexEntries:          len(lineIndex),
		LineCount:             idx.LineCount,
		EmptyLineCount:        idx.EmptyLineCount,
		LineEnding:            idx.LineEnding,
		CompressionFormat:     idx.CompressionFormat,
		DecompressedSizeBytes: idx.DecompressedSizeBytes,
		CompressionRatio:      idx.CompressionRatio,
		// The stored journal section as it is, or null for a file that
		// is not a journal.
		Journal: idx.Journal,
	}
	out.TimeSummary = timeSummaryFrom(idx.TimeIndex)
	// The line-length group exists only when the statistics were
	// computed (not for a seekable-zstd index), and the longest line
	// only when its position is known.
	if idx.LineLengthMax != nil {
		out.LineLength = &rxtypes.LineLengthStats{
			Max:    *idx.LineLengthMax,
			Avg:    idx.LineLengthAvg,
			Median: idx.LineLengthMedian,
			P95:    idx.LineLengthP95,
			P99:    idx.LineLengthP99,
			Stddev: idx.LineLengthStddev,
		}
		if idx.LineLengthMaxLineNumber != nil && idx.LineLengthMaxByteOffset != nil {
			out.LongestLine = &rxtypes.LongestLine{
				LineNumber: *idx.LineLengthMaxLineNumber,
				ByteOffset: *idx.LineLengthMaxByteOffset,
			}
		}
	}
	return out
}

// timeSummaryFrom projects an index's time section for a client: every
// member but max_before, the per-checkpoint array only a search by time
// reads. A nil section (no timestamp format) is a null summary.
func timeSummaryFrom(ti *rxtypes.TimeIndex) *rxtypes.TimeSummary {
	if ti == nil {
		return nil
	}
	out := &rxtypes.TimeSummary{
		Format:           ti.Format,
		HasZone:          ti.HasZone,
		TimestampedLines: ti.TimestampedLines,
		BackwardSteps:    ti.BackwardSteps,
		MaxBackwardMs:    ti.MaxBackwardMs,
	}
	if ti.First != nil {
		out.FirstMs = &ti.First.Ms
	}
	if ti.Last != nil {
		out.LastMs = &ti.Last.Ms
	}
	return out
}
