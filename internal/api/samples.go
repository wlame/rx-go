package api

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/wlame/rx/internal/fileutil"
	"github.com/wlame/rx/internal/index"
	"github.com/wlame/rx/internal/models"
	"github.com/wlame/rx/internal/security"
)

// parseValueOrRange parses a single element from a comma-separated list.
// It distinguishes three forms:
//   - Plain integer: "100" → (100, nil, nil)
//   - Negative integer: "-5" → (-5, nil, nil) — starts with '-' and has no other '-'
//   - Range: "100-200" → (100, &200, nil) — two integers separated by '-'
//
// The ambiguity between negative values and ranges is resolved by position of '-':
// if the string starts with '-' and contains no further '-', it is negative.
func parseValueOrRange(raw string) (start int, end *int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil, fmt.Errorf("empty value")
	}

	// Check if this is a range: find '-' that is not the leading negative sign.
	// A range looks like "100-200". A negative looks like "-5".
	// If the string starts with '-', strip it temporarily to look for a second '-'.
	dashIdx := -1
	searchFrom := 0
	if raw[0] == '-' {
		// Could be negative single or a range starting with a negative (not supported, but handle).
		searchFrom = 1
	}
	rest := raw[searchFrom:]
	idx := strings.Index(rest, "-")
	if idx >= 0 {
		dashIdx = searchFrom + idx
	}

	if dashIdx < 0 {
		// No range separator — plain or negative integer.
		v, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return 0, nil, fmt.Errorf("invalid integer: %s", raw)
		}
		return v, nil, nil
	}

	// Range: split on the dash.
	leftStr := raw[:dashIdx]
	rightStr := raw[dashIdx+1:]

	left, parseErr := strconv.Atoi(strings.TrimSpace(leftStr))
	if parseErr != nil {
		return 0, nil, fmt.Errorf("invalid range start: %s", leftStr)
	}
	right, parseErr := strconv.Atoi(strings.TrimSpace(rightStr))
	if parseErr != nil {
		return 0, nil, fmt.Errorf("invalid range end: %s", rightStr)
	}

	return left, &right, nil
}

// handleSamples handles GET /v1/samples — context extraction around byte offsets or lines.
//
// Required: path.
// One of: byte_offset (repeated) or line (repeated), or offsets/lines (comma-separated).
// Optional: before_context, after_context, context.
//
// Each element in the comma-separated list can be:
//   - A plain integer: "100" → single value with context
//   - A range: "100-200" → exact range, no context applied
//   - A negative integer: "-5" → from end of file (lines only)
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

	// Mutual exclusivity: cannot use both offsets and lines in the same request.
	// This matches the Python reference implementation's behavior.
	if len(byteOffsets) > 0 && len(lineNumbers) > 0 {
		writeError(w, http.StatusBadRequest, "cannot use both 'offsets' and 'lines'; provide only one")
		return
	}

	// Context resolution priority (matches Python):
	//   1. Explicit before_context / after_context (highest priority)
	//   2. Shared context param (sets both)
	//   3. Default of 3
	beforeCtx := 3
	afterCtx := 3

	if ctx := r.URL.Query().Get("context"); ctx != "" {
		if v, err := strconv.Atoi(ctx); err == nil {
			beforeCtx = v
			afterCtx = v
		}
	}

	// Specific before/after override context when present.
	if v := r.URL.Query().Get("before_context"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			beforeCtx = parsed
		}
	}
	if v := r.URL.Query().Get("after_context"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			afterCtx = parsed
		}
	}

	resp := models.NewSamplesResponse(path, beforeCtx, afterCtx)

	// Load file index once for this request. When available, GetContextByLines and
	// GetLineRange use it to binary-search near the target line instead of scanning
	// from byte 0 on every call (critical for multi-GB files).
	var fileIdx *models.FileIndex
	if s.Config.CacheDir != "" {
		cachePath := index.IndexCachePath(s.Config.CacheDir, path)
		idx, loadErr := index.Load(cachePath)
		if loadErr == nil && idx != nil && index.Validate(idx, path) {
			fileIdx = idx
		}
	}

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

	// Resolve total line count lazily (needed for negative line numbers).
	totalLines := -1
	countLines := func() (int, error) {
		if totalLines >= 0 {
			return totalLines, nil
		}
		n, err := fileutil.CountLines(path)
		if err != nil {
			return 0, err
		}
		totalLines = n
		return totalLines, nil
	}

	// Process byte offsets.
	for _, raw := range byteOffsets {
		start, end, err := parseValueOrRange(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid byte offset: %s", raw))
			return
		}

		if end != nil {
			// Range: return lines covering [start, end) bytes, no context.
			lines, rangeErr := fileutil.GetByteRange(path, int64(start), int64(*end))
			if rangeErr != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("error reading byte range: %v", rangeErr))
				return
			}
			key := fmt.Sprintf("%d-%d", start, *end)
			resp.Samples[key] = lines
			resp.Offsets[key] = -1
		} else {
			// Single value with context.
			lines, ctxErr := fileutil.GetContext(path, int64(start), beforeCtx, afterCtx)
			if ctxErr != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("error reading context: %v", ctxErr))
				return
			}
			key := strconv.Itoa(start)
			resp.Samples[key] = lines
			resp.Offsets[key] = -1
		}
	}

	// Process line numbers.
	for _, raw := range lineNumbers {
		start, end, err := parseValueOrRange(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid line number: %s", raw))
			return
		}

		if end != nil {
			// Range: return exact lines [start, end], no context.
			lines, rangeErr := fileutil.GetLineRange(path, start, *end, fileIdx)
			if rangeErr != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("error reading line range: %v", rangeErr))
				return
			}
			key := fmt.Sprintf("%d-%d", start, *end)
			resp.Samples[key] = lines
			resp.Lines[key] = -1
		} else {
			// Single value — resolve negative to positive line number.
			lineNum := start
			if lineNum < 0 {
				total, countErr := countLines()
				if countErr != nil {
					writeError(w, http.StatusInternalServerError, fmt.Sprintf("error counting lines: %v", countErr))
					return
				}
				// -1 means last line, -2 means second to last, etc.
				lineNum = total + lineNum + 1
				if lineNum < 1 {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("negative line %d resolves to %d, out of bounds", start, lineNum))
					return
				}
			}

			lines, ctxErr := fileutil.GetContextByLines(path, lineNum, beforeCtx, afterCtx, fileIdx)
			if ctxErr != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("error reading context: %v", ctxErr))
				return
			}
			key := strconv.Itoa(start)
			resp.Samples[key] = lines
			resp.Lines[key] = -1
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
