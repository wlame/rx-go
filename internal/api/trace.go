package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/wlame/rx/internal/engine"
)

// handleTrace handles GET /v1/trace — the primary search endpoint.
//
// Required query params: path (repeated), regexp (repeated).
// Optional: max_results, rg_extra_args, before_context, after_context,
// use_cache, use_index, on_file_url, on_match_url, on_complete_url.
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	paths := r.URL.Query()["path"]
	patterns := r.URL.Query()["regexp"]

	if len(paths) == 0 {
		writeError(w, http.StatusBadRequest, "missing required query parameter: path")
		return
	}
	if len(patterns) == 0 {
		writeError(w, http.StatusBadRequest, "missing required query parameter: regexp")
		return
	}

	// Parse optional integer parameters.
	maxResults := parseIntParam(r, "max_results", 0)
	beforeCtx := parseIntParam(r, "before_context", 0)
	afterCtx := parseIntParam(r, "after_context", 0)

	// Parse boolean parameters — default to true for cache/index usage.
	useCache := parseBoolParam(r, "use_cache", true)
	useIndex := parseBoolParam(r, "use_index", true)

	// rg_extra_args can be repeated.
	rgExtraArgs := r.URL.Query()["rg_extra_args"]

	// Build the engine request.
	req := engine.TraceRequest{
		Paths:         paths,
		Patterns:      patterns,
		MaxResults:    maxResults,
		RgExtraArgs:   rgExtraArgs,
		ContextBefore: beforeCtx,
		ContextAfter:  afterCtx,
		UseCache:      useCache,
		UseIndex:      useIndex,
	}

	// Run the trace engine. This blocks for the duration of the search but
	// the HTTP server handles each request in its own goroutine, so other
	// requests are not blocked.
	resp, err := engine.Trace(r.Context(), req)
	if err != nil {
		// Provide a clear error message: distinguish validation errors from internal failures.
		if strings.Contains(err.Error(), "invalid regex") {
			writeError(w, http.StatusBadRequest, err.Error())
		} else if strings.Contains(err.Error(), "path not found") {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("search failed: %v", err))
		}
		return
	}

	// Set the request ID if not already set by the engine.
	if resp.RequestID == "" {
		resp.RequestID = uuid.New().String()
	}

	// Reflect max_results back in the response (nil when 0/unset).
	if maxResults > 0 {
		resp.MaxResults = &maxResults
	}

	writeJSON(w, http.StatusOK, resp)
}

// parseIntParam reads a query parameter as an integer, returning defaultVal on missing/invalid.
func parseIntParam(r *http.Request, key string, defaultVal int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultVal
	}
	return n
}

// parseBoolParam reads a query parameter as a boolean.
// Recognizes "true", "1", "yes" as true; everything else (including absent) as defaultVal.
func parseBoolParam(r *http.Request, key string, defaultVal bool) bool {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return defaultVal
	}
	switch raw {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		return defaultVal
	}
}
