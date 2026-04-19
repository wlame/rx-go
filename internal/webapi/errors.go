package webapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/paths"
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
// error handling code continues to work untouched. This is documented
// in .go-rewriter/stage-5-decisions.md (Appendix A.4).

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
}

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

// ErrForbidden returns a 403 apiError with the given detail.
func ErrForbidden(detail string) huma.StatusError {
	return &apiError{Status: http.StatusForbidden, Detail: detail}
}

// ErrConflict returns a 409 apiError with the given detail.
func ErrConflict(detail string) huma.StatusError {
	return &apiError{Status: http.StatusConflict, Detail: detail}
}

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
// Path-sandbox error — user decision 6.9.3
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

// sandboxError is the wire type for path-sandbox rejections.
//
// ErrorCode is JSON-tagged "error" — the Go field is named ErrorCode
// (not Error) to avoid colliding with the error interface method.
type sandboxError struct {
	Detail    string   `json:"detail"`
	ErrorCode string   `json:"error"`
	Message   string   `json:"message"`
	Path      string   `json:"path"`
	Roots     []string `json:"roots"`
}

// Error implements the error interface.
func (e *sandboxError) Error() string { return e.Message }

// GetStatus implements huma.StatusError.
func (e *sandboxError) GetStatus() int { return http.StatusForbidden }

// NewSandboxError returns a huma.StatusError representing a
// paths.ErrPathOutsideRoots with the Go-idiomatic structured body.
func NewSandboxError(perr *paths.ErrPathOutsideRoots) huma.StatusError {
	msg := fmt.Sprintf("path %q is not within any configured --search-root", perr.Path)
	return &sandboxError{
		Detail:    "path_outside_search_root",
		ErrorCode: "path_outside_search_root",
		Message:   msg,
		Path:      perr.Path,
		Roots:     append([]string(nil), perr.Roots...),
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
