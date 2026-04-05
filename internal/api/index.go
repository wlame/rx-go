package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	json "github.com/goccy/go-json"

	"github.com/wlame/rx/internal/index"
	"github.com/wlame/rx/internal/models"
	"github.com/wlame/rx/internal/security"
)

// handleGetIndex handles GET /v1/index — return cached index for a file path.
func (s *Server) handleGetIndex(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "missing required query parameter: path")
		return
	}

	// Validate path is within search roots.
	if len(s.Config.SearchRoots) > 0 {
		resolved, err := security.ValidatePath(path, s.Config.SearchRoots)
		if err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		path = resolved
	}

	// Compute the cache path and try to load the index.
	cachePath := index.IndexCachePath(s.Config.CacheDir, path)
	idx, err := index.Load(cachePath)
	if err != nil || idx == nil {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("no index found for %s. Use POST /v1/index to create an indexing task.", path))
		return
	}

	writeJSON(w, http.StatusOK, idx)
}

// indexPostRequest is the JSON body for POST /v1/index.
type indexPostRequest struct {
	Path    string `json:"path"`
	Force   bool   `json:"force"`
	Analyze bool   `json:"analyze"`
}

// handlePostIndex handles POST /v1/index — start a background indexing task.
func (s *Server) handlePostIndex(w http.ResponseWriter, r *http.Request) {
	var req indexPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "missing required field: path")
		return
	}

	// Validate path is within search roots.
	normalizedPath := req.Path
	if len(s.Config.SearchRoots) > 0 {
		resolved, err := security.ValidatePath(req.Path, s.Config.SearchRoots)
		if err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		normalizedPath = resolved
	}

	// Check that file exists.
	if _, err := os.Stat(normalizedPath); os.IsNotExist(err) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("file not found: %s", req.Path))
		return
	}

	// Create a background task.
	taskID := s.TaskStore.Create("index", normalizedPath)
	cfg := s.Config

	// Launch the indexing goroutine.
	go func() {
		s.TaskStore.SetRunning(taskID)
		slog.Info("starting background index", "task_id", taskID, "path", normalizedPath)

		idx, err := index.BuildIndex(normalizedPath, cfg)
		if err != nil {
			slog.Error("background index failed", "task_id", taskID, "error", err)
			s.TaskStore.Fail(taskID, err.Error())
			return
		}

		// Convert index to a generic map for the task result.
		result := indexToMap(idx)
		s.TaskStore.Complete(taskID, result)
		slog.Info("background index completed", "task_id", taskID, "path", normalizedPath)
	}()

	// Return the task response immediately.
	task := s.TaskStore.Get(taskID)
	startedAt := task.StartedAt.Format("2006-01-02T15:04:05Z07:00")

	resp := models.TaskResponse{
		TaskID:    taskID,
		Status:    string(task.State),
		Message:   fmt.Sprintf("Indexing task started for %s", req.Path),
		Path:      normalizedPath,
		StartedAt: &startedAt,
	}

	writeJSON(w, http.StatusOK, resp)
}

// indexToMap converts a FileIndex to a generic map for task result storage.
func indexToMap(idx *models.FileIndex) map[string]interface{} {
	// Marshal then unmarshal to convert struct to map — simple and correct.
	data, err := json.Marshal(idx)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	result["success"] = true
	return result
}
