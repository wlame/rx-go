package webapi

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Error envelope for FastAPI compatibility
// ============================================================================
//
// The rx-viewer frontend was written against FastAPI's error shape:
//
//     {"detail": "human message"}
//
// huma v2's default error envelope uses RFC 9457 ("Problem Details"):
//
//     {"title": "...", "status": 404, "detail": "...", "errors": [...]}
//
// We keep the "detail" field but suppress the others so the frontend's
// error handling code continues to work untouched.

// humaNewError is installed as huma.NewError so every typed handler
// error (huma.Error404NotFound, huma.Error422UnprocessableEntity, etc)
// renders as {"detail": "..."}.
//
// Per huma's contract, this function must return a huma.StatusError
// (anything satisfying both the error and GetStatus() interfaces).
func humaNewError(status int, message string, errs ...error) huma.StatusError {
	detail := message
	if len(errs) > 0 {
		parts := make([]string, 0, len(errs)+1)
		if message != "" {
			parts = append(parts, message)
		}
		for _, e := range errs {
			if e != nil {
				parts = append(parts, e.Error())
			}
		}
		detail = strings.Join(parts, "; ")
	}
	return &apiError{Status: status, Detail: detail}
}

// apiError is our local huma.StatusError + error implementation.
//
// huma v2 writes this type directly to the body via huma's default
// JSON marshaler. Because the struct has a single "detail" field, the
// resulting JSON is {"detail": "..."} — exactly FastAPI's shape.
type apiError struct {
	Status int    `json:"-"`
	Detail string `json:"detail"`

	// errorType overrides the status-derived rx_errors_total label for a
	// failure the status alone does not identify: an uncompilable
	// pattern and a bad query parameter are both 400, but rx-python
	// counts them apart. Unexported, so it never reaches the response
	// body. Empty means "derive it from Status".
	errorType string
}

// MetricErrorType reports the rx_errors_total label this error should be
// counted under, or "" to let the caller derive one from the status.
func (e *apiError) MetricErrorType() string { return e.errorType }

func (e *apiError) Error() string { return e.Detail }

// GetStatus satisfies huma.StatusError.
func (e *apiError) GetStatus() int { return e.Status }

// ============================================================================
// Common error helpers — used by handler packages
// ============================================================================

// ErrNotFound returns a 404 with the given detail, already wrapped as
// an apiError. Use instead of huma.Error404NotFound when you want a
// specific detail string.
func ErrNotFound(detail string) huma.StatusError {
	return &apiError{Status: http.StatusNotFound, Detail: detail}
}

// ErrBadRequest returns a 400 apiError with the given detail.
func ErrBadRequest(detail string) huma.StatusError {
	return &apiError{Status: http.StatusBadRequest, Detail: detail}
}

// ErrInvalidRegex returns a 400 apiError for a pattern the regex engine
// refused to compile. It is a separate constructor from ErrBadRequest so
// the failure is counted as invalid_regex rather than invalid_params,
// matching rx-python.
func ErrInvalidRegex(detail string) huma.StatusError {
	return &apiError{
		Status:    http.StatusBadRequest,
		Detail:    detail,
		errorType: "invalid_regex",
	}
}

// ErrForbidden returns a 403 apiError with the given detail.
func ErrForbidden(detail string) huma.StatusError {
	return &apiError{Status: http.StatusForbidden, Detail: detail}
}

// ErrTaskConflict returns the 409 for a path whose task is already
// running: the detail sentence plus the running task's ID as task_id.
func ErrTaskConflict(detail, taskID string) huma.StatusError {
	return &taskConflictError{rxtypes.TaskConflictError{Detail: detail, TaskID: taskID}}
}

// runningOperationNames is how a 409 sentence names the operation of
// the task that holds a path, keyed by tasks.Task.Operation.
var runningOperationNames = map[string]string{
	"index":    "Indexing",
	"compress": "Compression",
}

// runningTaskConflict returns the 409 for a request refused because the
// task running holds the path. The task manager keeps one task per path
// whatever its operation, so the running task can be of another kind
// than the refused request (an index request refused by a running
// compress); the sentence and task_id both describe the running task.
// requestedPath is the path as the caller wrote it.
func runningTaskConflict(requestedPath string, running *tasks.Task) huma.StatusError {
	name, known := runningOperationNames[running.Operation]
	if !known {
		name = "Task " + running.Operation
	}
	return ErrTaskConflict(
		fmt.Sprintf("%s already in progress for %s (task: %s)", name, requestedPath, running.TaskID),
		running.TaskID,
	)
}

// taskConflictError carries the published TaskConflictError body. Like
// sandboxError below, it embeds the rxtypes struct so the schema in the
// OpenAPI document and the bytes on the wire cannot drift apart; the
// methods make it a huma.StatusError, which is what a handler returns.
type taskConflictError struct {
	rxtypes.TaskConflictError
}

// Error implements the error interface.
func (e *taskConflictError) Error() string { return e.Detail }

// GetStatus implements huma.StatusError.
func (e *taskConflictError) GetStatus() int { return http.StatusConflict }

// ErrServiceUnavailable returns a 503 apiError with the given detail.
func ErrServiceUnavailable(detail string) huma.StatusError {
	return &apiError{Status: http.StatusServiceUnavailable, Detail: detail}
}

// ErrInternal returns a 500 apiError. Never include untrusted data in
// the detail — logs are where the real stack goes.
func ErrInternal(detail string) huma.StatusError {
	return &apiError{Status: http.StatusInternalServerError, Detail: detail}
}

// ============================================================================
// Path-sandbox error
// ============================================================================
//
// paths.ErrPathOutsideRoots is returned by internal/paths when a
// requested filesystem path falls outside --search-root. On the HTTP
// side we render it as a Go-idiomatic JSON envelope (breaking from
// Python's prose message) because the frontend wants structured
// fields to build a "this path is outside your sandbox" UI.
//
// Wire format (HTTP 403):
//
//	{
//	    "detail": "path_outside_search_root",
//	    "error": "path_outside_search_root",
//	    "message": "path %q is not within any configured --search-root",
//	    "path":    "<the rejected path>",
//	    "roots":   ["<root1>", "<root2>", ...]
//	}
//
// The first "detail" field keeps the envelope parseable by FastAPI-style
// clients that expect a single "detail" key; the rest is additional
// structured info the new Go-native frontend can lean on.

// sandboxError carries the published SandboxError body and the status
// that goes with it. The body itself is rxtypes.SandboxError, so the
// schema in the OpenAPI document and the bytes on the wire cannot drift
// apart, and rx-python mirrors one shape rather than two.
type sandboxError struct {
	rxtypes.SandboxError
}

// Error implements the error interface.
func (e *sandboxError) Error() string { return e.Message }

// GetStatus implements huma.StatusError.
func (e *sandboxError) GetStatus() int { return http.StatusForbidden }

// NewSandboxError returns a huma.StatusError carrying the SandboxError
// body for a paths.ErrPathOutsideRoots.
//
// The roots are sorted: the operator's flag order is not something a
// client should have to know about, and sorting is what lets the two
// backends return byte-identical bodies for the same configuration.
func NewSandboxError(perr *paths.ErrPathOutsideRoots) huma.StatusError {
	roots := append([]string(nil), perr.Roots...)
	sort.Strings(roots)
	return &sandboxError{rxtypes.SandboxError{
		Detail:    rxtypes.SandboxErrorCode,
		ErrorCode: rxtypes.SandboxErrorCode,
		Message:   fmt.Sprintf("path %q is not within any configured --search-root", perr.Path),
		Path:      perr.Path,
		Roots:     roots,
	}}
}

// ============================================================================
// Declared error responses
// ============================================================================

// errorStatusDescriptions is the one table of error statuses the API
// declares in its OpenAPI document, with the sentence each one gets
// there. An operation picks the statuses it can answer from this table
// through errorResponses; the description is written once, here, so the
// same status reads the same on every operation.
//
// The statuses come from two places: the error constructors above,
// which handlers return, and huma itself, which answers 400 for a body
// that is not JSON and 422 for a request that fails the schema before
// the handler runs.
var errorStatusDescriptions = map[int]string{
	http.StatusBadRequest: "The request cannot be served as asked: a value rx cannot use " +
		"(an uncompilable pattern, a malformed line or offset list, a file below the index " +
		"threshold, an output file that exists), or a body that is not valid JSON",
	http.StatusForbidden: "Refused: the path is outside every configured --search-root " +
		"(SandboxError body), or it is hidden or cannot be read (ApiError body)",
	http.StatusNotFound: "The file, directory, index or task does not exist",
	http.StatusConflict: "A task for the same path is already running; task_id names it",
	http.StatusUnprocessableEntity: "The request does not match the schema: a required " +
		"parameter or field is missing, or a value has the wrong type or is out of range",
	http.StatusInternalServerError: "rx failed while serving the request",
	http.StatusServiceUnavailable:  "ripgrep is not available on this system",
}

// defaultErrorDescription describes the OpenAPI "default" response,
// which covers every status an operation does not list: a panic turned
// into 500 by recoverMiddleware, or one of huma's rarer answers such as
// 413 for an oversized body or 415 for a body that is not JSON.
const defaultErrorDescription = "Any other error"

// errorResponses builds the error part of an operation's OpenAPI
// responses: one entry per status given, described from
// errorStatusDescriptions, plus "default". Every body is the ApiError
// envelope ({"detail": ...}) except where noted:
//
//   - 403 is SandboxError for a path outside the search roots and
//     ApiError for any other refusal, so it is declared as oneOf the
//     two. The two shapes cannot be confused: each forbids properties
//     the other requires.
//   - 409 is TaskConflictError, the envelope plus the running task's ID.
//
// Registering the schemas through the API's own registry means the
// document names each once and the operations $ref it. Call it once per
// operation registration:
//
//	huma.Register(api, huma.Operation{...,
//	    Responses: errorResponses(api, http.StatusNotFound),
//	}, handler)
//
// A status missing from errorStatusDescriptions panics: it is a mistake
// in the registration code, and every test that builds a Server finds it.
func errorResponses(api huma.API, statuses ...int) map[string]*huma.Response {
	registry := api.OpenAPI().Components.Schemas
	envelope := registry.Schema(reflect.TypeOf(apiError{}), true, "ApiError")
	bodies := map[int]*huma.Schema{
		http.StatusForbidden: {OneOf: []*huma.Schema{
			registry.Schema(reflect.TypeOf(rxtypes.SandboxError{}), true, "SandboxError"),
			envelope,
		}},
		http.StatusConflict: registry.Schema(reflect.TypeOf(rxtypes.TaskConflictError{}), true, "TaskConflictError"),
	}

	responses := map[string]*huma.Response{
		"default": jsonResponse(defaultErrorDescription, envelope),
	}
	for _, status := range statuses {
		description, known := errorStatusDescriptions[status]
		if !known {
			panic(fmt.Sprintf("webapi: status %d has no entry in errorStatusDescriptions", status))
		}
		body, special := bodies[status]
		if !special {
			body = envelope
		}
		responses[strconv.Itoa(status)] = jsonResponse(description, body)
	}
	return responses
}

// jsonResponse is an OpenAPI response with an application/json body.
func jsonResponse(description string, schema *huma.Schema) *huma.Response {
	return &huma.Response{
		Description: description,
		Content:     map[string]*huma.MediaType{"application/json": {Schema: schema}},
	}
}

// ClassifyPathError promotes a path-validation error to the correct
// HTTP status + body shape. Returns (status, huma.StatusError) so the
// handler can just `return nil, err` with the mapped error.
func ClassifyPathError(err error) huma.StatusError {
	var perr *paths.ErrPathOutsideRoots
	if errors.As(err, &perr) {
		return NewSandboxError(perr)
	}
	return ErrForbidden(err.Error())
}
