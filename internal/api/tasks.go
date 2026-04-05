package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/wlame/rx/internal/models"
)

// handleGetTask handles GET /v1/tasks/{id} — retrieve background task status.
//
// Returns 404 if the task ID is not found in the in-memory store.
// Task states: pending, running, completed, failed.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "missing task ID")
		return
	}

	task := s.TaskStore.Get(taskID)
	if task == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("task not found: %s", taskID))
		return
	}

	// Build the response from the stored task.
	startedAt := task.StartedAt.Format("2006-01-02T15:04:05Z07:00")
	resp := models.TaskStatusResponse{
		TaskID:    task.ID,
		Status:    string(task.State),
		Path:      task.Path,
		Operation: task.Operation,
		StartedAt: &startedAt,
		Error:     task.Error,
		Result:    task.Result,
	}

	if task.CompletedAt != nil {
		completed := task.CompletedAt.Format("2006-01-02T15:04:05Z07:00")
		resp.CompletedAt = &completed
	}

	writeJSON(w, http.StatusOK, resp)
}
