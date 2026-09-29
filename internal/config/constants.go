package config

// Default values for the runtime tunables that control search
// performance and file-size thresholds.
//
// These mirror Python's constants at the time of the migration freeze.
// They are the values used when no environment override is set.
const (
	// DefaultMaxSubprocesses caps both the number of chunks one file is
	// split into and, unless RX_WORKERS is set, the number of rg
	// processes that run at once.
	DefaultMaxSubprocesses = 20

	// DefaultMinChunkSizeMB is the smallest chunk we'll carve out when
	// parallelising a single-file search.
	//
	// It is 20 MB because rx-python uses 20 MB: the same file is then
	// split into the same chunks by both backends, which keeps their
	// answers, file_chunks included, comparable on the same input.
	DefaultMinChunkSizeMB = 20

	// DefaultLargeFileMB is the size from which `rx index` indexes a
	// plain file, `rx samples` builds an index before a lookup, and a
	// trace answer for a plain file is cached. It also sets the index
	// checkpoint step (a fiftieth of it).
	DefaultLargeFileMB = 50
)

// Getters that read the current environment every call. We deliberately
// don't memoise: tests use t.Setenv to swap values per-test, and these
// are only called during request setup (not in hot loops), so the
// cost of an os.Getenv is negligible.

// MaxSubprocesses returns RX_MAX_SUBPROCESSES or DefaultMaxSubprocesses.
// Despite the historical name, in the Go port this caps *goroutines*.
func MaxSubprocesses() int {
	return GetIntEnv("RX_MAX_SUBPROCESSES", DefaultMaxSubprocesses)
}

// MinChunkSizeMB returns RX_MIN_CHUNK_SIZE_MB or DefaultMinChunkSizeMB.
func MinChunkSizeMB() int {
	return GetIntEnv("RX_MIN_CHUNK_SIZE_MB", DefaultMinChunkSizeMB)
}

// LargeFileMB returns RX_LARGE_FILE_MB or DefaultLargeFileMB.
func LargeFileMB() int {
	return GetIntEnv("RX_LARGE_FILE_MB", DefaultLargeFileMB)
}
