package webapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/wlame/rx-go/internal/prometheus"
)

// Context keys. Use a private type so external packages cannot collide
// by accident — a standard Go pattern for context.WithValue keys.
type ctxKey string

const (
	// ctxKeyRequestID holds the per-request UUID v7 string.
	ctxKeyRequestID ctxKey = "rx.request_id"
)

// RequestIDFromContext returns the request ID stored by
// requestIDMiddleware, or "" if none was set (e.g. tests that bypass
// middleware).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// requestIDMiddleware assigns a UUID v7 to every request and propagates
// it through ctx and the X-Request-ID response header.
//
// UUID v7 is time-sortable (prefix is ms since epoch) which makes log
// triage much easier than v4 randoms; the spec's decision 5.9 pins
// request IDs to v7.
//
// If the client supplies an X-Request-ID header we trust it (caps at
// 128 chars so a malicious client can't pollute logs with huge values).
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			id, err := uuid.NewV7()
			if err != nil {
				// Fall back to v4 on the (very rare) v7 failure.
				id = uuid.New()
			}
			rid = id.String()
		}
		if len(rid) > 128 {
			rid = rid[:128]
		}
		w.Header().Set("X-Request-ID", rid)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder wraps http.ResponseWriter to capture the status code
// so loggingMiddleware and metricsMiddleware can report it. The default
// ResponseWriter interface doesn't expose the code once written, so we
// intercept WriteHeader (and the first Write, which implicitly sends
// 200 if WriteHeader hasn't been called yet).
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	n, err := sr.ResponseWriter.Write(b)
	sr.bytes += n
	return n, err
}

// loggingMiddleware emits one structured log record per request at
// completion. Keys are deliberately stable so downstream log pipelines
// can alert on them.
func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w, status: 0}
			next.ServeHTTP(sr, r)
			dur := time.Since(start)
			logger.Info("http_request",
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sr.status),
				slog.Int("bytes", sr.bytes),
				slog.Duration("duration", dur),
			)
		})
	}
}

// recoverMiddleware turns goroutine panics (in the HTTP handler
// pathway) into a 500 with a logged stack trace, instead of bringing
// down the whole server. Kept as close to the handler as possible so
// the stack trace points at the real crashing code.
func recoverMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						slog.String("request_id", RequestIDFromContext(r.Context())),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("panic", fmt.Sprintf("%v", rec)),
						slog.String("stack", string(debug.Stack())),
					)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					// Write a minimal error envelope matching the rest of
					// the API — {"detail":"..."} — so the frontend can
					// surface the error without special-casing.
					_, _ = w.Write([]byte(`{"detail":"Internal server error"}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// metricsMiddleware updates rx_http_responses_total{method,endpoint,status}.
//
// Uses the matched chi route PATTERN as the `endpoint` label rather
// than r.URL.Path so per-request IDs don't explode Prometheus
// cardinality. Concretely: GET /v1/tasks/abc-123 and /v1/tasks/def-456
// both report endpoint="/v1/tasks/{task_id}" — one time series instead
// of N.
//
// Before Stage 8's fix, this middleware used r.URL.Path directly despite
// the comment claiming otherwise. That made long-running servers
// accumulate unbounded label values, slowing /metrics scrapes and
// causing Prometheus itself to enforce its cardinality limit by
// dropping metrics. See Stage 8 Reviewer 3 High #11.
//
// Fallback behavior: chi.RouteContext MAY be nil or return "" if the
// route didn't match any registered pattern (e.g. 404 static file).
// In that case we fall back to r.URL.Path so 404 traffic still shows
// up in metrics; the cardinality risk is bounded because no path-
// parameter explosion happens on unmatched routes.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr := &statusRecorder{ResponseWriter: w, status: 0}
		next.ServeHTTP(sr, r)
		if sr.status == 0 {
			sr.status = http.StatusOK
		}
		// Resolve the endpoint label from the chi route context. The
		// context is populated by chi's router when it matches a
		// registered pattern; it contains the template string
		// (e.g. "/v1/tasks/{task_id}").
		//
		// NOTE: chi.RouteContext must be invoked AFTER next.ServeHTTP
		// — the routing info only attaches to the request's context
		// inside the router's dispatch. Calling before ServeHTTP would
		// return a nil context.
		endpoint := r.URL.Path
		if rctx := chi.RouteContext(r.Context()); rctx != nil {
			if pattern := rctx.RoutePattern(); pattern != "" {
				endpoint = pattern
			}
		}
		prometheus.RecordHTTPResponse(r.Method, endpoint, sr.status)
	})
}
