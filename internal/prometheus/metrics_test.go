package prometheus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// findMetric returns the first metric family matching name, or nil if absent.
func findMetric(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	families, err := Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func TestRegistry_ContainsCoreMetrics(t *testing.T) {
	// Touch every metric so CounterVecs/HistogramVecs get at least one
	// observed label set (prometheus omits vecs with zero observations
	// from Gather()). Bare Counters/Gauges appear unconditionally.
	TraceRequestsTotal.WithLabelValues("ok").Inc()
	SamplesRequestsTotal.WithLabelValues("ok").Inc()
	AnalyzeRequestsTotal.WithLabelValues("ok").Inc()
	TraceDurationSeconds.WithLabelValues("regular").Observe(0)
	HTTPResponsesTotal.WithLabelValues("GET", "/v1/trace", "200").Inc()
	HookCallsTotal.WithLabelValues("on_file", "success").Inc()
	ActiveWorkers.Inc()
	FilesProcessedTotal.Inc()

	// These names must appear in the registry — they're visible via
	// /metrics and break monitoring dashboards if renamed.
	required := []string{
		"rx_trace_requests_total",
		"rx_samples_requests_total",
		"rx_http_responses_total",
		"rx_active_workers",
		"rx_files_processed_total",
	}
	for _, name := range required {
		if findMetric(t, name) == nil {
			t.Errorf("metric %q not registered", name)
		}
	}
}

func TestRegistry_DoesNotContainComplexityMetrics(t *testing.T) {
	// Regex complexity is excluded per user-instructions — double check
	// none of the dropped metric names leaked through.
	forbidden := []string{
		"rx_complexity_requests_total",
		"rx_complexity_duration_seconds",
		"rx_regex_complexity_score",
		"rx_regex_complexity_level_total",
	}
	for _, name := range forbidden {
		if findMetric(t, name) != nil {
			t.Errorf("metric %q should NOT be registered (complexity feature dropped)", name)
		}
	}
}

func TestRecordHTTPResponse(t *testing.T) {
	// Stage 9 Round 2 S6: the helpers only update counters when
	// Enable() has been called. Enable it here so this test keeps
	// measuring the actual increment; tests that verify the noop
	// branch are in TestGating_NoopWhenDisabled below.
	Enable()
	t.Cleanup(Disable)
	// Start from a known delta so parallel tests don't poison the count.
	HTTPResponsesTotal.WithLabelValues("POST", "/v1/compress", "200").Inc() // warm the label set
	before := getCounterValue(t, HTTPResponsesTotal.WithLabelValues("POST", "/v1/compress", "200"))
	RecordHTTPResponse("POST", "/v1/compress", 200)
	after := getCounterValue(t, HTTPResponsesTotal.WithLabelValues("POST", "/v1/compress", "200"))
	if after != before+1 {
		t.Errorf("HTTPResponsesTotal: expected delta 1, got %v → %v", before, after)
	}
}

// TestGating_NoopWhenDisabled covers Stage 9 Round 2 S6: with
// Enable() never called (or after Disable()), the Record* / Inc / Obs
// helpers must skip work — counters remain at the same value after
// every helper call.
func TestGating_NoopWhenDisabled(t *testing.T) {
	Disable()
	t.Cleanup(Disable)
	if IsEnabled() {
		t.Fatal("Disable() did not clear the enabled flag")
	}
	// Snapshot counters BEFORE all the helper calls.
	beforeHTTP := getCounterValue(t, HTTPResponsesTotal.WithLabelValues("GET", "/gate-test", "200"))
	beforeHook := getCounterValue(t, HookCallsTotal.WithLabelValues("on_file", "success"))
	beforeHits := getCounterValue(t, TraceCacheHitsTotal)
	beforeWrites := getCounterValue(t, TraceCacheWritesTotal)

	// Exercise every gated helper. None should observe a change.
	RecordHTTPResponse("GET", "/gate-test", 200)
	RecordCacheHit("trace")
	RecordCacheMiss("trace")
	RecordHook("on_file", "success")
	RecordHookDuration("on_file", time.Second)
	RecordError("test_error")
	RecordFileSize(1024)
	RecordPatternsPerRequest(3)
	RecordMatchesPerRequest(100)
	RecordParallelTasks(8)
	RecordMaxResultsLimited()
	RecordSamplesOffsets(5)
	RecordContextBefore(3)
	RecordContextAfter(3)
	RecordIndexLoadDuration(time.Second)
	RecordTraceCacheLoadDuration(time.Second)
	RecordTraceCacheReconstruction(time.Second)
	RecordTraceCacheSkip()
	IncActiveWorkers()
	DecActiveWorkers()
	IncWorkerTasksCompleted()
	IncWorkerTasksFailed()
	IncTraceCacheHits()
	IncTraceCacheMisses()
	IncTraceCacheWrites()
	AddMatchesFound(10)
	ObserveIndexBuildDuration(time.Second)

	// Verify no change.
	afterHTTP := getCounterValue(t, HTTPResponsesTotal.WithLabelValues("GET", "/gate-test", "200"))
	afterHook := getCounterValue(t, HookCallsTotal.WithLabelValues("on_file", "success"))
	afterHits := getCounterValue(t, TraceCacheHitsTotal)
	afterWrites := getCounterValue(t, TraceCacheWritesTotal)
	if afterHTTP != beforeHTTP {
		t.Errorf("HTTP: %v → %v (expected no change while disabled)", beforeHTTP, afterHTTP)
	}
	if afterHook != beforeHook {
		t.Errorf("Hook: %v → %v (expected no change while disabled)", beforeHook, afterHook)
	}
	if afterHits != beforeHits {
		t.Errorf("Hits: %v → %v (expected no change while disabled)", beforeHits, afterHits)
	}
	if afterWrites != beforeWrites {
		t.Errorf("Writes: %v → %v (expected no change while disabled)", beforeWrites, afterWrites)
	}
}

// TestGating_WorksWhenEnabled — mirror of the disabled test: after
// Enable(), helpers DO observe. Sanity check so someone who moves the
// atomic check can't accidentally break the enabled path.
func TestGating_WorksWhenEnabled(t *testing.T) {
	Enable()
	t.Cleanup(Disable)
	if !IsEnabled() {
		t.Fatal("Enable() did not set the flag")
	}
	before := getCounterValue(t, MaxResultsLimitedTotal)
	RecordMaxResultsLimited()
	RecordMaxResultsLimited()
	after := getCounterValue(t, MaxResultsLimitedTotal)
	if after != before+2 {
		t.Errorf("MaxResultsLimitedTotal: %v → %v, expected +2", before, after)
	}
}

// TestNewMetrics_RegisteredWhenEnabled verifies the Stage 9 Round 2 S6
// Python-parity metrics are registered and observable. When Enable()
// is called and each helper is invoked once, Gather() must return the
// metric family.
func TestNewMetrics_RegisteredWhenEnabled(t *testing.T) {
	Enable()
	t.Cleanup(Disable)
	// Touch each new metric family to ensure it appears in Gather().
	RecordError("test")
	RecordFileSize(1024)
	RecordPatternsPerRequest(1)
	RecordMatchesPerRequest(1)
	RecordParallelTasks(1)
	RecordMaxResultsLimited()
	RecordSamplesOffsets(1)
	RecordContextBefore(1)
	RecordContextAfter(1)
	RecordIndexLoadDuration(time.Second)
	RecordTraceCacheLoadDuration(time.Second)
	RecordTraceCacheReconstruction(time.Second)
	RecordTraceCacheSkip()
	RecordHookDuration("on_file", time.Second)

	for _, name := range []string{
		"rx_errors_total",
		"rx_file_size_bytes",
		"rx_patterns_per_request",
		"rx_matches_per_request",
		"rx_parallel_tasks_created",
		"rx_max_results_limited_total",
		"rx_offsets_per_samples_request",
		"rx_context_lines_before",
		"rx_context_lines_after",
		"rx_index_load_duration_seconds",
		"rx_trace_cache_load_duration_seconds",
		"rx_trace_cache_reconstruction_seconds",
		"rx_trace_cache_skip_total",
		"rx_hook_call_duration_seconds",
	} {
		if findMetric(t, name) == nil {
			t.Errorf("Stage 9 Round 2 S6 parity metric %q not registered", name)
		}
	}
}

func TestRecordCacheHitMiss(t *testing.T) {
	cases := []struct {
		kind string
		ok   bool
	}{
		{"trace", true},
		{"index", true},
		{"unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			if got := RecordCacheHit(tc.kind); got != tc.ok {
				t.Errorf("RecordCacheHit(%q): got %v, want %v", tc.kind, got, tc.ok)
			}
			if got := RecordCacheMiss(tc.kind); got != tc.ok {
				t.Errorf("RecordCacheMiss(%q): got %v, want %v", tc.kind, got, tc.ok)
			}
		})
	}
}

func TestRecordHook(t *testing.T) {
	// Stage 9 Round 2 S6: helpers are gated behind Enable(). Enable for
	// this test so the CounterVec gets labeled values and the family
	// appears in Gather().
	Enable()
	t.Cleanup(Disable)
	// Don't care about absolute count; care that Inc doesn't panic and
	// the label set is accepted.
	RecordHook("on_file", "success")
	RecordHook("on_match", "failure")
	RecordHook("on_complete", "success")
	family := findMetric(t, "rx_hook_calls_total")
	if family == nil {
		t.Fatal("rx_hook_calls_total not registered")
	}
	if len(family.GetMetric()) < 3 {
		t.Errorf("expected at least 3 label sets, got %d", len(family.GetMetric()))
	}
}

func TestRecordTraceDuration(t *testing.T) {
	// Enable for this test so helpers actually record.
	Enable()
	t.Cleanup(Disable)
	RecordTraceDuration("regular", 0)
	RecordTraceDuration("compressed", 0)
	RecordTraceDuration("seekable", 0)
	family := findMetric(t, "rx_trace_duration_seconds")
	if family == nil {
		t.Fatal("rx_trace_duration_seconds not registered")
	}
}

func TestHandler_ServesExposition(t *testing.T) {
	// Ensure at least one metric has a value before hitting /metrics.
	FilesProcessedTotal.Inc()

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "rx_files_processed_total") {
		t.Errorf("expected body to contain rx_files_processed_total, got:\n%s", body)
	}
}

// getCounterValue reaches into the unexported counter state to read its
// current value. Uses the client_model prototype (a prometheus-internal
// type) because that's the only supported way to read counters.
func getCounterValue(t *testing.T, c any) float64 {
	t.Helper()
	col, ok := c.(interface {
		Write(*dto.Metric) error
	})
	if !ok {
		t.Fatalf("counter does not expose Write: %T", c)
	}
	var m dto.Metric
	if err := col.Write(&m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if m.Counter == nil {
		t.Fatal("not a counter")
	}
	return m.Counter.GetValue()
}
