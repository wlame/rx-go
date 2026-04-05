package api

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleFrontendCatchAll serves the SPA frontend files. It is mounted as the
// lowest-priority catch-all route so that API routes are matched first.
//
// Behavior:
//   - If a static file matching the requested path exists, serve it.
//   - If no file matches, serve index.html (SPA client-side routing).
//   - If no frontend is installed, return a plain 404 with a helpful message.
//   - All paths are validated for directory traversal attacks.
func (s *Server) handleFrontendCatchAll(w http.ResponseWriter, r *http.Request) {
	if s.FrontendDir == "" {
		writeError(w, http.StatusNotFound, "frontend not installed")
		return
	}

	// Clean the request path. chi catch-all may include a leading slash.
	reqPath := strings.TrimPrefix(r.URL.Path, "/")
	if reqPath == "" {
		reqPath = "index.html"
	}

	// Validate the path for directory traversal.
	resolvedPath, err := validateFrontendPath(s.FrontendDir, reqPath)
	if err != nil {
		slog.Debug("frontend: path rejected", "path", reqPath, "error", err)
		// Fall back to serving index.html for SPA routing.
		resolvedPath = filepath.Join(s.FrontendDir, "index.html")
		if _, statErr := os.Stat(resolvedPath); statErr != nil {
			writeError(w, http.StatusNotFound, "frontend not installed")
			return
		}
	}

	http.ServeFile(w, r, resolvedPath)
}

// handleFavicon serves the favicon.ico file from the frontend directory.
// If no frontend is installed or the favicon doesn't exist, returns 204 No Content.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if s.FrontendDir == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	faviconPath := filepath.Join(s.FrontendDir, "favicon.ico")
	if _, err := os.Stat(faviconPath); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	http.ServeFile(w, r, faviconPath)
}

// validateFrontendPath validates that the requested path is safe (no traversal)
// and resolves to an existing file within the frontend directory.
func validateFrontendPath(baseDir, requestedPath string) (string, error) {
	// Clean the path to normalize ../ sequences.
	cleaned := filepath.Clean("/" + requestedPath)
	cleaned = strings.TrimPrefix(cleaned, "/")

	fullPath := filepath.Join(baseDir, cleaned)

	resolvedBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}

	resolvedFull, err := filepath.Abs(fullPath)
	if err != nil {
		return "", err
	}

	// Containment check: the resolved path must be within the base directory.
	if !isPathWithin(resolvedFull, resolvedBase) {
		return "", &pathTraversalError{path: requestedPath}
	}

	// Check the file exists and is not a directory.
	info, err := os.Stat(resolvedFull)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		// For directories, try index.html inside them.
		indexPath := filepath.Join(resolvedFull, "index.html")
		if _, err := os.Stat(indexPath); err == nil {
			return indexPath, nil
		}
		return "", &pathTraversalError{path: requestedPath}
	}

	return resolvedFull, nil
}

// isPathWithin returns true if child is equal to or a subdirectory/file under parent.
func isPathWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(child, prefix)
}

// pathTraversalError indicates a directory traversal attempt was blocked.
type pathTraversalError struct {
	path string
}

func (e *pathTraversalError) Error() string {
	return "path traversal blocked: " + e.path
}
