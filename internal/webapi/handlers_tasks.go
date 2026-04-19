package webapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// taskStatusInput is the path-param shape for GET /v1/tasks/{task_id}.
type taskStatusInput struct {
	TaskID string `path:"task_id" example:"6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53" doc:"Task UUID"`
}

// taskStatusOutput wraps TaskStatusResponse.
type taskStatusOutput struct {
	Body rxtypes.TaskStatusResponse
}

// registerTaskHandlers mounts GET /v1/tasks/{task_id}.
//
// Matches rx-python/src/rx/web.py:1879-1913. The task manager lives in
// internal/tasks; we just look up and project the state.
func registerTaskHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "task-status",
		Method:      http.MethodGet,
		Path:        "/v1/tasks/{task_id}",
		Summary:     "Get status of a background task",
		Description: "Returns queued/running/completed/failed state and, on completion, the task result.",
		Tags:        []string{"Operations"},
	}, func(_ context.Context, in *taskStatusInput) (*taskStatusOutput, error) {
		task, ok := s.cfg.TaskManager.Get(in.TaskID)
		if !ok {
			return nil, ErrNotFound(fmt.Sprintf("Task not found: %s", in.TaskID))
		}
		return &taskStatusOutput{Body: projectTaskState(task)}, nil
	})
}

// projectTaskState copies a tasks.Task (internal) to a
// rxtypes.TaskStatusResponse (wire). Split out so POST /v1/index and
// POST /v1/compress can use it to build their immediate response
// without duplicating field mapping.
func projectTaskState(t *tasks.Task) rxtypes.TaskStatusResponse {
	out := rxtypes.TaskStatusResponse{
		TaskID:    t.TaskID,
		Status:    string(t.Status),
		Path:      t.Path,
		Operation: t.Operation,
		Result:    t.Result,
	}
	if !t.StartedAt.IsZero() {
		s := formatTaskTime(t.StartedAt)
		out.StartedAt = &s
	}
	if t.CompletedAt != nil && !t.CompletedAt.IsZero() {
		s := formatTaskTime(*t.CompletedAt)
		out.CompletedAt = &s
	}
	if t.Error != "" {
		e := t.Error
		out.Error = &e
	}
	return out
}

// formatTaskTime renders a time.Time as an RFC3339-compatible ISO 8601
// string with UTC suffix — matching Python's datetime.isoformat()
// output when called on a UTC-aware datetime.
//
// Example: 2026-04-18T15:23:45.123456Z. Using microsecond precision
// keeps parity with Python; Go's native nanosecond precision would
// add three extra digits that Python doesn't emit.
func formatTaskTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}
