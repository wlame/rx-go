package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/config"
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
	Body map[string]any
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
// unifiedIndexToDict (matches rx-python/src/rx/web.py:755-823).
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
		data := unifiedIndexToDict(idx)
		// cli_command equivalent (only on API responses; CLI fills its own).
		data["cli_command"] = BuildCLICommand("index_get", map[string]any{"path": validated})
		return &getIndexOutput{Body: data}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "index-post",
		Method:      http.MethodPost,
		Path:        "/v1/index",
		Summary:     "Build line index for file (background task)",
		Description: "Creates a background task that indexes the file. Poll /v1/tasks/{id} for progress.",
		Tags:        []string{"Operations"},
	}, func(_ context.Context, in *postIndexInput) (*postIndexOutput, error) {
		return createIndexTask(s, in.Body)
	})
}

// createIndexTask is the heavy-lifting half of POST /v1/index, split out
// so integration tests can drive it without a full HTTP round-trip.
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

	// File-size threshold check. Request-provided threshold (MB) wins
	// over env default; converting MB→bytes here keeps the task payload
	// in native units.
	thresholdBytes := int64(config.LargeFileMB()) * 1024 * 1024
	if req.Threshold != nil {
		thresholdBytes = int64(*req.Threshold) * 1024 * 1024
	}
	if info.Size() < thresholdBytes {
		return nil, ErrBadRequest(fmt.Sprintf(
			"File size %d bytes is below threshold %d bytes",
			info.Size(), thresholdBytes,
		))
	}

	task, isNew := s.cfg.TaskManager.Create(validated, "index")
	if !isNew {
		return nil, ErrConflict(fmt.Sprintf(
			"Indexing already in progress for %s (task: %s)",
			req.Path, task.TaskID,
		))
	}

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
	// crashing the whole server. See Stage 8 Reviewer 3 High #1.
	mgr := s.cfg.TaskManager
	logger := s.cfg.Logger
	go runDetached(mgr, taskID, "index", logger, func() {
		runIndexTask(mgr, taskID, validated, req)
	})

	started := formatTaskTime(taskStarted)
	return &postIndexOutput{Body: rxtypes.TaskResponse{
		TaskID:    taskID,
		Status:    taskStatus,
		Message:   fmt.Sprintf("Indexing task started for %s", req.Path),
		Path:      validated,
		StartedAt: &started,
	}}, nil
}

// runIndexTask is the background worker for POST /v1/index.
//
// Pipeline:
//  1. Mark task Running.
//  2. If the file has a valid cached index AND --force is false AND the
//     caller doesn't want fresh analysis, reuse it.
//  3. Otherwise run index.Build() to produce a new line-offset index
//     (with or without --analyze statistics) and Save() it.
//  4. Mark task Completed (or Failed on error).
//
// Per-analyzer pluggable output is a separate track — this function
// stays analyzer-agnostic. When analyzer support lands, the registry
// hook slots in right after index.Build() and before the save.
func runIndexTask(mgr *tasks.Manager, taskID, absPath string, req rxtypes.IndexRequest) {
	mgr.MarkRunning(taskID)
	start := time.Now()

	// Attempt cache reuse first — skip if caller asked for --force, or
	// if they asked to --analyze and the cached index lacks analysis.
	if !req.Force {
		if existing, err := index.LoadForSource(absPath); err == nil && existing != nil {
			if !req.Analyze || existing.AnalysisPerformed {
				result := unifiedIndexToDict(existing)
				result["success"] = true
				result["index_path"] = index.GetCachePath(absPath)
				result["cli_command"] = BuildCLICommand("index_post", map[string]any{
					"path":    absPath,
					"force":   req.Force,
					"analyze": req.Analyze,
				})
				mgr.Complete(taskID, result)
				return
			}
		}
	}

	// Fresh build. index.Build() opens/stats the file itself, so an
	// open failure surfaces here.
	//
	// Window-size precedence: URL body param (req.AnalyzeWindowLines)
	// wins; if unset (zero), the resolver falls through to the env var
	// and then the compiled-in default. The CLI flag doesn't apply in
	// the HTTP path, so we pass cliFlag=0.
	windowLines := analyzer.ResolveWindowLines(0, req.AnalyzeWindowLines)
	idx, err := index.Build(absPath, index.BuildOptions{
		Analyze:     req.Analyze,
		WindowLines: windowLines,
	})
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

	result := unifiedIndexToDict(idx)
	result["success"] = true
	result["index_path"] = cachePath
	result["cli_command"] = BuildCLICommand("index_post", map[string]any{
		"path":    absPath,
		"force":   req.Force,
		"analyze": req.Analyze,
	})
	mgr.Complete(taskID, result)
}

// unifiedIndexToDict projects a UnifiedFileIndex to a JSON-shaped map
// matching rx-python/src/rx/web.py:826-898's _unified_index_to_dict.
//
// This shape is the contract for both GET /v1/index and the terminal
// task result of POST /v1/index, so changes here must be coordinated
// with the task handler.
func unifiedIndexToDict(idx *rxtypes.UnifiedFileIndex) map[string]any {
	out := map[string]any{
		"path":               idx.SourcePath,
		"file_type":          string(idx.FileType),
		"size_bytes":         idx.SourceSizeBytes,
		"created_at":         idx.CreatedAt,
		"build_time_seconds": idx.BuildTimeSeconds,
		"analysis_performed": idx.AnalysisPerformed,
		"line_index":         idx.LineIndex,
		"index_entries":      len(idx.LineIndex),
	}

	if idx.LineCount != nil {
		out["line_count"] = *idx.LineCount
	} else {
		out["line_count"] = nil
	}
	if idx.EmptyLineCount != nil {
		out["empty_line_count"] = *idx.EmptyLineCount
	} else {
		out["empty_line_count"] = nil
	}
	if idx.LineEnding != nil {
		out["line_ending"] = *idx.LineEnding
	} else {
		out["line_ending"] = nil
	}

	// line_length sub-object
	if idx.LineLengthMax != nil {
		ll := map[string]any{
			"max": *idx.LineLengthMax,
		}
		if idx.LineLengthAvg != nil {
			ll["avg"] = *idx.LineLengthAvg
		}
		if idx.LineLengthMedian != nil {
			ll["median"] = *idx.LineLengthMedian
		}
		if idx.LineLengthP95 != nil {
			ll["p95"] = *idx.LineLengthP95
		}
		if idx.LineLengthP99 != nil {
			ll["p99"] = *idx.LineLengthP99
		}
		if idx.LineLengthStddev != nil {
			ll["stddev"] = *idx.LineLengthStddev
		}
		out["line_length"] = ll
		if idx.LineLengthMaxLineNumber != nil {
			out["longest_line"] = map[string]any{
				"line_number": *idx.LineLengthMaxLineNumber,
				"byte_offset": *idx.LineLengthMaxByteOffset,
			}
		}
	} else {
		out["line_length"] = nil
		out["longest_line"] = nil
	}

	// Compression info
	out["compression_format"] = idx.CompressionFormat
	out["decompressed_size_bytes"] = idx.DecompressedSizeBytes
	out["compression_ratio"] = idx.CompressionRatio

	// Anomaly info. Anomalies is typed as *[]AnomalyRangeResult so that
	// a nil pointer serializes to JSON null (matches Python default).
	// Dereference with a nil-guard before len().
	if idx.Anomalies != nil {
		out["anomaly_count"] = len(*idx.Anomalies)
		out["anomaly_summary"] = idx.AnomalySummary
		out["anomalies"] = *idx.Anomalies
	} else {
		out["anomaly_count"] = 0
		out["anomaly_summary"] = idx.AnomalySummary
		out["anomalies"] = nil
	}
	return out
}
