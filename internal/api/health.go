package api

import (
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// handleHealth responds with system health and introspection data.
//
// Returns:
//   - version, uptime, Go version
//   - ripgrep version (by running `rg --version`)
//   - configured search roots
//   - key configuration values
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	rgVersion := getRgVersion()

	uptime := time.Since(s.StartTime).Seconds()

	// Build search roots list — nil when empty (matches Python behavior).
	var searchRoots interface{}
	if len(s.Config.SearchRoots) > 0 {
		searchRoots = s.Config.SearchRoots
	}

	resp := map[string]interface{}{
		"status":             "ok",
		"ripgrep_available":  rgVersion != "",
		"search_roots":       searchRoots,
		"app_version":        version,
		"go_version":         runtime.Version(),
		"uptime_seconds":     uptime,
		"rg_version":         rgVersion,
		"os_info": map[string]string{
			"system":  runtime.GOOS,
			"arch":    runtime.GOARCH,
			"version": runtime.Version(),
		},
		"constants": map[string]interface{}{
			"LOG_LEVEL":              s.Config.LogLevel,
			"DEBUG_MODE":             s.Config.Debug,
			"LINE_SIZE_ASSUMPTION_KB": s.Config.MaxLineSizeKB,
			"MAX_SUBPROCESSES":       s.Config.MaxSubprocesses,
			"MIN_CHUNK_SIZE_MB":      s.Config.MinChunkSizeMB,
			"MAX_FILES":              s.Config.MaxFiles,
			"CACHE_DIR":              s.Config.CacheDir,
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

// version is the application version, injected at build time or set by the CLI package.
// The serve command sets this before creating the server.
var version = "dev"

// SetVersion sets the application version string used in health responses.
func SetVersion(v string) { version = v }

// getRgVersion runs `rg --version` and returns the first line, or "" if rg is not available.
func getRgVersion() string {
	out, err := exec.Command("rg", "--version").Output()
	if err != nil {
		return ""
	}
	// rg --version outputs something like "ripgrep 14.1.0\n..."
	// Return just the first line.
	lines := strings.SplitN(string(out), "\n", 2)
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0])
	}
	return ""
}
