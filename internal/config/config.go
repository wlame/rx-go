package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for the rx tool
type Config struct {
	// Cache & Storage
	CacheDir string

	// Logging & Debug
	LogLevel string
	Debug    bool

	// Search Configuration
	SearchRoots []string

	// File Processing Limits
	MaxFiles          int
	MaxWorkers        int // Derived from MaxSubprocesses
	MinChunkSizeBytes int64
	MaxLineSizeKB     int

	// Indexing
	LargeFileMB      int
	SampleSizeLines  int
	AnomalyLineLimit int

	// Cache Control
	NoCache bool
	NoIndex bool

	// Webhooks
	HookOnFile         string
	HookOnMatch        string
	HookOnComplete     string
	DisableCustomHooks bool

	// Timestamp Detector Config
	TimestampMaxWords           int
	TimestampGapMinSeconds      int
	TimestampFormatLockThreshold int
}

// LoadConfig loads configuration from environment variables with defaults
func LoadConfig() (*Config, error) {
	cfg := &Config{
		// Defaults
		CacheDir:                     getDefaultCacheDir(),
		LogLevel:                     getEnvOrDefault("RX_LOG_LEVEL", "INFO"),
		Debug:                        getEnvBool("RX_DEBUG", false),
		SearchRoots:                  getSearchRoots(),
		MaxFiles:                     getEnvInt("RX_MAX_FILES", 1000),
		MaxWorkers:                   getMaxWorkers(),
		MinChunkSizeBytes:            int64(getEnvInt("RX_MIN_CHUNK_SIZE_MB", 20)) * 1024 * 1024,
		MaxLineSizeKB:                getEnvInt("RX_MAX_LINE_SIZE_KB", 8),
		LargeFileMB:                  getEnvInt("RX_LARGE_FILE_MB", 50),
		SampleSizeLines:              getEnvInt("RX_SAMPLE_SIZE_LINES", 1000000),
		AnomalyLineLimit:             getEnvInt("RX_ANOMALY_LINE_LIMIT", 1000),
		NoCache:                      getEnvBool("RX_NO_CACHE", false),
		NoIndex:                      getEnvBool("RX_NO_INDEX", false),
		HookOnFile:                   os.Getenv("RX_HOOK_ON_FILE"),
		HookOnMatch:                  os.Getenv("RX_HOOK_ON_MATCH"),
		HookOnComplete:               os.Getenv("RX_HOOK_ON_COMPLETE"),
		DisableCustomHooks:           getEnvBool("RX_DISABLE_CUSTOM_HOOKS", false),
		TimestampMaxWords:            getEnvInt("RX_TIMESTAMP_MAX_WORDS", 20),
		TimestampGapMinSeconds:       getEnvInt("RX_TIMESTAMP_GAP_MIN_SECONDS", 60),
		TimestampFormatLockThreshold: getEnvInt("RX_TIMESTAMP_FORMAT_LOCK_THRESHOLD", 10),
	}

	// Override CacheDir if RX_CACHE_DIR is set
	if cacheDir := os.Getenv("RX_CACHE_DIR"); cacheDir != "" {
		cfg.CacheDir = cacheDir
	}

	return cfg, cfg.Validate()
}

// Validate checks configuration for consistency
func (c *Config) Validate() error {
	if c.MaxFiles < 1 {
		return fmt.Errorf("RX_MAX_FILES must be >= 1, got %d", c.MaxFiles)
	}

	if c.MaxWorkers < 1 {
		return fmt.Errorf("max workers must be >= 1, got %d", c.MaxWorkers)
	}

	if c.MinChunkSizeBytes < 1024*1024 {
		return fmt.Errorf("RX_MIN_CHUNK_SIZE_MB must be >= 1, got %d", c.MinChunkSizeBytes/(1024*1024))
	}

	if c.LargeFileMB < 1 {
		return fmt.Errorf("RX_LARGE_FILE_MB must be >= 1, got %d", c.LargeFileMB)
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(c.CacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory %s: %w", c.CacheDir, err)
	}

	return nil
}

// GetIndexCacheDir returns the index cache directory
func (c *Config) GetIndexCacheDir() string {
	return filepath.Join(c.CacheDir, "indexes")
}

// GetTraceCacheDir returns the trace cache directory
func (c *Config) GetTraceCacheDir() string {
	return filepath.Join(c.CacheDir, "trace_cache")
}

// LargeFileThresholdBytes returns the large file threshold in bytes
func (c *Config) LargeFileThresholdBytes() int64 {
	return int64(c.LargeFileMB) * 1024 * 1024
}

// Helper functions

func getDefaultCacheDir() string {
	// Priority: RX_CACHE_DIR > XDG_CACHE_HOME/rx > ~/.cache/rx
	if cacheDir := os.Getenv("RX_CACHE_DIR"); cacheDir != "" {
		return cacheDir
	}

	if xdgCache := os.Getenv("XDG_CACHE_HOME"); xdgCache != "" {
		return filepath.Join(xdgCache, "rx")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("/tmp", "rx-cache")
	}

	return filepath.Join(homeDir, ".cache", "rx")
}

func getSearchRoots() []string {
	// Priority: RX_SEARCH_ROOTS (multi) > RX_SEARCH_ROOT (single) > no restriction
	// Empty RX_SEARCH_ROOTS means "allow any path" (no restrictions)
	if roots, exists := os.LookupEnv("RX_SEARCH_ROOTS"); exists {
		if roots == "" {
			return []string{} // Empty = allow any path
		}
		// Split by : on Unix, ; on Windows
		separator := ":"
		if runtime.GOOS == "windows" {
			separator = ";"
		}
		return strings.Split(roots, separator)
	}

	if root := os.Getenv("RX_SEARCH_ROOT"); root != "" {
		return []string{root}
	}

	// Default: no restrictions (allow any path)
	return []string{}
}

func getMaxWorkers() int {
	// Read RX_MAX_SUBPROCESSES but implement differently in Go
	// Use min(RX_MAX_SUBPROCESSES, runtime.NumCPU())
	maxSubprocesses := getEnvInt("RX_MAX_SUBPROCESSES", 20)

	cpuCount := runtime.NumCPU()

	// If configured value is reasonable, use it
	if maxSubprocesses > 0 && maxSubprocesses <= cpuCount*2 {
		return maxSubprocesses
	}

	// Otherwise, use CPU count capped at 20
	if cpuCount > 20 {
		return 20
	}

	return cpuCount
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			return parsed
		}
		// Also accept "1" and "0"
		if value == "1" {
			return true
		}
		if value == "0" {
			return false
		}
	}
	return defaultValue
}
