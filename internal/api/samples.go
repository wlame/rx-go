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

	// Accept both "byte_offset" (repeated) and "offsets" (comma-separated) for compatibility
	// with the Python API. The frontend uses "offsets" (comma-separated string).
	byteOffsets := r.URL.Query()["byte_offset"]
	if offsets := r.URL.Query().Get("offsets"); offsets != "" {
		for _, part := range strings.Split(offsets, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				byteOffsets = append(byteOffsets, part)
			}
		}
	}

	lineNumbers := r.URL.Query()["line"]
	if lines := r.URL.Query().Get("lines"); lines != "" {
		for _, part := range strings.Split(lines, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				lineNumbers = append(lineNumbers, part)
			}
		}
	}

	if len(byteOffsets) == 0 && len(lineNumbers) == 0 {
		writeError(w, http.StatusBadRequest, "must provide either 'offsets' or 'lines' parameter")
		return
	}

	beforeCtx := parseIntParam(r, "before_context", 3)
	afterCtx := parseIntParam(r, "after_context", 3)

	// Handle shared "context" param (sets both before and after).
	if ctx := r.URL.Query().Get("context"); ctx != "" {
		if v, err := strconv.Atoi(ctx); err == nil {
			beforeCtx = v
			afterCtx = v
		}
	}

	resp := models.NewSamplesResponse(path, beforeCtx, afterCtx)

	// Build CLI command for display.
	cliParts := []string{"rx", "samples", path}
	for _, o := range byteOffsets {
		cliParts = append(cliParts, "-b", o)
	}
	for _, l := range lineNumbers {
		cliParts = append(cliParts, "-l", l)
	}
	if beforeCtx > 0 || afterCtx > 0 {
		cliParts = append(cliParts, "-c", strconv.Itoa(beforeCtx))
	}
	cmd := strings.Join(cliParts, " ")
	resp.CLICommand = &cmd

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
