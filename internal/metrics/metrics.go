// Package metrics exposes Prometheus counters and histograms for request tracking.
//
// All metrics are registered on a custom registry (not the default global one)
// so tests can create isolated registries without polluting each other. The
// DefaultMetrics variable provides a ready-to-use instance for production code.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus collectors used by the application.
// Using a struct (rather than package-level globals) lets tests create
// isolated instances backed by separate registries.
type Metrics struct {
	Registry *prometheus.Registry

	// --- Request counters ---

	// RequestsTotal tracks the total number of HTTP requests by endpoint, method, and status code.
	RequestsTotal *prometheus.CounterVec

	// RequestDurationSeconds records HTTP request latency.
	RequestDurationSeconds *prometheus.HistogramVec

	// --- Search-specific ---

	// MatchesTotal counts the total number of regex matches found across all trace requests.
	MatchesTotal prometheus.Counter

	// FilesScannedTotal counts the total number of files processed across all trace requests.
	FilesScannedTotal prometheus.Counter

	// SearchDurationSeconds records the wall-clock time of the core search operation
	// (excludes HTTP overhead).
	SearchDurationSeconds prometheus.Histogram
}

// New creates a Metrics instance with all collectors registered on the given registry.
// If registry is nil, a new one is created.
func New(registry *prometheus.Registry) *Metrics {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}

	m := &Metrics{
		Registry: registry,

		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rx_requests_total",
			Help: "Total number of HTTP requests by endpoint, method, and status code.",
		}, []string{"endpoint", "method", "status"}),

		RequestDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rx_request_duration_seconds",
			Help:    "HTTP request latency in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"endpoint"}),

		MatchesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rx_matches_total",
			Help: "Total number of regex matches found across all trace requests.",
		}),

		FilesScannedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rx_files_scanned_total",
			Help: "Total number of files scanned across all trace requests.",
		}),

		SearchDurationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "rx_search_duration_seconds",
			Help:    "Wall-clock search duration in seconds (core engine, excludes HTTP overhead).",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),
	}

	// Register all collectors on the registry.
	registry.MustRegister(
		m.RequestsTotal,
		m.RequestDurationSeconds,
		m.MatchesTotal,
		m.FilesScannedTotal,
		m.SearchDurationSeconds,
	)

	return m
}

// Handler returns an http.Handler that serves the /metrics endpoint in
// Prometheus text exposition format, scoped to this Metrics instance's registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: false,
	})
}

// DefaultMetrics is the package-level instance used in production. It is
// initialized lazily on first access via Init(). Test code should call New()
// with its own registry instead.
var DefaultMetrics *Metrics

// Init creates the DefaultMetrics instance backed by a new registry.
// It is safe to call multiple times; subsequent calls are no-ops.
func Init() *Metrics {
	if DefaultMetrics == nil {
		DefaultMetrics = New(nil)
	}
	return DefaultMetrics
}
