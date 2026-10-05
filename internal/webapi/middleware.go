package webapi

import (
	"context"
	"errors"
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
// triage much easier than v4 randoms, so generated request IDs are v7
// (v4 only when v7 generation fails).
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

// statusClientClosedRequest is the status the request log and
// rx_http_responses_total report for a request whose client went away
// before its answer: 499, the code nginx uses for the same event. It is
// never written to a client, so no operation declares it.
const statusClientClosedRequest = 499

// reportedStatus returns the status to log and count for a request
// whose context is ctx and whose handler wrote status written.
//
// When the client goes away, net/http cancels the request's context, and
// a handler that honors it (samples stops its read, trace stops its
// workers) answers with a server error that reaches nobody. That 5xx is
// the cancellation's effect, not a server fault, so it is reported as
// statusClientClosedRequest. Any other status is the answer the request
// would have had with the client still there, and it stays.
func reportedStatus(ctx context.Context, written int) int {
	if written >= http.StatusInternalServerError && errors.Is(ctx.Err(), context.Canceled) {
		return statusClientClosedRequest
	}
	return written
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
				slog.Int("status", reportedStatus(r.Context(), sr.status)),
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

// unmatchedEndpointLabel is the endpoint label of a request no route
// matched: a path nothing serves, or a method a path does not accept.
// One constant keeps the series count fixed whatever paths a client
// tries.
const unmatchedEndpointLabel = "unmatched"

// metricsMiddleware updates rx_http_responses_total{method,endpoint,status_code}.
// It is the only place that counts a response, so each request is
// counted exactly once, with the status the client received, or 499 for
// a request whose client went away first (reportedStatus).
//
// The endpoint label is the matched chi route PATTERN, never r.URL.Path,
// so path parameters do not multiply the series: GET /v1/tasks/abc-123
// and /v1/tasks/def-456 both report endpoint="/v1/tasks/{task_id}".
// A request chi did not match has no pattern and is labeled
// unmatchedEndpointLabel.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr := &statusRecorder{ResponseWriter: w, status: 0}
		next.ServeHTTP(sr, r)
		if sr.status == 0 {
			sr.status = http.StatusOK
		}
		prometheus.RecordHTTPResponse(r.Method, endpointLabel(r), reportedStatus(r.Context(), sr.status))
	})
}

// endpointLabel returns the chi route pattern that served r, or
// unmatchedEndpointLabel when no route matched.
//
// It must be called AFTER next.ServeHTTP: chi's router fills the route
// context (shared with this middleware through the request's context)
// while it dispatches, so before that the pattern is still empty.
func endpointLabel(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return unmatchedEndpointLabel
}

// securityHeaders is the table of response headers every route carries.
//
// The viewer renders untrusted content by definition — the log lines it
// displays are attacker-influenced in many deployments — and these three
// protections cannot be expressed in the SPA's meta tag, because a
// browser ignores them there:
//
//   - frame-ancestors / X-Frame-Options: nothing otherwise stops the
//     viewer being framed, so a page on another origin could overlay it
//     and harvest clicks.
//   - X-Content-Type-Options: without it a browser may re-guess a
//     response's type and execute a log file as script.
//   - Referrer-Policy: a filesystem path in the query string would
//     otherwise travel to any host the user navigates to next.
//
// The header CSP carries only what a meta tag cannot. The full policy
// stays in the meta tag, which is the artifact that knows what Monaco
// needs — one copy, and a stricter header would intersect with it into
// something that breaks the editor.
//
// `serve` binds loopback by default, which limits exposure but does not
// remove it: users run it behind a reverse proxy, and a browser tab on
// any site can reach 127.0.0.1.
//
// rx-python sends the same table (`src/rx/web.py`).
var securityHeaders = map[string]string{
	"Content-Security-Policy": "frame-ancestors 'none'",
	"X-Frame-Options":         "DENY",
	"X-Content-Type-Options":  "nosniff",
	"Referrer-Policy":         "no-referrer",
}

// securityHeadersMiddleware sets securityHeaders on every response.
//
// Set before the handler runs, so they are present whatever the handler
// writes — including a 404 from the SPA fallback and a panic turned into
// a 500, which are exactly the responses a hardening check would
// otherwise miss.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		for name, value := range securityHeaders {
			header.Set(name, value)
		}
		next.ServeHTTP(w, r)
	})
}
