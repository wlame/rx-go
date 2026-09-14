// Package prometheus registers the rx_* metrics reported by /metrics
// and exposes typed helpers so business logic doesn't deal with the
// prometheus/client_golang API directly.
//
// Design notes:
//   - We use a private *prometheus.Registry instead of the global
//     DefaultRegisterer. Tests create multiple registries in parallel,
//     and the Go runtime panics on duplicate metric registration at the
//     default registry, so an isolated registry is strictly safer.
//   - The set of metrics mirrors rx-python/src/rx/prometheus.py MINUS
//     the rx_complexity_* family, because the regex-complexity feature
//     is not implemented in rx-go.
//   - Helpers take basic types (strings, ints, time.Durations) so
//     callers don't import this package's internals. Any non-existent
//     label value is allowed; promauto handles it.
package prometheus

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// enabled gates every Record* / Inc / Observe helper in this package.
// Zero-value (false) means "noop" — metric updates return immediately.
// The `serve` command calls Enable() once on startup to flip it true;
// CLI invocations never enable and therefore pay no collection cost.
//
// An atomic bool is the cheapest possible gate —
// on amd64 / arm64 it's a single mov instruction, well below the 1ns
// mark. The helpers below early-return on the atomic check; the
// Prometheus client library's own overhead (WithLabelValues lookup,
// metric-family map access) is skipped entirely.
var enabled atomic.Bool

// Enable turns on metric collection. Called by webapi.Server.Start
// when launching the HTTP server. Safe to call more than once.
func Enable() { enabled.Store(true) }

// Disable turns off metric collection. Primarily useful for tests
// that want to isolate the "metrics off" path. Safe to call more
// than once.
func Disable() { enabled.Store(false) }

// IsEnabled reports the current state. Exposed for tests.
func IsEnabled() bool { return enabled.Load() }

// Registry is the private registry for all rx_* metrics, plus the Go
// runtime (go_*) and process (process_*) families. Exposed so tests can
// pass it to promhttp.HandlerFor, and so downstream code can register
// their own collectors if ever needed.
var Registry = newRegistry()

// newRegistry returns a registry holding the standard Go runtime and
// process collectors. Both read their values when /metrics is scraped
// and do nothing in between, so the CLI, which never serves /metrics,
// pays nothing for them.
func newRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return r
}

// factory is promauto wired to Registry so every Counter/Histogram/Gauge
// declaration below auto-registers without littering init(). The
// inventoryRegisterer between them records each family's name for
// FamilyNames.
var factory = promauto.With(inventoryRegisterer{Registry})

// ============================================================================
// Request-level metrics
// ============================================================================

var (
	// TraceRequestsTotal — counter per trace request; labels: status (success, error).
	TraceRequestsTotal = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rx_trace_requests_total",
			Help: "Total number of trace (search) requests",
		},
		[]string{"status"},
	)

	// SamplesRequestsTotal — counter per samples request.
	SamplesRequestsTotal = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rx_samples_requests_total",
			Help: "Total number of samples (context) requests",
		},
		[]string{"status"},
	)

	// AnalyzeRequestsTotal — counter per analyze request (indexing with --analyze).
	AnalyzeRequestsTotal = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rx_analyze_requests_total",
			Help: "Total number of analyze requests",
		},
		[]string{"status"},
	)

	// TraceDurationSeconds — histogram of trace durations.
	TraceDurationSeconds = factory.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rx_trace_duration_seconds",
			Help:    "Time spent serving trace requests",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"path_kind"}, // "regular" | "compressed" | "seekable"
	)

	// SamplesDurationSeconds — histogram of samples durations.
	SamplesDurationSeconds = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_samples_duration_seconds",
			Help:    "Time spent serving samples requests",
			Buckets: prometheus.DefBuckets,
		},
	)
)

// ============================================================================
// File & byte metrics
// ============================================================================

// File-scope counters incremented by the search engine as work proceeds.
var (
	// FilesProcessedTotal counts the files a trace scanned or answered
	// from its cache. A file that failed part-way is counted here and in
	// FilesSkippedTotal.
	FilesProcessedTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_files_processed_total",
			Help: "Total number of files processed across all requests",
		},
	)

	// FilesSkippedTotal counts the files a trace answer lists as skipped:
	// binary, unreadable, or failed during the scan.
	FilesSkippedTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_files_skipped_total",
			Help: "Total number of files skipped (binary, inaccessible, etc.)",
		},
	)

	// BytesProcessedTotal adds the on-disk size of every file
	// FilesProcessedTotal counts, as rx-python does.
	BytesProcessedTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_bytes_processed_total",
			Help: "Total bytes read across all files",
		},
	)

	// MatchesFoundTotal counts matches returned from the engine
	// (post-dedup, post-max-results truncation).
	MatchesFoundTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_matches_found_total",
			Help: "Total matches returned by the search engine",
		},
	)
)

// ============================================================================
// Cache metrics
// ============================================================================

// Cache observability — one hit/miss pair per cache kind, each updated
// through its gated Inc* helper.
var (
	// IndexCacheHitsTotal increments when a lookup finds a valid index
	// on disk.
	IndexCacheHitsTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_index_cache_hits_total",
			Help: "Unified-index cache hits",
		},
	)
	// IndexCacheMissesTotal increments when a lookup finds no index, or
	// one that no longer describes its file.
	IndexCacheMissesTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_index_cache_misses_total",
			Help: "Unified-index cache misses",
		},
	)
	// TraceCacheHitsTotal increments when a trace cache file is reused.
	TraceCacheHitsTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_trace_cache_hits_total",
			Help: "Trace cache hits",
		},
	)
	// TraceCacheMissesTotal increments when a trace is re-run from source.
	TraceCacheMissesTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_trace_cache_misses_total",
			Help: "Trace cache misses",
		},
	)
	// TraceCacheWritesTotal increments when a trace result is persisted.
	TraceCacheWritesTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_trace_cache_writes_total",
			Help: "Trace cache writes",
		},
	)

	// IndexBuildDurationSeconds tracks how long it takes to build a
	// unified line-offset index. Python parity:
	// rx-python/src/rx/cli/prometheus.py::index_build_duration_seconds.
	// Buckets cover the expected range from 1 ms (tiny files) to ~5 min
	// (multi-GB logs on slow storage).
	IndexBuildDurationSeconds = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_index_build_duration_seconds",
			Help:    "Time spent building a unified line-offset index",
			Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 300},
		},
	)
)

// ============================================================================
// HTTP metrics
// ============================================================================

var (
	// HTTPResponsesTotal labels responses by HTTP method, endpoint template
	// (NOT raw URL), and status code. Chi's pattern-match name is used as
	// the endpoint label to keep cardinality bounded.
	HTTPResponsesTotal = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rx_http_responses_total",
			Help: "HTTP responses by status code",
		},
		[]string{"method", "endpoint", "status_code"},
	)
)

// ============================================================================
// Task / worker metrics
// ============================================================================

// Task / worker observability. ActiveWorkers should be Inc'd on goroutine
// start and Dec'd in defer, giving a live gauge of concurrency.
var (
	// ActiveWorkers is the live count of worker goroutines.
	ActiveWorkers = factory.NewGauge(
		prometheus.GaugeOpts{
			Name: "rx_active_workers",
			Help: "Current number of active worker goroutines",
		},
	)
	// WorkerTasksCompleted counts chunk-level tasks that finished without error.
	WorkerTasksCompleted = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_worker_tasks_completed_total",
			Help: "Total worker tasks completed",
		},
	)
	// WorkerTasksFailed counts chunk-level tasks that errored out.
	WorkerTasksFailed = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_worker_tasks_failed_total",
			Help: "Total worker tasks that failed",
		},
	)
)

// ============================================================================
// Hooks metrics
// ============================================================================

var (
	// HookCallsTotal counts webhook POSTs with kind (on_file|on_match|on_complete)
	// and status (success|failure) labels. // rx-go dispatches webhooks fire-and-forget; a single failed HTTP
	// call bumps status="failure".
	HookCallsTotal = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rx_hook_calls_total",
			Help: "Total webhook calls, by hook kind and status",
		},
		[]string{"kind", "status"},
	)
	// HookCallDurationSeconds measures how long a webhook POST took,
	// labeled by event type as rx-python labels its equivalent.
	HookCallDurationSeconds = factory.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rx_hook_call_duration_seconds",
			Help:    "Time spent calling hooks (per event type)",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.0, 3.0, 5.0},
		},
		[]string{"kind"},
	)
)

// ============================================================================
// Metrics shared with rx-python
// ============================================================================
//
// These families exist in rx-python's /metrics output too, with the same
// names, labels and bucket boundaries, so a dashboard works against
// either backend. They are emitted only when Enable() has been called
// (serve mode); CLI mode skips them via the helper gate below.
var (
	// ErrorsTotal: per-kind error counter (invalid_regex, file_not_found,
	// binary_file, permission_error, internal_error, etc.). Mirrors
	// rx-python/src/rx/prometheus.py::errors_total.
	ErrorsTotal = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rx_errors_total",
			Help: "Total errors by type",
		},
		[]string{"error_type"},
	)

	// FileSizeBytes: histogram of per-file size at scan time. The
	// buckets match Python's exact boundary list so dashboards port
	// over unchanged.
	FileSizeBytes = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name: "rx_file_size_bytes",
			Help: "Size of files being processed",
			Buckets: []float64{
				1024, 10_240, 102_400, 1_048_576, 10_485_760,
				104_857_600, 524_288_000, 1_073_741_824,
				5_368_709_120, 10_737_418_240,
				26_843_545_600, 53_687_091_200,
			},
		},
	)

	// PatternsPerRequest: histogram of pattern count per request.
	PatternsPerRequest = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_patterns_per_request",
			Help:    "Number of regex patterns in a single request",
			Buckets: []float64{1, 2, 3, 5, 10, 20, 50, 100},
		},
	)

	// MatchesPerRequest: histogram of matches returned per request.
	MatchesPerRequest = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_matches_per_request",
			Help:    "Number of matches found per request",
			Buckets: []float64{0, 1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 50000, 100000, 500000, 1000000},
		},
	)

	// ParallelTasksCreated: histogram of concurrent-task count per request.
	ParallelTasksCreated = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_parallel_tasks_created",
			Help:    "Number of parallel tasks created per request",
			Buckets: []float64{1, 2, 4, 8, 12, 16, 20, 24, 32, 48, 64, 100, 200},
		},
	)

	// MaxResultsLimitedTotal: counter of requests that hit the cap.
	MaxResultsLimitedTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_max_results_limited_total",
			Help: "Count of requests that hit max_results limit",
		},
	)

	// OffsetsPerSamplesRequest: histogram of offset count per samples call.
	OffsetsPerSamplesRequest = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_offsets_per_samples_request",
			Help:    "Number of offsets requested in samples endpoint",
			Buckets: []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000},
		},
	)

	// ContextLinesBefore / ContextLinesAfter: histograms of context.
	ContextLinesBefore = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_context_lines_before",
			Help:    "Number of context lines before each match",
			Buckets: []float64{0, 1, 2, 3, 5, 10, 20, 50, 100},
		},
	)
	ContextLinesAfter = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_context_lines_after",
			Help:    "Number of context lines after each match",
			Buckets: []float64{0, 1, 2, 3, 5, 10, 20, 50, 100},
		},
	)

	// IndexLoadDurationSeconds: histogram of cache-read time.
	IndexLoadDurationSeconds = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_index_load_duration_seconds",
			Help:    "Time to load index from disk",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0},
		},
	)

	// TraceCacheSkipTotal: counter of trace-cache skip conditions.
	TraceCacheSkipTotal = factory.NewCounter(
		prometheus.CounterOpts{
			Name: "rx_trace_cache_skip_total",
			Help: "Number of times trace cache was skipped (small file, max_results, etc.)",
		},
	)

	// TraceCacheLoadDurationSeconds: histogram of trace-cache read time.
	TraceCacheLoadDurationSeconds = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_trace_cache_load_duration_seconds",
			Help:    "Time to load trace cache from disk",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0},
		},
	)

	// TraceCacheReconstructionSeconds: histogram of cache-hit match
	// reconstruction time.
	TraceCacheReconstructionSeconds = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_trace_cache_reconstruction_seconds",
			Help:    "Time to reconstruct matches from trace cache",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1.0, 5.0, 10.0, 30.0},
		},
	)

	// AnalyzeRequestsDurationSeconds: how long an index build with
	// analysis took, from the task's start to the saved index. Same
	// name and buckets as rx-python's.
	AnalyzeRequestsDurationSeconds = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rx_analyze_duration_seconds",
			Help:    "Time spent processing file analysis requests",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0},
		},
	)
)

// ============================================================================
// Helpers — business code calls these; prometheus types never exposed.
// ============================================================================
//
// Every helper in this section early-returns when Enable() hasn't been
// called. The atomic.Bool.Load() is a single memory read — on amd64
// and arm64 it's <1ns and the Go compiler often optimizes it away
// entirely when the register is already hot. The savings come from
// NOT calling the prometheus/client_golang WithLabelValues hash lookup,
// NOT doing the strconv.Itoa on status codes, NOT incrementing the
// counter's internal atomic etc.
//
// CLI invocations of rx must not pay any metric collection cost —
// someone running `rx "error" file.log` from a shell script does not
// scrape metrics and should not pay for them.

// RecordHTTPResponse increments HTTPResponsesTotal for the given (method, path, status).
func RecordHTTPResponse(method, path string, status int) {
	if !enabled.Load() {
		return
	}
	HTTPResponsesTotal.
		WithLabelValues(method, path, strconv.Itoa(status)).
		Inc()
}

// RecordTraceRequest counts one /v1/trace request. status is "success"
// for a request that produced a response body and "error" for one
// rejected or failed, spelled as rx-python spells it so a dashboard
// ports between the backends unchanged.
func RecordTraceRequest(status string) {
	if !enabled.Load() {
		return
	}
	TraceRequestsTotal.WithLabelValues(status).Inc()
}

// RecordSamplesRequest counts one /v1/samples request.
func RecordSamplesRequest(status string) {
	if !enabled.Load() {
		return
	}
	SamplesRequestsTotal.WithLabelValues(status).Inc()
}

// RecordAnalyzeRequest counts one index-with-analysis request.
func RecordAnalyzeRequest(status string) {
	if !enabled.Load() {
		return
	}
	AnalyzeRequestsTotal.WithLabelValues(status).Inc()
}

// RecordTraceDuration observes a trace-request duration.
// pathKind is "regular", "compressed", or "seekable".
func RecordTraceDuration(pathKind string, dur time.Duration) {
	if !enabled.Load() {
		return
	}
	TraceDurationSeconds.
		WithLabelValues(pathKind).
		Observe(dur.Seconds())
}

// IncIndexCacheHits increments IndexCacheHitsTotal when enabled.
func IncIndexCacheHits() {
	if !enabled.Load() {
		return
	}
	IndexCacheHitsTotal.Inc()
}

// IncIndexCacheMisses increments IndexCacheMissesTotal when enabled.
func IncIndexCacheMisses() {
	if !enabled.Load() {
		return
	}
	IndexCacheMissesTotal.Inc()
}

// RecordHook bumps HookCallsTotal with (kind, status) labels.
func RecordHook(kind, status string) {
	if !enabled.Load() {
		return
	}
	HookCallsTotal.WithLabelValues(kind, status).Inc()
}

// RecordHookDuration adds a duration observation to the per-kind
// HookCallDurationSeconds histogram.
func RecordHookDuration(kind string, dur time.Duration) {
	if !enabled.Load() {
		return
	}
	HookCallDurationSeconds.WithLabelValues(kind).Observe(dur.Seconds())
}

// LargeFileThresholdMB publishes RX_LARGE_FILE_MB, the size from which
// a plain file's trace is cached and an index is built for it. A scrape
// cannot read the trace-cache and index families without it.
var LargeFileThresholdMB = factory.NewGauge(
	prometheus.GaugeOpts{
		Name: "rx_large_file_threshold_mb",
		Help: `Threshold in MB (RX_LARGE_FILE_MB) from which a file is "large": its trace is cached and it gets an index`,
	},
)

// RipgrepProcessingSeconds measures wall-clock time inside one ripgrep
// invocation, separating the subprocess cost from the rest of a request.
var RipgrepProcessingSeconds = factory.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "rx_ripgrep_processing_seconds",
		Help:    "Time spent in ripgrep processing",
		Buckets: prometheus.DefBuckets,
	},
)

// RecordConfig publishes the configuration gauges a scrape needs to
// interpret the rest of the exposition.
func RecordConfig(largeFileMB int) {
	if !enabled.Load() {
		return
	}
	LargeFileThresholdMB.Set(float64(largeFileMB))
}

// RecordRipgrepProcessing observes how long one ripgrep run took.
func RecordRipgrepProcessing(dur time.Duration) {
	if !enabled.Load() {
		return
	}
	RipgrepProcessingSeconds.Observe(dur.Seconds())
}

// RecordError increments ErrorsTotal with the given error_type label.
func RecordError(errorType string) {
	if !enabled.Load() {
		return
	}
	ErrorsTotal.WithLabelValues(errorType).Inc()
}

// RecordFileScanned counts one file a trace scanned or answered from
// its cache: FilesProcessedTotal by one, its size into FileSizeBytes,
// and the same size onto BytesProcessedTotal.
func RecordFileScanned(sizeBytes int64) {
	if !enabled.Load() {
		return
	}
	FilesProcessedTotal.Inc()
	FileSizeBytes.Observe(float64(sizeBytes))
	BytesProcessedTotal.Add(float64(sizeBytes))
}

// AddFilesSkipped adds the number of files one trace answer lists as
// skipped to FilesSkippedTotal.
func AddFilesSkipped(n int) {
	if !enabled.Load() || n <= 0 {
		return
	}
	FilesSkippedTotal.Add(float64(n))
}

// RecordPatternsPerRequest observes the regex pattern count on a
// request's PatternsPerRequest histogram (request-scoped, called once
// per completed request).
func RecordPatternsPerRequest(n int) {
	if !enabled.Load() {
		return
	}
	PatternsPerRequest.Observe(float64(n))
}

// RecordMatchesPerRequest observes the returned-match count on a
// request's MatchesPerRequest histogram (request-scoped, called once
// per completed request).
func RecordMatchesPerRequest(n int) {
	if !enabled.Load() {
		return
	}
	MatchesPerRequest.Observe(float64(n))
}

// RecordParallelTasks observes the parallel worker count used by a
// request (non-zero values only — zero means single-threaded path).
func RecordParallelTasks(n int) {
	if !enabled.Load() || n <= 0 {
		return
	}
	ParallelTasksCreated.Observe(float64(n))
}

// RecordMaxResultsLimited increments MaxResultsLimitedTotal.
func RecordMaxResultsLimited() {
	if !enabled.Load() {
		return
	}
	MaxResultsLimitedTotal.Inc()
}

// RecordSamplesDuration observes how long one successful samples
// request took on SamplesDurationSeconds.
func RecordSamplesDuration(dur time.Duration) {
	if !enabled.Load() {
		return
	}
	SamplesDurationSeconds.Observe(dur.Seconds())
}

// RecordAnalyzeDuration observes how long one index build with analysis
// took on AnalyzeRequestsDurationSeconds.
func RecordAnalyzeDuration(dur time.Duration) {
	if !enabled.Load() {
		return
	}
	AnalyzeRequestsDurationSeconds.Observe(dur.Seconds())
}

// RecordSamplesOffsets observes the offset count on the
// OffsetsPerSamplesRequest histogram (one call per samples request).
func RecordSamplesOffsets(n int) {
	if !enabled.Load() {
		return
	}
	OffsetsPerSamplesRequest.Observe(float64(n))
}

// RecordContextBefore observes the before-context line count on the
// ContextLinesBefore histogram.
func RecordContextBefore(n int) {
	if !enabled.Load() {
		return
	}
	ContextLinesBefore.Observe(float64(n))
}

// RecordContextAfter observes the after-context line count on the
// ContextLinesAfter histogram.
func RecordContextAfter(n int) {
	if !enabled.Load() {
		return
	}
	ContextLinesAfter.Observe(float64(n))
}

// RecordIndexLoadDuration observes the time taken to load an index
// from cache on the IndexLoadDurationSeconds histogram (Python parity).
func RecordIndexLoadDuration(dur time.Duration) {
	if !enabled.Load() {
		return
	}
	IndexLoadDurationSeconds.Observe(dur.Seconds())
}

// RecordTraceCacheLoadDuration observes trace-cache read time on the
// TraceCacheLoadDurationSeconds histogram.
func RecordTraceCacheLoadDuration(dur time.Duration) {
	if !enabled.Load() {
		return
	}
	TraceCacheLoadDurationSeconds.Observe(dur.Seconds())
}

// RecordTraceCacheReconstruction observes the time taken to rebuild
// match structures from a cache hit on the
// TraceCacheReconstructionSeconds histogram.
func RecordTraceCacheReconstruction(dur time.Duration) {
	if !enabled.Load() {
		return
	}
	TraceCacheReconstructionSeconds.Observe(dur.Seconds())
}

// RecordTraceCacheSkip increments TraceCacheSkipTotal when the trace
// cache was bypassed (small file, max_results cap, etc.).
func RecordTraceCacheSkip() {
	if !enabled.Load() {
		return
	}
	TraceCacheSkipTotal.Inc()
}

// ============================================================================
// Gated helpers for previously-direct-access counters.
// ============================================================================
//
// Call sites must not touch a counter directly: these wrappers are
// cheap no-ops in CLI mode, doing work only when Enable() has been
// called.

// IncActiveWorkers increments the ActiveWorkers gauge when enabled.
func IncActiveWorkers() {
	if !enabled.Load() {
		return
	}
	ActiveWorkers.Inc()
}

// DecActiveWorkers decrements the ActiveWorkers gauge when enabled.
func DecActiveWorkers() {
	if !enabled.Load() {
		return
	}
	ActiveWorkers.Dec()
}

// IncWorkerTasksCompleted increments the WorkerTasksCompleted counter.
func IncWorkerTasksCompleted() {
	if !enabled.Load() {
		return
	}
	WorkerTasksCompleted.Inc()
}

// IncWorkerTasksFailed increments the WorkerTasksFailed counter.
func IncWorkerTasksFailed() {
	if !enabled.Load() {
		return
	}
	WorkerTasksFailed.Inc()
}

// IncTraceCacheHits increments TraceCacheHitsTotal when enabled.
func IncTraceCacheHits() {
	if !enabled.Load() {
		return
	}
	TraceCacheHitsTotal.Inc()
}

// IncTraceCacheMisses increments TraceCacheMissesTotal when enabled.
func IncTraceCacheMisses() {
	if !enabled.Load() {
		return
	}
	TraceCacheMissesTotal.Inc()
}

// IncTraceCacheWrites increments TraceCacheWritesTotal when enabled.
func IncTraceCacheWrites() {
	if !enabled.Load() {
		return
	}
	TraceCacheWritesTotal.Inc()
}

// AddMatchesFound gates MatchesFoundTotal.Add.
func AddMatchesFound(n int) {
	if !enabled.Load() || n <= 0 {
		return
	}
	MatchesFoundTotal.Add(float64(n))
}

// ObserveIndexBuildDuration gates IndexBuildDurationSeconds.Observe.
func ObserveIndexBuildDuration(dur time.Duration) {
	if !enabled.Load() {
		return
	}
	IndexBuildDurationSeconds.Observe(dur.Seconds())
}

// Handler returns an http.Handler that exposes Registry in the
// prometheus text exposition format. Intended to be mounted at /metrics.
// Calling this handler returns the exposition even when Enable() has
// NOT been called — but the output will only show counters at zero
// / histograms with no observations. That matches Python's behavior.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		Registry: Registry,
	})
}
