package api

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/wlame/rx/internal/fileutil"
	"github.com/wlame/rx/internal/models"
	"github.com/wlame/rx/internal/security"
)

// handleSamples handles GET /v1/samples — context extraction around byte offsets or lines.
//
// Required: path.
// One of: byte_offset (repeated) or line (repeated).
// Optional: before_context, after_context.
func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
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

	// Check file exists.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("file not found: %s", path))
		return
	}

	byteOffsets := r.URL.Query()["byte_offset"]
	lineNumbers := r.URL.Query()["line"]

	if len(byteOffsets) == 0 && len(lineNumbers) == 0 {
		writeError(w, http.StatusBadRequest, "must provide either 'byte_offset' or 'line' parameter")
		return
	}

	beforeCtx := parseIntParam(r, "before_context", 3)
	afterCtx := parseIntParam(r, "after_context", 3)

	resp := models.NewSamplesResponse(path, beforeCtx, afterCtx)

	// Process byte offsets.
	for _, raw := range byteOffsets {
		offset, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid byte_offset: %s", raw))
			return
		}

		lines, ctxErr := fileutil.GetContext(path, offset, beforeCtx, afterCtx)
		if ctxErr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("error reading context: %v", ctxErr))
			return
		}

		key := strconv.FormatInt(offset, 10)
		resp.Samples[key] = lines
		resp.Offsets[key] = -1 // line number lookup not implemented in this simplified version
	}

	// Process line numbers.
	for _, raw := range lineNumbers {
		lineNum, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid line number: %s", raw))
			return
		}

		lines, ctxErr := fileutil.GetContextByLines(path, lineNum, beforeCtx, afterCtx)
		if ctxErr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("error reading context: %v", ctxErr))
			return
		}

		key := strconv.Itoa(lineNum)
		resp.Samples[key] = lines
		resp.Lines[key] = -1 // byte offset lookup not implemented in this simplified version
	}

	writeJSON(w, http.StatusOK, resp)
}
