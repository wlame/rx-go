package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
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

// registerTaskHandlers mounts GET /v1/tasks/{task_id} and
// DELETE /v1/tasks/{task_id}, which cancels a findings scan or a task
// over a directory.
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
		Responses:   errorResponses(api, http.StatusNotFound),
	}, func(_ context.Context, in *taskStatusInput) (*taskStatusOutput, error) {
		task, ok := s.cfg.TaskManager.Get(in.TaskID)
		if !ok {
			return nil, ErrNotFound(fmt.Sprintf("Task not found: %s", in.TaskID))
		}
		return &taskStatusOutput{Body: projectTaskState(task)}, nil
	})

	cancelResponses := errorResponses(api, http.StatusNotFound, http.StatusConflict)
	cancelResponses[strconv.Itoa(http.StatusConflict)] = jsonResponse("The task cannot be cancelled: it is not a "+
		"findings scan or a task over a directory, it has already ended, or the scan of its one file has finished "+
		"reading and is writing its answer; task_id names it.",
		api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(rxtypes.TaskConflictError{}), true, "TaskConflictError"))
	huma.Register(api, huma.Operation{
		OperationID: "task-cancel",
		Method:      http.MethodDelete,
		Path:        "/v1/tasks/{task_id}",
		Summary:     "Cancel a findings scan or a task over a directory",
		Description: "Cancels a queued or running findings task (POST /v1/findings): the scan stops and caches " +
			"nothing, and the task ends at once with status cancelled, which this answer shows. A client that " +
			"joined the same scan sees it cancelled and may start it again. A task over a directory (POST " +
			"/v1/index or POST /v1/findings given a directory) stops the file it is on at the file's next read and " +
			"takes no later file: the files ended before the cancel keep their index or findings, the file a " +
			"cancel stops writes nothing. Until its worker has stopped it shows status cancelling and keeps the " +
			"directory (a POST for it answers 409), then it ends cancelled; a second DELETE meanwhile answers " +
			"200 with it cancelling. 409 for a task of another operation (an index " +
			"task over one file, compress, chain_index), for a task that has already ended, and for a findings " +
			"task over one file whose scan has finished reading and is writing its answer (such a task never " +
			"ends cancelled with findings written).",
		Tags:      []string{"Operations"},
		Responses: cancelResponses,
	}, func(_ context.Context, in *taskStatusInput) (*taskStatusOutput, error) {
		mgr := s.cfg.TaskManager
		// Cancel refuses with one of three errors: an unknown task (404),
		// a task of another operation, or one that has ended (409).
		switch err := mgr.Cancel(in.TaskID); {
		case errors.Is(err, tasks.ErrTaskNotFound):
			return nil, ErrNotFound(fmt.Sprintf("Task not found: %s", in.TaskID))
		case errors.Is(err, tasks.ErrNotCancellable):
			return nil, ErrTaskConflict(fmt.Sprintf("Task %s cannot be cancelled: only a findings scan or a task over a directory can",
				in.TaskID), in.TaskID)
		case err != nil:
			return nil, ErrTaskConflict(fmt.Sprintf("Task %s cannot be cancelled: %v", in.TaskID, err), in.TaskID)
		}
		task, ok := mgr.Get(in.TaskID)
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
		TaskID:     t.TaskID,
		Status:     string(t.Status),
		Path:       t.Path,
		Operation:  t.Operation,
		Progress:   t.Progress(),
		CLICommand: t.CLICommand,
		Result:     t.Result,
	}
	if b := t.Bytes(); b != nil {
		out.BytesRead, out.BytesTotal = &b.Read, &b.Total
	}
	// Only a task over a directory reports its files.
	if f := t.Files(); f != nil {
		out.FilesDone, out.FilesTotal = &f.Done, &f.Total
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
