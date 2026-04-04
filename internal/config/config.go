// Package config loads and validates all RX_* environment variables into typed configuration structs.
//
// Configuration is purely environment-variable driven (no config files). Each field has a sensible
// default matching the Python reference implementation. The Load function reads the process
// environment once and returns an immutable Config value.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Default values — matching the Python reference implementation exactly.
const (
	DefaultMaxLineSizeKB   = 8
	DefaultMaxSubprocesses = 20
	DefaultMinChunkSizeMB  = 20
	DefaultMaxFiles        = 1000
	DefaultLargeFileMB     = 50
	DefaultFrameBatchMB    = 32 // Go-specific: target decompressed size per seekable zstd batch
	DefaultLogLevel        = "WARNING"
	DefaultSampleSizeLines = 1_000_000
	DefaultAnomalyLimit    = 1000
	DefaultTSMaxWords      = 6
	DefaultTSMaxLines      = 100
	DefaultTSLockThreshold = 10
	DefaultNewlineSymbol   = "\n"
)

// Config holds all RX_* configuration values loaded from the environment.
// All fields are exported and read-only after Load() returns.
type Config struct {
	// Core search engine
	MaxLineSizeKB   int // RX_MAX_LINE_SIZE_KB — max assumed average line length in KB
	MaxSubprocesses int // RX_MAX_SUBPROCESSES — max parallel rg subprocesses
	MinChunkSizeMB  int // RX_MIN_CHUNK_SIZE_MB — minimum chunk size for file splitting
	MaxFiles        int // RX_MAX_FILES — max files when traversing directories
	Debug           bool // RX_DEBUG — enable debug logging and rg command echo
	LargeFileMB     int // RX_LARGE_FILE_MB — threshold for "large file" treatment

	// Cache control
	CacheDir string // resolved cache directory (RX_CACHE_DIR > XDG_CACHE_HOME/rx > ~/.cache/rx)
	NoCache  bool   // RX_NO_CACHE — disable trace cache (read AND write)
	NoIndex  bool   // RX_NO_INDEX — disable line-offset index usage

	// Search roots (for serve mode)
	SearchRoots []string // RX_SEARCH_ROOT / RX_SEARCH_ROOTS, os.PathSeparator-separated

	// Webhooks
	HookOnFileURL       string // RX_HOOK_ON_FILE_URL
	HookOnMatchURL      string // RX_HOOK_ON_MATCH_URL
	HookOnCompleteURL   string // RX_HOOK_ON_COMPLETE_URL
	DisableCustomHooks  bool   // RX_DISABLE_CUSTOM_HOOKS

	// Frontend manager
	FrontendVersion string // RX_FRONTEND_VERSION
	FrontendURL     string // RX_FRONTEND_URL
	FrontendPath    string // RX_FRONTEND_PATH

	// Logging
	LogLevel string // RX_LOG_LEVEL — DEBUG/INFO/WARNING/ERROR

	// Analysis / anomaly detection
	SampleSizeLines     int // RX_SAMPLE_SIZE_LINES
	AnomalyLineLimit    int // RX_ANOMALY_LINE_LIMIT
	TimestampMaxWords   int // RX_TIMESTAMP_MAX_WORDS
	TimestampMaxLines   int // RX_TIMESTAMP_MAX_LINES_BETWEEN
	TimestampLockThresh int // RX_TIMESTAMP_FORMAT_LOCK_THRESHOLD

	// Display
	NewlineSymbol string // NEWLINE_SYMBOL (with \n, \r escape processing)

	// Go-specific
	FrameBatchSizeMB int // RX_FRAME_BATCH_SIZE_MB — target decompressed size per seekable zstd batch
}

// Load reads all RX_* environment variables and returns a fully populated Config.
// It never returns an error — invalid or missing values fall back to defaults.
func Load() Config {
	c := Config{
		MaxLineSizeKB:       parseIntEnv("RX_MAX_LINE_SIZE_KB", DefaultMaxLineSizeKB),
		MaxSubprocesses:     parseIntEnv("RX_MAX_SUBPROCESSES", DefaultMaxSubprocesses),
		MinChunkSizeMB:      parseIntEnv("RX_MIN_CHUNK_SIZE_MB", DefaultMinChunkSizeMB),
		MaxFiles:            parseIntEnv("RX_MAX_FILES", DefaultMaxFiles),
		Debug:               parseBoolEnv("RX_DEBUG"),
		LargeFileMB:         parseIntEnv("RX_LARGE_FILE_MB", DefaultLargeFileMB),
		NoCache:             parseBoolEnv("RX_NO_CACHE"),
		NoIndex:             parseBoolEnv("RX_NO_INDEX"),
		HookOnFileURL:       parseStringEnv("RX_HOOK_ON_FILE_URL", ""),
		HookOnMatchURL:      parseStringEnv("RX_HOOK_ON_MATCH_URL", ""),
		HookOnCompleteURL:   parseStringEnv("RX_HOOK_ON_COMPLETE_URL", ""),
		DisableCustomHooks:  parseBoolEnv("RX_DISABLE_CUSTOM_HOOKS"),
		FrontendVersion:     parseStringEnv("RX_FRONTEND_VERSION", ""),
		FrontendURL:         parseStringEnv("RX_FRONTEND_URL", ""),
		FrontendPath:        parseStringEnv("RX_FRONTEND_PATH", ""),
		LogLevel:            parseStringEnv("RX_LOG_LEVEL", DefaultLogLevel),
		SampleSizeLines:     parseIntEnv("RX_SAMPLE_SIZE_LINES", DefaultSampleSizeLines),
		AnomalyLineLimit:    parseIntEnv("RX_ANOMALY_LINE_LIMIT", DefaultAnomalyLimit),
		TimestampMaxWords:   parseIntEnv("RX_TIMESTAMP_MAX_WORDS", DefaultTSMaxWords),
		TimestampMaxLines:   parseIntEnv("RX_TIMESTAMP_MAX_LINES_BETWEEN", DefaultTSMaxLines),
		TimestampLockThresh: parseIntEnv("RX_TIMESTAMP_FORMAT_LOCK_THRESHOLD", DefaultTSLockThreshold),
		NewlineSymbol:       resolveNewlineSymbol(),
		FrameBatchSizeMB:    parseIntEnv("RX_FRAME_BATCH_SIZE_MB", DefaultFrameBatchMB),
	}

	c.CacheDir = resolveCacheDir()
	c.SearchRoots = resolveSearchRoots()

	return c
}

// resolveCacheDir implements the three-level cache directory resolution chain:
//
//  1. RX_CACHE_DIR (highest priority, used as-is)
//  2. XDG_CACHE_HOME/rx
//  3. ~/.cache/rx (fallback)
func resolveCacheDir() string {
	if v := os.Getenv("RX_CACHE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "rx")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Last resort — relative path (should not happen in practice).
		return filepath.Join(".cache", "rx")
	}
	return filepath.Join(home, ".cache", "rx")
}

// resolveSearchRoots parses RX_SEARCH_ROOT and RX_SEARCH_ROOTS into a deduplicated
// slice of directory paths. RX_SEARCH_ROOTS is split on os.PathListSeparator (`:` on
// Unix, `;` on Windows). Both variables can coexist — their values are merged.
func resolveSearchRoots() []string {
	seen := make(map[string]struct{})
	var roots []string

	addRoot := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, exists := seen[s]; !exists {
			seen[s] = struct{}{}
			roots = append(roots, s)
		}
	}

	// Single root variable.
	if v := os.Getenv("RX_SEARCH_ROOT"); v != "" {
		addRoot(v)
	}

	// Multi-root variable — split on os.PathListSeparator.
	if v := os.Getenv("RX_SEARCH_ROOTS"); v != "" {
		for _, part := range strings.Split(v, string(os.PathListSeparator)) {
			addRoot(part)
		}
	}

	return roots
}

// resolveNewlineSymbol reads NEWLINE_SYMBOL and processes \n and \r escape sequences.
func resolveNewlineSymbol() string {
	raw := parseStringEnv("NEWLINE_SYMBOL", DefaultNewlineSymbol)
	// Process escape sequences so the user can write literal "\n" in the env var.
	raw = strings.ReplaceAll(raw, `\n`, "\n")
	raw = strings.ReplaceAll(raw, `\r`, "\r")
	return raw
}

// --- helper functions -------------------------------------------------------

// parseBoolEnv returns true when the environment variable is set to
// one of "1", "true", or "yes" (case-insensitive). All other values — including
// the empty string and unset — return false.
func parseBoolEnv(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}

// parseIntEnv reads an integer environment variable. If the variable is unset,
// empty, or contains a non-integer value, the provided default is returned.
func parseIntEnv(key string, defaultVal int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return defaultVal
	}
	return n
}

// parseStringEnv returns the environment variable value, or defaultVal if unset/empty.
func parseStringEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
