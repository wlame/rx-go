package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setenv is a helper that sets an env var and registers cleanup.
func setenv(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv(key, value)
}

// clearAllRXEnv removes every RX_* and related env var so tests start from a clean slate.
func clearAllRXEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"RX_MAX_LINE_SIZE_KB", "RX_MAX_SUBPROCESSES", "RX_MIN_CHUNK_SIZE_MB",
		"RX_MAX_FILES", "RX_DEBUG", "RX_LARGE_FILE_MB",
		"RX_CACHE_DIR", "XDG_CACHE_HOME", "RX_NO_CACHE", "RX_NO_INDEX",
		"RX_SEARCH_ROOT", "RX_SEARCH_ROOTS",
		"RX_HOOK_ON_FILE_URL", "RX_HOOK_ON_MATCH_URL", "RX_HOOK_ON_COMPLETE_URL",
		"RX_DISABLE_CUSTOM_HOOKS",
		"RX_FRONTEND_VERSION", "RX_FRONTEND_URL", "RX_FRONTEND_PATH",
		"RX_LOG_LEVEL",
		"RX_SAMPLE_SIZE_LINES", "RX_ANOMALY_LINE_LIMIT",
		"RX_TIMESTAMP_MAX_WORDS", "RX_TIMESTAMP_MAX_LINES_BETWEEN",
		"RX_TIMESTAMP_FORMAT_LOCK_THRESHOLD",
		"NEWLINE_SYMBOL",
		"RX_FRAME_BATCH_SIZE_MB",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// --- defaults ---------------------------------------------------------------

func TestLoad_defaults(t *testing.T) {
	clearAllRXEnv(t)

	cfg := Load()

	assert.Equal(t, DefaultMaxLineSizeKB, cfg.MaxLineSizeKB)
	assert.Equal(t, DefaultMaxSubprocesses, cfg.MaxSubprocesses)
	assert.Equal(t, DefaultMinChunkSizeMB, cfg.MinChunkSizeMB)
	assert.Equal(t, DefaultMaxFiles, cfg.MaxFiles)
	assert.False(t, cfg.Debug)
	assert.Equal(t, DefaultLargeFileMB, cfg.LargeFileMB)
	assert.False(t, cfg.NoCache)
	assert.False(t, cfg.NoIndex)
	assert.Empty(t, cfg.HookOnFileURL)
	assert.Empty(t, cfg.HookOnMatchURL)
	assert.Empty(t, cfg.HookOnCompleteURL)
	assert.False(t, cfg.DisableCustomHooks)
	assert.Empty(t, cfg.FrontendVersion)
	assert.Empty(t, cfg.FrontendURL)
	assert.Empty(t, cfg.FrontendPath)
	assert.Equal(t, DefaultLogLevel, cfg.LogLevel)
	assert.Equal(t, DefaultSampleSizeLines, cfg.SampleSizeLines)
	assert.Equal(t, DefaultAnomalyLimit, cfg.AnomalyLineLimit)
	assert.Equal(t, DefaultTSMaxWords, cfg.TimestampMaxWords)
	assert.Equal(t, DefaultTSMaxLines, cfg.TimestampMaxLines)
	assert.Equal(t, DefaultTSLockThreshold, cfg.TimestampLockThresh)
	assert.Equal(t, "\n", cfg.NewlineSymbol)
	assert.Equal(t, DefaultFrameBatchMB, cfg.FrameBatchSizeMB)
	assert.Empty(t, cfg.SearchRoots)
}

// --- int parsing ------------------------------------------------------------

func TestLoad_int_env_vars(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "RX_MAX_LINE_SIZE_KB", "16")
	setenv(t, "RX_MAX_SUBPROCESSES", "40")
	setenv(t, "RX_MIN_CHUNK_SIZE_MB", "50")
	setenv(t, "RX_MAX_FILES", "2000")
	setenv(t, "RX_LARGE_FILE_MB", "100")
	setenv(t, "RX_FRAME_BATCH_SIZE_MB", "64")

	cfg := Load()

	assert.Equal(t, 16, cfg.MaxLineSizeKB)
	assert.Equal(t, 40, cfg.MaxSubprocesses)
	assert.Equal(t, 50, cfg.MinChunkSizeMB)
	assert.Equal(t, 2000, cfg.MaxFiles)
	assert.Equal(t, 100, cfg.LargeFileMB)
	assert.Equal(t, 64, cfg.FrameBatchSizeMB)
}

func TestLoad_int_invalid_falls_back(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "RX_MAX_LINE_SIZE_KB", "not_a_number")
	setenv(t, "RX_MAX_SUBPROCESSES", "")

	cfg := Load()

	assert.Equal(t, DefaultMaxLineSizeKB, cfg.MaxLineSizeKB, "non-integer should fall back to default")
	assert.Equal(t, DefaultMaxSubprocesses, cfg.MaxSubprocesses, "empty should fall back to default")
}

// --- bool parsing -----------------------------------------------------------

func TestParseBoolEnv_true_values(t *testing.T) {
	for _, val := range []string{"1", "true", "yes", "TRUE", "Yes", "YES", "True"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("RX_TEST_BOOL", val)
			assert.True(t, parseBoolEnv("RX_TEST_BOOL"), "expected true for %q", val)
		})
	}
}

func TestParseBoolEnv_false_values(t *testing.T) {
	for _, val := range []string{"0", "false", "no", "FALSE", "No", "", "random", "2"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("RX_TEST_BOOL", val)
			assert.False(t, parseBoolEnv("RX_TEST_BOOL"), "expected false for %q", val)
		})
	}
}

func TestParseBoolEnv_unset(t *testing.T) {
	os.Unsetenv("RX_TEST_BOOL_UNSET")
	assert.False(t, parseBoolEnv("RX_TEST_BOOL_UNSET"))
}

func TestParseBoolEnv_whitespace(t *testing.T) {
	t.Setenv("RX_TEST_BOOL", "  true  ")
	assert.True(t, parseBoolEnv("RX_TEST_BOOL"), "whitespace around 'true' should still parse")
}

func TestLoad_bool_env_vars(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "RX_DEBUG", "yes")
	setenv(t, "RX_NO_CACHE", "1")
	setenv(t, "RX_NO_INDEX", "true")
	setenv(t, "RX_DISABLE_CUSTOM_HOOKS", "TRUE")

	cfg := Load()

	assert.True(t, cfg.Debug)
	assert.True(t, cfg.NoCache)
	assert.True(t, cfg.NoIndex)
	assert.True(t, cfg.DisableCustomHooks)
}

// --- string env vars --------------------------------------------------------

func TestLoad_string_env_vars(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "RX_HOOK_ON_FILE_URL", "http://example.com/file")
	setenv(t, "RX_HOOK_ON_MATCH_URL", "http://example.com/match")
	setenv(t, "RX_HOOK_ON_COMPLETE_URL", "http://example.com/complete")
	setenv(t, "RX_FRONTEND_VERSION", "v1.2.3")
	setenv(t, "RX_LOG_LEVEL", "DEBUG")

	cfg := Load()

	assert.Equal(t, "http://example.com/file", cfg.HookOnFileURL)
	assert.Equal(t, "http://example.com/match", cfg.HookOnMatchURL)
	assert.Equal(t, "http://example.com/complete", cfg.HookOnCompleteURL)
	assert.Equal(t, "v1.2.3", cfg.FrontendVersion)
	assert.Equal(t, "DEBUG", cfg.LogLevel)
}

// --- cache dir resolution chain ---------------------------------------------

func TestResolveCacheDir_RX_CACHE_DIR_wins(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "RX_CACHE_DIR", "/custom/cache")
	setenv(t, "XDG_CACHE_HOME", "/xdg/cache")

	cfg := Load()
	assert.Equal(t, "/custom/cache", cfg.CacheDir)
}

func TestResolveCacheDir_XDG_CACHE_HOME_fallback(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "XDG_CACHE_HOME", "/xdg/cache")

	cfg := Load()
	assert.Equal(t, filepath.Join("/xdg/cache", "rx"), cfg.CacheDir)
}

func TestResolveCacheDir_home_fallback(t *testing.T) {
	clearAllRXEnv(t)

	cfg := Load()

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".cache", "rx"), cfg.CacheDir)
}

// --- search roots -----------------------------------------------------------

func TestResolveSearchRoots_single(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "RX_SEARCH_ROOT", "/var/log")

	cfg := Load()
	assert.Equal(t, []string{"/var/log"}, cfg.SearchRoots)
}

func TestResolveSearchRoots_multiple(t *testing.T) {
	clearAllRXEnv(t)

	roots := strings.Join([]string{"/var/log", "/tmp/data"}, string(os.PathListSeparator))
	setenv(t, "RX_SEARCH_ROOTS", roots)

	cfg := Load()
	assert.Equal(t, []string{"/var/log", "/tmp/data"}, cfg.SearchRoots)
}

func TestResolveSearchRoots_combined_dedup(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "RX_SEARCH_ROOT", "/var/log")
	roots := strings.Join([]string{"/var/log", "/tmp/data"}, string(os.PathListSeparator))
	setenv(t, "RX_SEARCH_ROOTS", roots)

	cfg := Load()
	// /var/log appears once (deduped), /tmp/data is added.
	assert.Equal(t, []string{"/var/log", "/tmp/data"}, cfg.SearchRoots)
}

func TestResolveSearchRoots_empty_parts_skipped(t *testing.T) {
	clearAllRXEnv(t)

	// Separator-separated with empty parts (e.g. trailing colon).
	roots := "/var/log" + string(os.PathListSeparator) + "" + string(os.PathListSeparator) + "/tmp"
	setenv(t, "RX_SEARCH_ROOTS", roots)

	cfg := Load()
	assert.Equal(t, []string{"/var/log", "/tmp"}, cfg.SearchRoots)
}

func TestResolveSearchRoots_none_set(t *testing.T) {
	clearAllRXEnv(t)

	cfg := Load()
	assert.Empty(t, cfg.SearchRoots)
}

// --- newline symbol ---------------------------------------------------------

func TestNewlineSymbol_default(t *testing.T) {
	clearAllRXEnv(t)

	cfg := Load()
	assert.Equal(t, "\n", cfg.NewlineSymbol)
}

func TestNewlineSymbol_escape_processing(t *testing.T) {
	clearAllRXEnv(t)

	// The user writes the literal two-character string "\n" in the env var.
	setenv(t, "NEWLINE_SYMBOL", `\r\n`)

	cfg := Load()
	assert.Equal(t, "\r\n", cfg.NewlineSymbol)
}

func TestNewlineSymbol_custom(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "NEWLINE_SYMBOL", ">>")

	cfg := Load()
	assert.Equal(t, ">>", cfg.NewlineSymbol)
}

// --- analysis env vars ------------------------------------------------------

func TestLoad_analysis_env_vars(t *testing.T) {
	clearAllRXEnv(t)

	setenv(t, "RX_SAMPLE_SIZE_LINES", "500000")
	setenv(t, "RX_ANOMALY_LINE_LIMIT", "500")
	setenv(t, "RX_TIMESTAMP_MAX_WORDS", "10")
	setenv(t, "RX_TIMESTAMP_MAX_LINES_BETWEEN", "200")
	setenv(t, "RX_TIMESTAMP_FORMAT_LOCK_THRESHOLD", "20")

	cfg := Load()

	assert.Equal(t, 500000, cfg.SampleSizeLines)
	assert.Equal(t, 500, cfg.AnomalyLineLimit)
	assert.Equal(t, 10, cfg.TimestampMaxWords)
	assert.Equal(t, 200, cfg.TimestampMaxLines)
	assert.Equal(t, 20, cfg.TimestampLockThresh)
}
