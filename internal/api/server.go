// Package api implements the HTTP API handlers for the rx web server.
//
// It provides a chi-based router with middleware for request ID generation,
// structured logging, panic recovery, CORS, and request timing. The Server
// type wires up all endpoint handlers and manages graceful shutdown.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	json "github.com/goccy/go-json"
	"github.com/google/uuid"

	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/metrics"
)

// Server holds the chi router, configuration, and shared state for all API handlers.
type Server struct {
	Router       chi.Router
	Config       *config.Config
	TaskStore    *TaskStore
	RequestStore *RequestStore
	Metrics      *metrics.Metrics
	FrontendDir  string    // path to frontend SPA files, empty if not installed
	StartTime    time.Time // server start time for uptime calculation
}

// NewServer creates a fully-wired chi router with middleware and all endpoint handlers.
func NewServer(cfg *config.Config) *Server {
	m := metrics.New(nil)

	s := &Server{
		Config:       cfg,
		TaskStore:    NewTaskStore(),
		RequestStore: NewRequestStore(),
		Metrics:      m,
		StartTime:    time.Now(),
	}

	r := chi.NewRouter()

	// Middleware stack — order matters. Outermost runs first.
	r.Use(requestIDMiddleware)          // Attach UUID v4 request ID to every request.
	r.Use(s.metricsMiddleware)          // Record Prometheus counters/histograms.
	r.Use(requestTimingMiddleware)      // Record and log request duration.
	r.Use(recoveryMiddleware)           // Catch panics and return 500.
	r.Use(loggingMiddleware)            // Structured slog logging per request.
	r.Use(corsMiddleware())             // Permissive CORS for development.

	// Mount all endpoint handlers.
	r.Get("/health", s.handleHealth)
	r.Get("/metrics", s.handleMetrics)
	r.Get("/v1/trace", s.handleTrace)
	r.Get("/v1/samples", s.handleSamples)
	r.Get("/v1/index", s.handleGetIndex)
	r.Post("/v1/index", s.handlePostIndex)
	r.Get("/v1/complexity", s.handleComplexity)
	r.Get("/v1/detectors", s.handleDetectors)
	r.Get("/v1/tasks/{id}", s.handleGetTask)
	r.Get("/v1/tree", s.handleTree)
	r.Get("/favicon.ico", s.handleFavicon)

	// Frontend SPA catch-all — must be last so API routes match first.
	r.NotFound(s.handleFrontendCatchAll)

	s.Router = r
	return s
}

// handleMetrics serves the Prometheus metrics endpoint.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.Metrics.Handler().ServeHTTP(w, r)
}

// statusRecorder wraps http.ResponseWriter to capture the status code
// for use in metrics middleware.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.statusCode = code
	sr.ResponseWriter.WriteHeader(code)
}

// metricsMiddleware records Prometheus request counters and duration histograms.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		sr := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(sr, r)

		duration := time.Since(start).Seconds()

		// Use the route pattern if available (chi provides this), otherwise the raw path.
		endpoint := r.URL.Path
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
			endpoint = rctx.RoutePattern()
		}

		s.Metrics.RequestsTotal.WithLabelValues(endpoint, r.Method, strconv.Itoa(sr.statusCode)).Inc()
		s.Metrics.RequestDurationSeconds.WithLabelValues(endpoint).Observe(duration)
	})
}

// ListenAndServe starts the HTTP server on the given address with graceful shutdown
// support. When the provided context is cancelled, the server drains in-flight requests
// within a 10-second deadline before returning.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start serving in a background goroutine.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("RX API server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Block until shutdown signal or server error.
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server shutdown: %w", err)
		}
		return nil

	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("server: %w", err)
		}
		return nil
	}
}

// --- Middleware ---

// requestIDKey is the context key for the per-request UUID.
type requestIDKeyType struct{}

var requestIDKey = requestIDKeyType{}

// RequestIDFromContext returns the request ID stored in the context, or "unknown".
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return "unknown"
}

// requestIDMiddleware generates a UUID v4 for each request and adds it to both
// the request context and the X-Request-ID response header.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.New().String()
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestTimingMiddleware records the request start time and logs the duration
// after the response is written.
func requestTimingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		duration := time.Since(start)

		slog.Debug("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ms", duration.Milliseconds(),
			"request_id", RequestIDFromContext(r.Context()),
		)
	})
}

// recoveryMiddleware catches panics in downstream handlers, logs the stack trace,
// and returns a 500 Internal Server Error response.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rvr := recover(); rvr != nil {
				slog.Error("panic recovered",
					"error", fmt.Sprintf("%v", rvr),
					"stack", string(debug.Stack()),
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()),
				)
				http.Error(w, `{"detail":"internal server error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs each incoming request at Info level.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"request_id", RequestIDFromContext(r.Context()),
		)
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware returns a permissive CORS handler suitable for local development.
// In production, this should be tightened to specific origins.
func corsMiddleware() func(http.Handler) http.Handler {
	return cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Request-ID"},
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: false,
		MaxAge:           300,
	})
}

// --- JSON response helpers ---

// writeJSON marshals v to JSON and writes it as the response body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal JSON response", "error", err)
		return
	}
	_, _ = w.Write(data)
}

// writeError writes a JSON error response matching FastAPI's {"detail": "..."} format.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
