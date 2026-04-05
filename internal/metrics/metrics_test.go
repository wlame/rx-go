package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_RegistersAllCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	assert.NotNil(t, m.RequestsTotal)
	assert.NotNil(t, m.RequestDurationSeconds)
	assert.NotNil(t, m.MatchesTotal)
	assert.NotNil(t, m.FilesScannedTotal)
	assert.NotNil(t, m.SearchDurationSeconds)
	assert.Same(t, reg, m.Registry)
}

func TestNew_NilRegistry_CreatesOne(t *testing.T) {
	m := New(nil)
	assert.NotNil(t, m.Registry)
}

func TestMetrics_CounterIncrements(t *testing.T) {
	m := New(nil)

	// Increment counters.
	m.RequestsTotal.WithLabelValues("/v1/trace", "GET", "200").Inc()
	m.RequestsTotal.WithLabelValues("/v1/trace", "GET", "200").Inc()
	m.RequestsTotal.WithLabelValues("/health", "GET", "200").Inc()
	m.MatchesTotal.Add(42)
	m.FilesScannedTotal.Add(5)

	// Serve /metrics and verify the text output contains expected metric names and values.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	body, err := io.ReadAll(rr.Body)
	require.NoError(t, err)
	text := string(body)

	// Counter for /v1/trace should show 2.
	assert.Contains(t, text, `rx_requests_total{endpoint="/v1/trace",method="GET",status="200"} 2`)
	// Counter for /health should show 1.
	assert.Contains(t, text, `rx_requests_total{endpoint="/health",method="GET",status="200"} 1`)
	// Matches counter.
	assert.Contains(t, text, "rx_matches_total 42")
	// Files scanned counter.
	assert.Contains(t, text, "rx_files_scanned_total 5")
}

func TestMetrics_HistogramObserve(t *testing.T) {
	m := New(nil)

	m.RequestDurationSeconds.WithLabelValues("/v1/trace").Observe(0.123)
	m.SearchDurationSeconds.Observe(1.5)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rr, req)

	body, err := io.ReadAll(rr.Body)
	require.NoError(t, err)
	text := string(body)

	// Histogram should have a _count of 1.
	assert.Contains(t, text, `rx_request_duration_seconds_count{endpoint="/v1/trace"} 1`)
	assert.Contains(t, text, "rx_search_duration_seconds_count 1")
}

func TestMetrics_Handler_PrometheusTextFormat(t *testing.T) {
	m := New(nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	contentType := rr.Header().Get("Content-Type")
	assert.True(t, strings.Contains(contentType, "text/plain") || strings.Contains(contentType, "text/plain"),
		"Content-Type should be prometheus text format, got: %s", contentType)
}

func TestMetrics_IsolatedRegistries(t *testing.T) {
	// Two Metrics instances should not interfere with each other.
	m1 := New(nil)
	m2 := New(nil)

	m1.MatchesTotal.Add(100)
	m2.MatchesTotal.Add(1)

	// m1 handler should show 100, not 101.
	rr1 := httptest.NewRecorder()
	m1.Handler().ServeHTTP(rr1, httptest.NewRequest("GET", "/metrics", nil))
	assert.Contains(t, rr1.Body.String(), "rx_matches_total 100")

	// m2 handler should show 1, not 101.
	rr2 := httptest.NewRecorder()
	m2.Handler().ServeHTTP(rr2, httptest.NewRequest("GET", "/metrics", nil))
	assert.Contains(t, rr2.Body.String(), "rx_matches_total 1")
}

func TestInit_ReturnsDefaultMetrics(t *testing.T) {
	// Reset global state for this test.
	DefaultMetrics = nil

	m := Init()
	assert.NotNil(t, m)
	assert.Same(t, DefaultMetrics, m)

	// Calling Init again returns the same instance.
	m2 := Init()
	assert.Same(t, m, m2)

	// Clean up global state.
	DefaultMetrics = nil
}
