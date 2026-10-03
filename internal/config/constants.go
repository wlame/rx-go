package config

import "time"

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

	// DefaultAnalyzeWindowLines is the sliding-window size, in lines,
	// of the anomaly detectors when nothing else sets it. Large enough
	// for multi-line tracebacks and small JSON blobs, small enough that
	// per-worker memory is negligible.
	DefaultAnalyzeWindowLines = 128

	// MaxAnalyzeWindowLines is the largest window the detectors take:
	// the size of the fixed array each analyzer window holds.
	MaxAnalyzeWindowLines = 2048

	// DefaultTaskTTLMinutes is how long a finished background task is
	// kept before the sweeper removes it.
	DefaultTaskTTLMinutes = 60
)

// Getters that read the current environment every call, through the
// settings table in int_settings.go, which bounds every value. We
// deliberately don't memoise: tests use t.Setenv to swap values
// per-test, and these are only called during request setup (not in hot
// loops), so the cost of an os.Getenv is negligible.

// Workers returns RX_WORKERS, from 1 to 256, or 0 when it is not set
// (or not acceptable): the caller then picks the worker count itself.
func Workers() int {
	return WorkersSetting.Value()
}

// MaxSubprocesses returns RX_MAX_SUBPROCESSES, from 1 to 256, or
// DefaultMaxSubprocesses. Despite the historical name, in the Go port
// this caps *goroutines*.
func MaxSubprocesses() int {
	return MaxSubprocessesSetting.Value()
}

// MinChunkSizeMB returns RX_MIN_CHUNK_SIZE_MB, from 1 MB to 1 TiB, or
// DefaultMinChunkSizeMB.
func MinChunkSizeMB() int {
	return MinChunkSizeMBSetting.Value()
}

// LargeFileMB returns RX_LARGE_FILE_MB, from 1 MB to 1 TiB, or
// DefaultLargeFileMB.
func LargeFileMB() int {
	return LargeFileMBSetting.Value()
}

// AnalyzeWindowLines returns RX_ANALYZE_WINDOW_LINES, from 1 to
// MaxAnalyzeWindowLines, or DefaultAnalyzeWindowLines.
func AnalyzeWindowLines() int {
	return AnalyzeWindowLinesSetting.Value()
}

// TaskTTL returns RX_TASK_TTL_MINUTES, from 1 minute to one week, or
// DefaultTaskTTLMinutes, as a duration.
func TaskTTL() time.Duration {
	return time.Duration(TaskTTLMinutesSetting.Value()) * time.Minute
}
