package config

// Default values for the runtime tunables that control search
// performance and file-size thresholds.
//
// These mirror Python's constants at the time of the migration freeze.
// They are the values used when no environment override is set.
const (
	// DefaultMaxLineSizeKB is the assumed largest line length the engine
	// tolerates without splitting. Used by the chunker to decide how
	// much overlap to keep at chunk boundaries.
	DefaultMaxLineSizeKB = 8

	// DefaultMaxSubprocesses is the upper bound on concurrent rg workers.
	// In the Go port this becomes the goroutine-limit for errgroup.
	DefaultMaxSubprocesses = 20

	// DefaultMinChunkSizeMB is the smallest chunk we'll carve out when
	// parallelising a single-file search.
	//
	// USER DECISION 6.9.6 (2026-04-18): keep at 20 MB for Python parity.
	// Rationale: Python tuned this to amortize subprocess-startup cost
	// for `dd`; the Go port drops `dd`, but the user prefers matching
	// behavior over a theoretical perf win so that rx-go output is
	// byte-compatible with Python output on the same inputs.
	DefaultMinChunkSizeMB = 20

	// DefaultMaxFiles caps how many files a single trace request will
	// scan before giving up. Protects against runaway directory scans.
	DefaultMaxFiles = 1000

	// DefaultLargeFileMB is the size at which a file triggers unified
	// index creation. Files smaller than this are scanned in one shot
	// without any index or cache.
	DefaultLargeFileMB = 50
)

// Getters that read the current environment every call. We deliberately
// don't memoise: tests use t.Setenv to swap values per-test, and these
// are only called during request setup (not in hot loops), so the
// cost of an os.Getenv is negligible.

// MaxLineSizeKB returns RX_MAX_LINE_SIZE_KB or DefaultMaxLineSizeKB.
func MaxLineSizeKB() int {
	return GetIntEnv("RX_MAX_LINE_SIZE_KB", DefaultMaxLineSizeKB)
}

// MaxSubprocesses returns RX_MAX_SUBPROCESSES or DefaultMaxSubprocesses.
// Despite the historical name, in the Go port this caps *goroutines*.
func MaxSubprocesses() int {
	return GetIntEnv("RX_MAX_SUBPROCESSES", DefaultMaxSubprocesses)
}

// MinChunkSizeMB returns RX_MIN_CHUNK_SIZE_MB or DefaultMinChunkSizeMB.
func MinChunkSizeMB() int {
	return GetIntEnv("RX_MIN_CHUNK_SIZE_MB", DefaultMinChunkSizeMB)
}

// MaxFiles returns RX_MAX_FILES or DefaultMaxFiles.
func MaxFiles() int {
	return GetIntEnv("RX_MAX_FILES", DefaultMaxFiles)
}

// LargeFileMB returns RX_LARGE_FILE_MB or DefaultLargeFileMB.
func LargeFileMB() int {
	return GetIntEnv("RX_LARGE_FILE_MB", DefaultLargeFileMB)
}
