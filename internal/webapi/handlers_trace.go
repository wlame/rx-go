package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/requeststore"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// traceInput is the query-string shape for GET /v1/trace.
//
// Notes on huma tag semantics:
//   - query:"path" with a []string type ⇒ repeatable param: ?path=a&path=b
//   - required:"true" makes huma emit a 422 when the param is missing
//   - example:"..." surfaces in the generated OpenAPI and Swagger UI
//   - doc:"..." is the human description
//
// Field names use exact Python spellings to keep the OpenAPI shape
// identical for frontend consumption.
type traceInput struct {
	Path []string `query:"path" required:"true" example:"/var/log/app.log" doc:"File or directory path(s) to search"`
	// Regexp uses the Python-compatible name (singular); the engine
	// accepts multiple via repeated ?regexp=... params.
	Regexp []string `query:"regexp" required:"true" example:"error" doc:"Regex pattern(s) to search for"`
	// MaxResults: 0 sentinel means "not set" (huma doesn't allow pointer
	// query params). Valid user values start at 1.
	MaxResults     int    `query:"max_results" minimum:"0" example:"100" doc:"Maximum results to return. 0 = unlimited."`
	RequestID      string `query:"request_id" example:"01936c8e-7b2a-7000-8000-000000000001" doc:"Custom UUID v7 request ID"`
	HookOnFile     string `query:"hook_on_file" doc:"URL to POST when file scan completes"`
	HookOnMatch    string `query:"hook_on_match" doc:"URL to POST per match. Requires max_results."`
	HookOnComplete string `query:"hook_on_complete" doc:"URL to POST when the whole trace completes"`
}

// traceOutput wraps the TraceResponse body.
type traceOutput struct {
	Body rxtypes.TraceResponse
}

// registerTraceHandlers mounts GET /v1/trace.
//
// Matches rx-python/src/rx/web.py:355-607. Flow:
//  1. Check ripgrep availability (503 when missing).
//  2. Validate every path via paths.ValidatePathWithinRoots (403 outside).
//  3. Resolve effective hook config (env + per-request overrides).
//  4. Validate hook_on_match ⇒ max_results constraint (400 if violated).
//  5. Verify every path exists (404 otherwise).
//  6. Generate / accept a request ID.
//  7. Record RequestInfo in the request store.
//  8. Call trace.Engine.RunWithOptions with a dispatcher-wrapped HookFirer.
//  9. Build response, fire on_complete hook, record metrics, return.
func registerTraceHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "trace",
		Method:      http.MethodGet,
		Path:        "/v1/trace",
		Summary:     "Search file for regex patterns (supports multiple patterns)",
		Description: "Uses ripgrep to scan one or more paths for one or more regex patterns, returning match offsets.",
		Tags:        []string{"Search"},
	}, func(ctx context.Context, in *traceInput) (*traceOutput, error) {
		if s.cfg.RipgrepPath == "" {
			prometheus.RecordHTTPResponse(http.MethodGet, "/v1/trace", http.StatusServiceUnavailable)
			return nil, ErrServiceUnavailable("ripgrep is not available on this system")
		}

		// Sandbox check across every path.
		validatedPaths := make([]string, 0, len(in.Path))
		for _, p := range in.Path {
			v, err := paths.ValidatePathWithinRoots(p)
			if err != nil {
				var perr *paths.ErrPathOutsideRoots
				if errors.As(err, &perr) {
					return nil, NewSandboxError(perr)
				}
				// Unsandboxed (no roots configured) → allow the path;
				// treat the special error explicitly so tests without
				// SetSearchRoots still exercise the endpoint.
				if errors.Is(err, paths.ErrNoSearchRootsConfigured) {
					v = p
				} else {
					return nil, ErrForbidden(err.Error())
				}
			}
			validatedPaths = append(validatedPaths, v)
		}

		// Effective hook config.
		overrides := hookOverridesFromQuery(in.HookOnFile, in.HookOnMatch, in.HookOnComplete)
		hookConfig := hooks.EffectiveHooks(hooks.HookEnvFromEnv(), overrides)

		// SSRF validation: reject hook URLs pointing at loopback / link-
		// local / private addresses unless the operator has explicitly
		// opted in via RX_ALLOW_INTERNAL_HOOKS. Prevents a malicious
		// user from targeting internal infrastructure (e.g. cloud IMDS)
		// via the request-scoped hook_on_* query params. See Stage 8
		// Reviewer 2 High #15 / Finding 15.
		if err := hooks.ValidateConfig(hookConfig); err != nil {
			return nil, ErrBadRequest(err.Error())
		}

		// Validation: max_results required when on_match is active.
		// 0 means "not set" (huma sentinel for pointer absence).
		if hookConfig.HasMatchHook() && in.MaxResults == 0 {
			return nil, ErrBadRequest(
				"max_results is required when hook_on_match is configured. " +
					"This prevents accidentally triggering millions of HTTP calls.",
			)
		}

		// Existence check.
		for _, p := range validatedPaths {
			if _, err := os.Stat(p); err != nil {
				if os.IsNotExist(err) {
					return nil, ErrNotFound(fmt.Sprintf("Path not found: %s", p))
				}
				return nil, ErrForbidden(err.Error())
			}
		}

		// Request ID: accept client-supplied, otherwise UUID v7.
		reqID := in.RequestID
		if reqID == "" {
			if id, err := uuid.NewV7(); err == nil {
				reqID = id.String()
			} else {
				reqID = uuid.New().String()
			}
		}

		// Store request metadata (start time, paths, patterns).
		var maxResultsPtr *int
		if in.MaxResults > 0 {
			m := in.MaxResults
			maxResultsPtr = &m
		}
		info := &requeststore.RequestInfo{
			RequestID:  reqID,
			Paths:      append([]string(nil), validatedPaths...),
			Patterns:   append([]string(nil), in.Regexp...),
			MaxResults: maxResultsPtr,
			StartedAt:  time.Now(),
		}
		s.cfg.RequestStore.Add(info)

		// Build a HookFirer — use the dispatcher when configured, else Noop.
		firer := buildHookFirer(s, hookConfig, reqID)

		// Run the engine.
		start := time.Now()
		resp, err := s.cfg.Engine.RunWithOptions(ctx, validatedPaths, in.Regexp, trace.Options{
			MaxResults: maxResultsPtr,
			HookFirer:  firer,
			RequestID:  reqID,
		})
		if err != nil {
			prometheus.RecordHTTPResponse(http.MethodGet, "/v1/trace", http.StatusInternalServerError)
			return nil, ErrInternal(fmt.Sprintf("Internal error: %s", err.Error()))
		}
		resp.RequestID = reqID

		// Update request-store bookkeeping.
		dur := time.Since(start)
		s.cfg.RequestStore.Update(reqID, func(r *requeststore.RequestInfo) {
			completed := time.Now()
			r.CompletedAt = &completed
			r.TotalMatches = int64(len(resp.Matches))
			r.TotalFilesScanned = int64(len(resp.Files))
			r.TotalFilesSkipped = int64(len(resp.SkippedFiles))
			r.TotalTimeMS = dur.Milliseconds()
		})

		// Fire on_complete hook if configured.
		if hookConfig.OnCompleteURL != "" && s.cfg.Hooks != nil {
			s.cfg.Hooks.OnComplete(resp)
		}

		// Attach CLI command equivalent. CLICommand is *string per Stage
		// 9 Round 2 S2 rule — &cli converts the builder's returned
		// string into a pointer for JSON serialization.
		cli := BuildCLICommand("trace", map[string]any{
			"path":        validatedPaths,
			"regexp":      in.Regexp,
			"max_results": maxResultsPtr,
		})
		resp.CLICommand = &cli

		return &traceOutput{Body: *resp}, nil
	})
}

// hookOverridesFromQuery converts empty strings to nil pointers so the
// "not supplied" case stays distinct from "explicitly disable" (the
// latter being an empty string arriving via &hook_on_file=, which is
// rare but supported).
//
// Huma gives us `""` for "absent", so we must treat empty as nil.
func hookOverridesFromQuery(onFile, onMatch, onComplete string) hooks.HookOverrides {
	var (
		of *string
		om *string
		oc *string
	)
	if onFile != "" {
		of = &onFile
	}
	if onMatch != "" {
		om = &onMatch
	}
	if onComplete != "" {
		oc = &onComplete
	}
	return hooks.HookOverrides{
		OnFileURL:     of,
		OnMatchURL:    om,
		OnCompleteURL: oc,
	}
}

// buildHookFirer picks the right trace.HookFirer for this request.
// When no hooks fire (either because none are configured OR we have no
// Dispatcher), we use trace.NoopHookFirer to keep the engine's fast path.
//
// The Dispatcher stores per-request URLs inside its config; we adapt by
// mutating the request-specific URL map in requestHookFirer — see the
// closure below. At the HTTP layer, dispatcher.Hooks holds the
// process-level config; per-request overrides land on a custom firer
// that chooses per event.
func buildHookFirer(s *Server, cfg hooks.HookConfig, requestID string) trace.HookFirer {
	if !cfg.HasAny() {
		return trace.NoopHookFirer{}
	}
	if s.cfg.Hooks == nil {
		// No dispatcher wired; silently drop events.
		return trace.NoopHookFirer{}
	}
	return &requestHookFirer{
		dispatcher: s.cfg.Hooks,
		cfg:        cfg,
		requestID:  requestID,
	}
}

// requestHookFirer adapts the per-request hook config to the shared
// hooks.Dispatcher. It forwards OnFile/OnMatch to the dispatcher only
// when the corresponding URL is set for this request.
type requestHookFirer struct {
	dispatcher *hooks.Dispatcher
	cfg        hooks.HookConfig
	requestID  string
}

// OnFile forwards to the dispatcher iff an on_file URL is set.
func (r *requestHookFirer) OnFile(ctx context.Context, path string, info trace.FileInfo) {
	if r.cfg.OnFileURL == "" {
		return
	}
	r.dispatcher.OnFile(ctx, path, info)
}

// OnMatch forwards to the dispatcher iff an on_match URL is set.
func (r *requestHookFirer) OnMatch(ctx context.Context, path string, m trace.MatchInfo) {
	if r.cfg.OnMatchURL == "" {
		return
	}
	r.dispatcher.OnMatch(ctx, path, m)
}
