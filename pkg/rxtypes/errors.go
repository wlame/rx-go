package rxtypes

// ErrorResponse is the FastAPI-compatible single-field error envelope.
//
// Python FastAPI returns {"detail": "..."} on every HTTPException,
// including validation errors (after we override the 422 handler).
// The Go port mimics this exactly so the rx-viewer frontend can keep
// its error-handling code untouched.
type ErrorResponse struct {
	Detail string `json:"detail"`
}

// SandboxError is the body both backends return with HTTP 403 when a
// requested path falls outside every configured `--search-root`.
//
// The structured fields exist so a client can build a "this path is
// outside your sandbox" panel rather than print a sentence: `error` is a
// stable machine code to branch on, `path` names what was refused, and
// `roots` lists what would have been accepted. `detail` repeats the code
// so a FastAPI-style client that only reads that key still gets
// something it can compare.
//
// `roots` is sorted, so two backends configured with the same roots
// return byte-identical bodies whatever order the operator wrote the
// flags in.
//
// ErrorCode is JSON-tagged "error"; the Go field is named ErrorCode so
// it does not collide with the error interface method on the wrappers
// that embed this shape.
type SandboxError struct {
	Detail    string   `json:"detail" doc:"Machine code, repeated from error for single-key clients."`
	ErrorCode string   `json:"error" doc:"Stable machine code: always path_outside_search_root."`
	Message   string   `json:"message" doc:"Human-readable explanation naming the refused path."`
	Path      string   `json:"path" doc:"The path that was refused, as the caller supplied it."`
	Roots     []string `json:"roots" doc:"The configured search roots, sorted."`
}

// SandboxErrorCode is the value of SandboxError.ErrorCode and
// SandboxError.Detail. Clients branch on it.
const SandboxErrorCode = "path_outside_search_root"
