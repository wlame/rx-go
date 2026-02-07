package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test Group 1: Load and Defaults

func TestLoadConfig_DefaultValues(t *testing.T) {
	// Clear all RX_* env vars
	clearRxEnvVars(t)

	cfg, err := LoadConfig()

	require.NoError(t, err)
	assert.NotNil(t, cfg)

	// Check defaults
	assert.Equal(t, "INFO", cfg.LogLevel)
	assert.False(t, cfg.Debug)
	assert.Equal(t, 1000, cfg.MaxFiles)
	assert.Equal(t, 20*1024*1024, int(cfg.MinChunkSizeBytes))
	assert.Equal(t, 8, cfg.MaxLineSizeKB)
	assert.Equal(t, 50, cfg.LargeFileMB)
	assert.Equal(t, 1000000, cfg.SampleSizeLines)
	assert.Equal(t, 1000, cfg.AnomalyLineLimit)
	assert.False(t, cfg.NoCache)
	assert.False(t, cfg.NoIndex)
	assert.False(t, cfg.DisableCustomHooks)
	assert.Equal(t, 20, cfg.TimestampMaxWords)
	assert.Equal(t, 60, cfg.TimestampGapMinSeconds)
	assert.Equal(t, 10, cfg.TimestampFormatLockThreshold)

	// MaxWorkers should be reasonable (CPU count or 20, whichever is lower)
	assert.Greater(t, cfg.MaxWorkers, 0)
	assert.LessOrEqual(t, cfg.MaxWorkers, 20)
}

func TestLoadConfig_WithEnvironmentVariables(t *testing.T) {
	clearRxEnvVars(t)

	// Set various env vars
	os.Setenv("RX_LOG_LEVEL", "DEBUG")
	os.Setenv("RX_DEBUG", "1")
	os.Setenv("RX_MAX_FILES", "500")
	os.Setenv("RX_MIN_CHUNK_SIZE_MB", "10")
	os.Setenv("RX_MAX_LINE_SIZE_KB", "16")
	os.Setenv("RX_LARGE_FILE_MB", "100")
	os.Setenv("RX_SAMPLE_SIZE_LINES", "500000")
	os.Setenv("RX_NO_CACHE", "true")
	os.Setenv("RX_NO_INDEX", "1")
	defer clearRxEnvVars(t)

	cfg, err := LoadConfig()

	require.NoError(t, err)
	assert.Equal(t, "DEBUG", cfg.LogLevel)
	assert.True(t, cfg.Debug)
	assert.Equal(t, 500, cfg.MaxFiles)
	assert.Equal(t, 10*1024*1024, int(cfg.MinChunkSizeBytes))
	assert.Equal(t, 16, cfg.MaxLineSizeKB)
	assert.Equal(t, 100, cfg.LargeFileMB)
	assert.Equal(t, 500000, cfg.SampleSizeLines)
	assert.True(t, cfg.NoCache)
	assert.True(t, cfg.NoIndex)
}

func TestLoadConfig_WebhookEnvironmentVariables(t *testing.T) {
	clearRxEnvVars(t)

	os.Setenv("RX_HOOK_ON_FILE", "http://example.com/file")
	os.Setenv("RX_HOOK_ON_MATCH", "http://example.com/match")
	os.Setenv("RX_HOOK_ON_COMPLETE", "http://example.com/complete")
	os.Setenv("RX_DISABLE_CUSTOM_HOOKS", "1")
	defer clearRxEnvVars(t)

	cfg, err := LoadConfig()

	require.NoError(t, err)
	assert.Equal(t, "http://example.com/file", cfg.HookOnFile)
	assert.Equal(t, "http://example.com/match", cfg.HookOnMatch)
	assert.Equal(t, "http://example.com/complete", cfg.HookOnComplete)
	assert.True(t, cfg.DisableCustomHooks)
}

func TestLoadConfig_TimestampDetectorConfig(t *testing.T) {
	clearRxEnvVars(t)

	os.Setenv("RX_TIMESTAMP_MAX_WORDS", "30")
	os.Setenv("RX_TIMESTAMP_GAP_MIN_SECONDS", "120")
	os.Setenv("RX_TIMESTAMP_FORMAT_LOCK_THRESHOLD", "5")
	defer clearRxEnvVars(t)

	cfg, err := LoadConfig()

	require.NoError(t, err)
	assert.Equal(t, 30, cfg.TimestampMaxWords)
	assert.Equal(t, 120, cfg.TimestampGapMinSeconds)
	assert.Equal(t, 5, cfg.TimestampFormatLockThreshold)
}

// Test Group 2: Cache Directory

func TestConfig_GetIndexCacheDir_Default(t *testing.T) {
	clearRxEnvVars(t)

	cfg, err := LoadConfig()
	require.NoError(t, err)

	dir := cfg.GetIndexCacheDir()

	assert.Contains(t, dir, "indexes")
	// Should be under cache directory
	assert.Contains(t, dir, cfg.CacheDir)
}

func TestConfig_GetTraceCacheDir_Default(t *testing.T) {
	clearRxEnvVars(t)

	cfg, err := LoadConfig()
	require.NoError(t, err)

	dir := cfg.GetTraceCacheDir()

	assert.Contains(t, dir, "trace_cache")
	assert.Contains(t, dir, cfg.CacheDir)
}

func TestConfig_CacheDir_RX_CACHE_DIR_Priority(t *testing.T) {
	clearRxEnvVars(t)

	tmpDir := t.TempDir()
	os.Setenv("RX_CACHE_DIR", tmpDir)
	defer os.Unsetenv("RX_CACHE_DIR")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	// RX_CACHE_DIR should be used directly
	assert.Equal(t, tmpDir, cfg.CacheDir)
}

func TestConfig_CacheDir_XDG_CACHE_HOME(t *testing.T) {
	clearRxEnvVars(t)

	tmpDir := t.TempDir()
	os.Setenv("XDG_CACHE_HOME", tmpDir)
	defer os.Unsetenv("XDG_CACHE_HOME")

	// Make sure RX_CACHE_DIR is not set
	os.Unsetenv("RX_CACHE_DIR")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	// Should use XDG_CACHE_HOME/rx
	expected := filepath.Join(tmpDir, "rx")
	assert.Equal(t, expected, cfg.CacheDir)
}

func TestConfig_CacheDir_DefaultHomeCache(t *testing.T) {
	clearRxEnvVars(t)

	// Ensure both RX_CACHE_DIR and XDG_CACHE_HOME are not set
	os.Unsetenv("RX_CACHE_DIR")
	os.Unsetenv("XDG_CACHE_HOME")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	// Should use ~/.cache/rx or fallback
	assert.NotEmpty(t, cfg.CacheDir)
	// Should contain "rx"
	assert.Contains(t, cfg.CacheDir, "rx")
}

func TestConfig_GetIndexCacheDir_CustomRX_CACHE_DIR(t *testing.T) {
	clearRxEnvVars(t)

	tmpDir := t.TempDir()
	os.Setenv("RX_CACHE_DIR", tmpDir)
	defer os.Unsetenv("RX_CACHE_DIR")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	dir := cfg.GetIndexCacheDir()

	assert.Contains(t, dir, tmpDir)
	assert.Contains(t, dir, "indexes")
	assert.Equal(t, filepath.Join(tmpDir, "indexes"), dir)
}

func TestConfig_GetTraceCacheDir_CustomRX_CACHE_DIR(t *testing.T) {
	clearRxEnvVars(t)

	tmpDir := t.TempDir()
	os.Setenv("RX_CACHE_DIR", tmpDir)
	defer os.Unsetenv("RX_CACHE_DIR")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	dir := cfg.GetTraceCacheDir()

	assert.Contains(t, dir, tmpDir)
	assert.Contains(t, dir, "trace_cache")
	assert.Equal(t, filepath.Join(tmpDir, "trace_cache"), dir)
}

// Test Group 3: Environment Variable Parsing

func TestGetEnvInt_ValidValues(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected int
	}{
		{"positive", "42", 42},
		{"zero", "0", 0},
		{"large", "1000000", 1000000},
		{"negative", "-1", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("TEST_VAR", tt.envValue)
			defer os.Unsetenv("TEST_VAR")

			val := getEnvInt("TEST_VAR", 999)
			assert.Equal(t, tt.expected, val)
		})
	}
}

func TestGetEnvInt_InvalidValues(t *testing.T) {
	tests := []struct {
		name         string
		envValue     string
		defaultValue int
		expected     int
	}{
		{"invalid_string", "abc", 100, 100},
		{"empty", "", 200, 200},
		{"float", "3.14", 300, 300},
		{"special_chars", "!@#", 400, 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("TEST_VAR", tt.envValue)
			defer os.Unsetenv("TEST_VAR")

			val := getEnvInt("TEST_VAR", tt.defaultValue)
			assert.Equal(t, tt.expected, val)
		})
	}
}

func TestGetEnvInt_NotSet(t *testing.T) {
	os.Unsetenv("TEST_VAR")

	val := getEnvInt("TEST_VAR", 123)
	assert.Equal(t, 123, val)
}

func TestGetEnvBool_TruthyValues(t *testing.T) {
	tests := []struct {
		envValue string
		expected bool
	}{
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"1", true},
		{"false", false},
		{"False", false},
		{"FALSE", false},
		{"0", false},
		{"", false}, // Empty defaults to provided default
	}

	for _, tt := range tests {
		t.Run(tt.envValue, func(t *testing.T) {
			os.Setenv("TEST_BOOL", tt.envValue)
			defer os.Unsetenv("TEST_BOOL")

			val := getEnvBool("TEST_BOOL", false)

			if tt.envValue == "" {
				// Empty should return default
				assert.Equal(t, false, val)
			} else {
				assert.Equal(t, tt.expected, val)
			}
		})
	}
}

func TestGetEnvBool_InvalidValues(t *testing.T) {
	tests := []struct {
		name         string
		envValue     string
		defaultValue bool
		expected     bool
	}{
		{"invalid_string", "yes", true, true},
		{"invalid_string_false_default", "yes", false, false},
		{"number_2", "2", true, true},
		{"number_2_false_default", "2", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("TEST_BOOL", tt.envValue)
			defer os.Unsetenv("TEST_BOOL")

			val := getEnvBool("TEST_BOOL", tt.defaultValue)
			assert.Equal(t, tt.expected, val)
		})
	}
}

func TestGetEnvBool_NotSet(t *testing.T) {
	os.Unsetenv("TEST_BOOL")

	valTrue := getEnvBool("TEST_BOOL", true)
	assert.True(t, valTrue)

	valFalse := getEnvBool("TEST_BOOL", false)
	assert.False(t, valFalse)
}

func TestGetEnvOrDefault_WithValue(t *testing.T) {
	os.Setenv("TEST_STRING", "custom_value")
	defer os.Unsetenv("TEST_STRING")

	val := getEnvOrDefault("TEST_STRING", "default")
	assert.Equal(t, "custom_value", val)
}

func TestGetEnvOrDefault_Empty(t *testing.T) {
	os.Setenv("TEST_STRING", "")
	defer os.Unsetenv("TEST_STRING")

	val := getEnvOrDefault("TEST_STRING", "default")
	assert.Equal(t, "default", val)
}

func TestGetEnvOrDefault_NotSet(t *testing.T) {
	os.Unsetenv("TEST_STRING")

	val := getEnvOrDefault("TEST_STRING", "default")
	assert.Equal(t, "default", val)
}

// Test Group 4: Search Roots

func TestConfig_SearchRoots_Default(t *testing.T) {
	clearRxEnvVars(t)

	// Clear both env vars
	os.Unsetenv("RX_SEARCH_ROOTS")
	os.Unsetenv("RX_SEARCH_ROOT")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	// Default is empty (no restrictions)
	assert.Equal(t, []string{}, cfg.SearchRoots)
}

func TestConfig_SearchRoots_FromRX_SEARCH_ROOTS(t *testing.T) {
	clearRxEnvVars(t)

	separator := ":"
	if runtime.GOOS == "windows" {
		separator = ";"
	}

	os.Setenv("RX_SEARCH_ROOTS", "/var/log"+separator+"/tmp"+separator+"/home/user")
	defer os.Unsetenv("RX_SEARCH_ROOTS")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, 3, len(cfg.SearchRoots))
	assert.Contains(t, cfg.SearchRoots, "/var/log")
	assert.Contains(t, cfg.SearchRoots, "/tmp")
	assert.Contains(t, cfg.SearchRoots, "/home/user")
}

func TestConfig_SearchRoots_FromRX_SEARCH_ROOT_Single(t *testing.T) {
	clearRxEnvVars(t)

	os.Setenv("RX_SEARCH_ROOT", "/var/log")
	defer os.Unsetenv("RX_SEARCH_ROOT")

	// Make sure RX_SEARCH_ROOTS is not set
	os.Unsetenv("RX_SEARCH_ROOTS")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, 1, len(cfg.SearchRoots))
	assert.Equal(t, "/var/log", cfg.SearchRoots[0])
}

func TestConfig_SearchRoots_EmptyRX_SEARCH_ROOTS_MeansNoRestrictions(t *testing.T) {
	clearRxEnvVars(t)

	// Empty string explicitly set means "allow any path"
	os.Setenv("RX_SEARCH_ROOTS", "")
	defer os.Unsetenv("RX_SEARCH_ROOTS")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, []string{}, cfg.SearchRoots)
}

func TestConfig_SearchRoots_RX_SEARCH_ROOTS_TakesPriority(t *testing.T) {
	clearRxEnvVars(t)

	os.Setenv("RX_SEARCH_ROOTS", "/var/log")
	os.Setenv("RX_SEARCH_ROOT", "/should/be/ignored")
	defer func() {
		os.Unsetenv("RX_SEARCH_ROOTS")
		os.Unsetenv("RX_SEARCH_ROOT")
	}()

	cfg, err := LoadConfig()
	require.NoError(t, err)

	// Should use RX_SEARCH_ROOTS, not RX_SEARCH_ROOT
	assert.Equal(t, 1, len(cfg.SearchRoots))
	assert.Equal(t, "/var/log", cfg.SearchRoots[0])
}

// Test Group 5: Validation

func TestConfig_Validate_ValidConfig(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &Config{
		CacheDir:          tmpDir,
		MaxFiles:          1000,
		MaxWorkers:        10,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		LargeFileMB:       50,
	}

	err := cfg.Validate()
	assert.NoError(t, err)

	// Cache dir should be created
	_, err = os.Stat(tmpDir)
	assert.NoError(t, err)
}

func TestConfig_Validate_InvalidMaxFiles(t *testing.T) {
	cfg := &Config{
		CacheDir:          t.TempDir(),
		MaxFiles:          0, // Invalid
		MaxWorkers:        10,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		LargeFileMB:       50,
	}

	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "RX_MAX_FILES")
}

func TestConfig_Validate_InvalidMaxWorkers(t *testing.T) {
	cfg := &Config{
		CacheDir:          t.TempDir(),
		MaxFiles:          1000,
		MaxWorkers:        0, // Invalid
		MinChunkSizeBytes: 20 * 1024 * 1024,
		LargeFileMB:       50,
	}

	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "max workers")
}

func TestConfig_Validate_InvalidMinChunkSize(t *testing.T) {
	cfg := &Config{
		CacheDir:          t.TempDir(),
		MaxFiles:          1000,
		MaxWorkers:        10,
		MinChunkSizeBytes: 500 * 1024, // Less than 1MB
		LargeFileMB:       50,
	}

	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "RX_MIN_CHUNK_SIZE_MB")
}

func TestConfig_Validate_InvalidLargeFileMB(t *testing.T) {
	cfg := &Config{
		CacheDir:          t.TempDir(),
		MaxFiles:          1000,
		MaxWorkers:        10,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		LargeFileMB:       0, // Invalid
	}

	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "RX_LARGE_FILE_MB")
}

func TestConfig_Validate_CreatesNonexistentCacheDir(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "new_cache_dir")

	cfg := &Config{
		CacheDir:          cacheDir,
		MaxFiles:          1000,
		MaxWorkers:        10,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		LargeFileMB:       50,
	}

	// Directory should not exist yet
	_, err := os.Stat(cacheDir)
	assert.Error(t, err)

	// Validate should create it
	err = cfg.Validate()
	assert.NoError(t, err)

	// Directory should now exist
	stat, err := os.Stat(cacheDir)
	assert.NoError(t, err)
	assert.True(t, stat.IsDir())
}

// Test Group 6: Helper Functions

func TestGetMaxWorkers_Default(t *testing.T) {
	clearRxEnvVars(t)

	workers := getMaxWorkers()

	// Should be based on CPU count, capped at 20
	assert.Greater(t, workers, 0)
	assert.LessOrEqual(t, workers, 20)
}

func TestGetMaxWorkers_CustomValue(t *testing.T) {
	clearRxEnvVars(t)

	os.Setenv("RX_MAX_SUBPROCESSES", "5")
	defer os.Unsetenv("RX_MAX_SUBPROCESSES")

	workers := getMaxWorkers()

	assert.Equal(t, 5, workers)
}

func TestGetMaxWorkers_ExceedsCPUCount(t *testing.T) {
	clearRxEnvVars(t)

	// Set to very high value
	os.Setenv("RX_MAX_SUBPROCESSES", "1000")
	defer os.Unsetenv("RX_MAX_SUBPROCESSES")

	workers := getMaxWorkers()

	// Should be capped or use CPU count
	cpuCount := runtime.NumCPU()
	if cpuCount > 20 {
		assert.Equal(t, 20, workers)
	} else {
		assert.Equal(t, cpuCount, workers)
	}
}

func TestConfig_LargeFileThresholdBytes(t *testing.T) {
	cfg := &Config{LargeFileMB: 100}

	threshold := cfg.LargeFileThresholdBytes()

	assert.Equal(t, int64(100*1024*1024), threshold)
}

func TestConfig_LargeFileThresholdBytes_Default(t *testing.T) {
	clearRxEnvVars(t)

	cfg, err := LoadConfig()
	require.NoError(t, err)

	threshold := cfg.LargeFileThresholdBytes()

	// Default is 50MB
	assert.Equal(t, int64(50*1024*1024), threshold)
}

// Test Group 7: Complex Scenarios

func TestConfig_AllEnvironmentVariables(t *testing.T) {
	clearRxEnvVars(t)

	tmpDir := t.TempDir()

	// Use a MaxWorkers value that will work on any CPU count
	// Use 2 which is safe and reasonable
	maxWorkersValue := 2

	// Set every environment variable
	os.Setenv("RX_CACHE_DIR", tmpDir)
	os.Setenv("RX_LOG_LEVEL", "TRACE")
	os.Setenv("RX_DEBUG", "true")
	os.Setenv("RX_SEARCH_ROOTS", "/var/log:/tmp")
	os.Setenv("RX_MAX_FILES", "2000")
	os.Setenv("RX_MAX_SUBPROCESSES", "2")
	os.Setenv("RX_MIN_CHUNK_SIZE_MB", "30")
	os.Setenv("RX_MAX_LINE_SIZE_KB", "32")
	os.Setenv("RX_LARGE_FILE_MB", "200")
	os.Setenv("RX_SAMPLE_SIZE_LINES", "2000000")
	os.Setenv("RX_ANOMALY_LINE_LIMIT", "5000")
	os.Setenv("RX_NO_CACHE", "1")
	os.Setenv("RX_NO_INDEX", "1")
	os.Setenv("RX_HOOK_ON_FILE", "http://hook.file")
	os.Setenv("RX_HOOK_ON_MATCH", "http://hook.match")
	os.Setenv("RX_HOOK_ON_COMPLETE", "http://hook.complete")
	os.Setenv("RX_DISABLE_CUSTOM_HOOKS", "true")
	os.Setenv("RX_TIMESTAMP_MAX_WORDS", "50")
	os.Setenv("RX_TIMESTAMP_GAP_MIN_SECONDS", "300")
	os.Setenv("RX_TIMESTAMP_FORMAT_LOCK_THRESHOLD", "20")
	defer clearRxEnvVars(t)

	cfg, err := LoadConfig()

	require.NoError(t, err)
	assert.Equal(t, tmpDir, cfg.CacheDir)
	assert.Equal(t, "TRACE", cfg.LogLevel)
	assert.True(t, cfg.Debug)
	assert.Equal(t, 2, len(cfg.SearchRoots))
	assert.Equal(t, 2000, cfg.MaxFiles)
	assert.Equal(t, maxWorkersValue, cfg.MaxWorkers)
	assert.Equal(t, 30*1024*1024, int(cfg.MinChunkSizeBytes))
	assert.Equal(t, 32, cfg.MaxLineSizeKB)
	assert.Equal(t, 200, cfg.LargeFileMB)
	assert.Equal(t, 2000000, cfg.SampleSizeLines)
	assert.Equal(t, 5000, cfg.AnomalyLineLimit)
	assert.True(t, cfg.NoCache)
	assert.True(t, cfg.NoIndex)
	assert.Equal(t, "http://hook.file", cfg.HookOnFile)
	assert.Equal(t, "http://hook.match", cfg.HookOnMatch)
	assert.Equal(t, "http://hook.complete", cfg.HookOnComplete)
	assert.True(t, cfg.DisableCustomHooks)
	assert.Equal(t, 50, cfg.TimestampMaxWords)
	assert.Equal(t, 300, cfg.TimestampGapMinSeconds)
	assert.Equal(t, 20, cfg.TimestampFormatLockThreshold)
}

// Helper functions

func clearRxEnvVars(t *testing.T) {
	t.Helper()

	// List of all RX_* environment variables
	envVars := []string{
		"RX_CACHE_DIR",
		"RX_LOG_LEVEL",
		"RX_DEBUG",
		"RX_SEARCH_ROOTS",
		"RX_SEARCH_ROOT",
		"RX_MAX_FILES",
		"RX_MAX_SUBPROCESSES",
		"RX_MIN_CHUNK_SIZE_MB",
		"RX_MAX_LINE_SIZE_KB",
		"RX_LARGE_FILE_MB",
		"RX_SAMPLE_SIZE_LINES",
		"RX_ANOMALY_LINE_LIMIT",
		"RX_NO_CACHE",
		"RX_NO_INDEX",
		"RX_HOOK_ON_FILE",
		"RX_HOOK_ON_MATCH",
		"RX_HOOK_ON_COMPLETE",
		"RX_DISABLE_CUSTOM_HOOKS",
		"RX_TIMESTAMP_MAX_WORDS",
		"RX_TIMESTAMP_GAP_MIN_SECONDS",
		"RX_TIMESTAMP_FORMAT_LOCK_THRESHOLD",
		"XDG_CACHE_HOME",
	}

	for _, envVar := range envVars {
		os.Unsetenv(envVar)
	}
}
