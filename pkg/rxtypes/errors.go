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
