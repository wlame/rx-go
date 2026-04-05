package api

import (
	"net/http"

	"github.com/wlame/rx/internal/models"
)

// handleComplexity handles GET /v1/complexity — regex complexity analysis stub.
//
// This is a STUB endpoint. It accepts a regex query parameter and returns a
// ComplexityResponse with all fields set to zero/null/"not_implemented" values.
// HTTP 200 is returned (not 501) because the endpoint exists — the feature is deferred.
func (s *Server) handleComplexity(w http.ResponseWriter, r *http.Request) {
	regex := r.URL.Query().Get("regex")
	if regex == "" {
		writeError(w, http.StatusBadRequest, "missing required query parameter: regex")
		return
	}

	resp := models.NewStubComplexityResponse(regex)
	writeJSON(w, http.StatusOK, resp)
}
