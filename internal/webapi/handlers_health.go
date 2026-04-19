package webapi

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// healthOutput wraps the rxtypes.HealthResponse body for huma. huma
// requires the handler return type to have a Body field (or no body);
// the struct below follows that convention across the whole webapi
// package.
type healthOutput struct {
	Body rxtypes.HealthResponse
}

// registerHealthHandlers mounts GET /health.
//
// Matches rx-python/src/rx/web.py:291-331. Key differences vs Python:
//   - python_version → go_version (Appendix A.1 of the spec).
//   - python_packages → go_packages (see buildGoPackages).
//   - Everything else stays on its Python name so rx-viewer's health
//     dashboard code doesn't need a branch.
func registerHealthHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/health",
		Summary:     "Health check and system introspection",
		Description: "Returns service status, ripgrep availability, app version, OS info, " +
			"environment variables, and hook configuration.",
		Tags: []string{"General"},
	}, func(_ context.Context, _ *struct{}) (*healthOutput, error) {
		return &healthOutput{Body: buildHealthResponse(s)}, nil
	})
}

// buildHealthResponse assembles the full HealthResponse struct. Factored
// out of the handler so tests can call it directly without spinning up
// a full httptest server.
func buildHealthResponse(s *Server) rxtypes.HealthResponse {
	return rxtypes.HealthResponse{
		Status:           "ok",
		RipgrepAvailable: s.cfg.RipgrepPath != "",
		AppVersion:       s.cfg.AppVersion,
		GoVersion:        runtime.Version(),
		OSInfo:           getOSInfo(),
		SystemResources:  getSystemResources(),
		GoPackages:       getGoPackages(),
		Constants:        getConstants(),
		Environment:      getAppEnvVariables(),
		Hooks:            getHookConfigMap(),
		DocsURL:          "https://github.com/wlame/rx-tool",
		SearchRoots:      getSearchRootsForHealth(),
	}
}

// getOSInfo is the runtime.GOOS/GOARCH bundle for /health.
func getOSInfo() map[string]string {
	return map[string]string{
		"system":  runtime.GOOS,
		"machine": runtime.GOARCH,
		// release/version are Linux-specific on Python's side; Go doesn't
		// have a cross-platform way to get them without cgo. We report
		// the compiler instead, which is more useful for debugging.
		"compiler": runtime.Compiler,
		"version":  runtime.Version(),
	}
}

// getSystemResources returns a best-effort resource snapshot. Python's
// version uses psutil; Go's stdlib doesn't expose RAM totals, but on
// Linux we can parse /proc/meminfo without cgo. The same approach covers
// the common deployment target for rx-go.
//
// Stage 9 Round 2 R1-B9 fix: previously all three ram_* fields were
// hard-coded null. With /proc/meminfo parsing we now populate them on
// Linux; macOS / Windows still return null because adding platform-
// specific syscalls would require cgo or build tags and the containers
// we care about are Linux-based.
func getSystemResources() map[string]any {
	out := map[string]any{
		"cpu_cores":          runtime.NumCPU(),
		"cpu_cores_physical": runtime.NumCPU(),
		// Explicit nulls for parity with Python's schema — rx-viewer
		// displays "N/A" when a field is nil. These are overwritten
		// below when /proc/meminfo is available.
		"ram_total_gb":     nil,
		"ram_available_gb": nil,
		"ram_percent_used": nil,
	}
	if totalGB, availGB, percentUsed, ok := readMemInfoGB(); ok {
		out["ram_total_gb"] = totalGB
		out["ram_available_gb"] = availGB
		out["ram_percent_used"] = percentUsed
	}
	return out
}

// readMemInfoGB reads /proc/meminfo (Linux) and returns
// (total_gb, available_gb, percent_used, ok). Returns ok=false on any
// platform without /proc/meminfo or when the file can't be parsed, so
// the caller can fall back to null.
//
// Units: kilobytes in /proc/meminfo → gibibytes in Python's output.
// Python uses GB = 1024^3 (psutil convention) — match that here.
func readMemInfoGB() (totalGB, availGB, percentUsed float64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, false
	}
	var totalKB, availKB int64
	haveTotal, haveAvail := false, false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = parseMemKB(line)
			haveTotal = totalKB > 0
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB = parseMemKB(line)
			haveAvail = availKB > 0
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if !haveTotal || !haveAvail {
		return 0, 0, 0, false
	}
	// Kilobytes → gibibytes. Divide by 1024*1024 to get GB.
	totalGB = float64(totalKB) / (1024 * 1024)
	availGB = float64(availKB) / (1024 * 1024)
	// Round to 1 decimal place to match Python's psutil rounding.
	totalGB = float64(int(totalGB*10)) / 10
	availGB = float64(int(availGB*10)) / 10
	if totalKB > 0 {
		percentUsed = float64(int((float64(totalKB-availKB)/float64(totalKB))*1000)) / 10
	}
	return totalGB, availGB, percentUsed, true
}

// parseMemKB extracts the integer kB value from a /proc/meminfo line
// like "MemTotal:       16384000 kB". Returns 0 on parse failure.
func parseMemKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// getGoPackages reports rx-go's own dependency versions via the build
// info embedded in the binary at compile time.
//
// This mirrors Python's "python_packages" field but using go-module
// metadata: the runtime/debug.BuildInfo API surfaces exactly the
// dependencies compiled into the current binary.
func getGoPackages() map[string]string {
	out := map[string]string{}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	// Hoist the interesting packages. rx-viewer's health dashboard
	// shows these by name, so the key list must match rx-python's
	// key_packages convention (load-bearing for tests).
	keyPackages := map[string]bool{
		"github.com/danielgtaylor/huma/v2":    true,
		"github.com/go-chi/chi/v5":            true,
		"github.com/klauspost/compress":       true,
		"github.com/prometheus/client_golang": true,
		"github.com/spf13/cobra":              true,
		"github.com/google/uuid":              true,
	}
	for _, dep := range info.Deps {
		if keyPackages[dep.Path] {
			out[dep.Path] = dep.Version
		}
	}
	return out
}

// getConstants returns the map rx-viewer uses to show "Current tunables"
// — min chunk size, max files, etc. Same keys as Python.
func getConstants() map[string]any {
	return map[string]any{
		"LOG_LEVEL":               getLogLevelName(),
		"DEBUG_MODE":              config.DebugMode(),
		"LINE_SIZE_ASSUMPTION_KB": config.MaxLineSizeKB(),
		"MAX_SUBPROCESSES":        config.MaxSubprocesses(),
		"MIN_CHUNK_SIZE_MB":       config.MinChunkSizeMB(),
		"MAX_FILES":               config.MaxFiles(),
		// NEWLINE_SYMBOL uses Python's repr() format (quoted escape
		// sequence). Matching Python exactly makes string comparisons
		// easy in parity tests. See formatPythonRepr below.
		"NEWLINE_SYMBOL": formatPythonRepr(config.GetStringEnv("NEWLINE_SYMBOL", "\\n")),
		"CACHE_DIR":      config.GetCacheBase(),
	}
}

// formatPythonRepr mirrors Python's repr() for NEWLINE_SYMBOL's
// health-check surface. Python's rx.utils decodes the literal env value
// (e.g. "\n") into an actual newline character, then /health emits
// repr(NEWLINE_SYMBOL) which for "\n" yields the 4-char string `'\n'`
// (apostrophe, backslash, n, apostrophe).
//
// Stage 9 Round 2 R1-B9 fix: Round 1's implementation used a ReplaceAll
// that escaped the literal backslash a SECOND time, producing `'\\n'`
// on the wire. Python's path (repr of the decoded character) produces
// `'\n'` on the wire. Mismatch was visible in the /health JSON
// constants map.
//
// strconv.Quote is the closest stdlib match to Python's repr():
//   - For a real newline char, strconv.Quote returns `"\n"` (double
//     quotes + backslash + n). We strip the outer double-quotes and
//     substitute Python's single-quote convention.
func formatPythonRepr(envLiteral string) string {
	// Decode the literal escape sequences ("\\n" → actual \n). Mirrors
	// rx-python/src/rx/utils.py's two-step replace.
	decoded := strings.ReplaceAll(envLiteral, "\\r", "\r")
	decoded = strings.ReplaceAll(decoded, "\\n", "\n")
	quoted := strconv.Quote(decoded)
	// Swap Python's single-quote convention in place of Go's double.
	return "'" + quoted[1:len(quoted)-1] + "'"
}

// getLogLevelName returns the slog level /health exposes. Prefers the
// value programmatically wired via SetRequestedLogLevel (set by serve
// startup based on flags/env at the time it configured slog). Falls
// back to the RX_LOG_LEVEL env var for callers that haven't adopted
// SetRequestedLogLevel — this preserves legacy behavior while fixing
// the divergence flagged in Stage 8 Reviewer 3 High #13.
//
// slog doesn't expose the effective level on its default handler, so
// the package-level requested-level pointer is the single source of
// truth. If a future version exposes slog.Handler.Enabled probing
// that can inspect the default handler directly, this helper can
// be simplified.
func getLogLevelName() string {
	return requestedLogLevelName(os.Getenv("RX_LOG_LEVEL"))
}

// getAppEnvVariables returns every env var whose name starts with an
// rx-related prefix. Matches Python's get_app_env_variables.
func getAppEnvVariables() map[string]string {
	prefixes := []string{"RX_", "UVICORN_", "PROMETHEUS_", "NEWLINE_SYMBOL"}
	out := map[string]string{}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		v := kv[eq+1:]
		if k == "NEWLINE_SYMBOL" {
			out[k] = v
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				out[k] = v
				break
			}
		}
	}
	return out
}

// getHookConfigMap returns a JSON-friendly view of the effective hook
// env config. Same keys as Python's get_hook_env_config().
func getHookConfigMap() map[string]any {
	env := hooks.HookEnvFromEnv()
	return map[string]any{
		"RX_HOOK_ON_FILE_URL":     env.OnFileURL,
		"RX_HOOK_ON_MATCH_URL":    env.OnMatchURL,
		"RX_HOOK_ON_COMPLETE_URL": env.OnCompleteURL,
		"RX_DISABLE_CUSTOM_HOOKS": env.DisableCustom,
		"custom_hooks_enabled":    !env.DisableCustom,
		"hooks_configured":        env.OnFileURL != "" || env.OnMatchURL != "" || env.OnCompleteURL != "",
	}
}

// getSearchRootsForHealth returns the search-roots slice (or nil when
// the sandbox is off, matching Python's null).
func getSearchRootsForHealth() []string {
	roots := paths.GetSearchRoots()
	if len(roots) == 0 {
		return nil
	}
	return roots
}
