package webapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/frontend"
	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/trace"
)

// Config bundles the settings needed to stand up a webapi Server.
//
// All fields are optional: zero-valued / nil fields use defaults (a
// fresh trace.Engine, a default tasks.Manager, a no-op hooks firer, etc).
// This keeps tests ergonomic — httptest callers only override what they
// care about — while production callers supply fully-configured deps.
type Config struct {
	// Host/Port bind address. Defaults: host "127.0.0.1", port 7777.
	Host string
	Port int

	// AppVersion surfaces through /health. Defaults to "dev".
	AppVersion string

	// RipgrepPath is empty when ripgrep is not on PATH. /health reports
	// "ripgrep_available: false" in that case, and /v1/trace returns 503
	// Service Unavailable. /v1/samples does not run ripgrep and answers
	// either way.
	RipgrepPath string

	// Backends. Any nil pointer is replaced with a sensible default at
	// NewServer() time.
	Engine      *trace.Engine
	TaskManager *tasks.Manager
	Hooks       *hooks.Dispatcher
	Frontend    *frontend.Manager

	// Logger used by middleware. Defaults to slog.Default().
	Logger *slog.Logger

	// APIToken, when non-empty, is required as a bearer token on every
	// /v1 request (see auth.go). Empty leaves the API open.
	APIToken string

	// ShutdownTimeout is how long Shutdown waits for in-flight requests
	// to drain before forcibly killing the server. Defaults to 10s.
	ShutdownTimeout time.Duration

	// SamplesIndexWait is how long GET /v1/samples waits for a line
	// index it needs to be built before answering 202 with the build's
	// task. Zero takes RX_SAMPLES_WAIT_SECONDS, or 5 s
	// (config.SamplesIndexWait).
	SamplesIndexWait time.Duration

	// buildSamplesIndex builds and stores the index a samples lookup
	// waits for; samples.BuildIndex unless a test replaces it. Set here,
	// before the server starts, so no handler goroutine can read it
	// while it changes.
	buildSamplesIndex samplesIndexBuilder
}

// Server owns the http.Server, chi router, and huma API instance.
//
// Build one via NewServer, call Start() to begin listening in a
// goroutine, and Shutdown(ctx) to drain gracefully.
type Server struct {
	cfg    Config
	router chi.Router
	api    huma.API
	http   *http.Server

	// samplesIndex runs the index builds GET /v1/samples waits for,
	// one per file at a time.
	samplesIndex *samplesIndexBuilds

	// Exported for handler helpers (e.g. integration tests that need
	// to fire a direct http.Handler call).
	Handler http.Handler
}

// NewServer constructs a *Server ready to be started. It registers
// middleware and all endpoints but does NOT open a listening socket.
func NewServer(cfg Config) *Server {
	applyConfigDefaults(&cfg)

	router := chi.NewRouter()

	// ------------------------------------------------------------------
	// Middleware ordering matters for propagation.
	//   security  → response headers a meta tag cannot carry; first, so
	//               they are present on every response including a panic
	//   requestID → populates ctx, emits X-Request-ID header
	//   logger    → reads requestID from ctx; logs method/path/status
	//   recover   → converts panics to 500 + structured log
	//   metrics   → rx_http_responses_total
	//   token     → 401 for a /v1 request without the API token, after
	//               the layers above so a refusal is logged and counted
	// ------------------------------------------------------------------
	router.Use(securityHeadersMiddleware)
	router.Use(requestIDMiddleware)
	router.Use(loggingMiddleware(cfg.Logger))
	router.Use(recoverMiddleware(cfg.Logger))
	router.Use(metricsMiddleware)
	router.Use(apiTokenMiddleware(cfg.APIToken))

	// Build the huma API with the custom OpenAPI config (title, servers,
	// info, error envelope override).
	humaCfg := newHumaConfig(cfg.AppVersion)
	api := humachi.New(router, humaCfg)

	s := &Server{
		cfg:          cfg,
		router:       router,
		api:          api,
		samplesIndex: newSamplesIndexBuilds(cfg.TaskManager, cfg.Logger, cfg.buildSamplesIndex),
	}

	// Schemas huma cannot reflect from the Go types; they must be in
	// place before the first operation that uses them is registered.
	registerWireSchemas(api)

	// Register all typed handlers through huma. Route order does not
	// matter across the humachi calls because chi matches exact routes
	// first; the SPA catch-all comes after.
	registerHealthHandlers(s, api)
	registerTraceHandlers(s, api)
	registerSamplesHandlers(s, api)
	registerIndexHandlers(s, api)
	registerCompressHandlers(s, api)
	registerTaskHandlers(s, api)
	registerTreeHandlers(s, api)
	registerDetectorsHandlers(s, api)
	declareOptionalBearerToken(api)

	// Non-huma routes: /metrics (Prometheus expfmt is raw, not JSON;
	// doesn't fit huma's typed handler model), favicon, SPA.
	registerMetricsHandler(router)
	registerFaviconHandler(router)
	// SPA/static MUST be registered last so reserved prefixes fall through
	// to their registered handlers (they return 404 on miss; the SPA
	// catch-all would otherwise pretend "/v1/foo" is a client-side route).
	registerStaticHandlers(router, cfg.Frontend)

	s.http = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.Handler = router
	return s
}

// applyConfigDefaults fills in nil/zero fields. Mutates the argument.
func applyConfigDefaults(cfg *Config) {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 7777
	}
	if cfg.AppVersion == "" {
		cfg.AppVersion = "dev"
	}
	if cfg.Engine == nil {
		cfg.Engine = trace.New()
	}
	if cfg.TaskManager == nil {
		cfg.TaskManager = tasks.New(tasks.Config{})
	}
	// Always start the sweeper, whether we built the manager or the
	// caller gave us one. Start() is idempotent (sync.Once-gated).
	cfg.TaskManager.Start()
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}
	if cfg.SamplesIndexWait == 0 {
		cfg.SamplesIndexWait = config.SamplesIndexWait()
	}
	if cfg.buildSamplesIndex == nil {
		cfg.buildSamplesIndex = samples.BuildIndex
	}
}

// Start binds the listening socket and begins serving. Blocks until
// http.Server.ListenAndServe returns (normally only on Shutdown or
// fatal error). Intended to run in its own goroutine; in the usual
// case the caller selects on SIGINT/SIGTERM and then calls Shutdown.
//
// It calls prometheus.Enable() before the socket binds, so metric
// updates from in-flight HTTP work are recorded. CLI invocations
// (rx trace / index / samples / compress) never take this path, so the
// prometheus gate stays false for them and they pay nothing.
func (s *Server) Start() error {
	enableMetrics()
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		// Graceful shutdown path — not an error.
		return nil
	}
	return err
}

// enableMetrics turns metric collection on and publishes the
// configuration gauges a scrape needs to read the other families.
func enableMetrics() {
	prometheus.Enable()
	prometheus.RecordConfig(config.LargeFileMB())
}

// ServeHTTP makes *Server satisfy http.Handler. Useful for httptest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// Shutdown drains in-flight requests, then closes dependent backends
// (task manager, hook dispatcher). Respects the caller's ctx deadline
// as the upper bound for the drain; always runs backend shutdown even
// if the HTTP drain times out, so resources are released promptly.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cfg.Logger.Info("webapi: shutdown starting")

	// The caller ctx typically already has a reasonable deadline; if
	// not, layer our own ShutdownTimeout on top.
	drainCtx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()

	// HTTP server drain first so no new work enters the backends.
	httpErr := s.http.Shutdown(drainCtx)
	if httpErr != nil {
		s.cfg.Logger.Warn("webapi: http shutdown returned error",
			slog.String("err", httpErr.Error()))
	}

	// Backends.
	if s.cfg.TaskManager != nil {
		s.cfg.TaskManager.Stop()
	}
	if s.cfg.Hooks != nil {
		s.cfg.Hooks.Close()
		s.cfg.Hooks.Wait()
	}
	s.cfg.Logger.Info("webapi: shutdown complete")
	return httpErr
}

// API exposes the huma.API for tests that want to enumerate registered
// operations (OpenAPI golden-file diffing uses this).
func (s *Server) API() huma.API { return s.api }

// Router exposes the underlying chi.Router for tests that need to
// install additional handlers (not used in production).
func (s *Server) Router() chi.Router { return s.router }
