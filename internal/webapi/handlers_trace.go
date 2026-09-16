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
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// MaxTraceContextLines is the most context lines a trace request may ask
// for on each side of a match, through context, before_context or
// after_context; a larger value is refused with a 422.
//
// Every match carries its own window, so the window multiplies the size
// of an answer: at this cap one match brings at most 201 lines. That is
// room for a long stack trace around a log line. A wider read around one
// place in a file is what GET /v1/samples is for.
//
// The parameters' struct tags spell the same number (`maximum:"100"`),
// because a tag cannot name a constant; a test compares the published
// maximum with this value.
const MaxTraceContextLines = 100

// traceInput is the query-string shape for GET /v1/trace.
//
// Notes on huma tag semantics:
//   - query:"path,explode" with a []string type ⇒ repeatable param:
//     ?path=a&path=b, one value per repetition, a comma kept inside its
//     value. Without the explode option huma splits a single value at
//     commas and ignores repetitions, which breaks a counted repetition
//     such as a{2,5} and any path that contains a comma. Every slice
//     query field carries it; TestOpenAPI_EveryArrayQueryParameterIsExploded
//     checks the published spec for that.
//   - required:"true" makes huma emit a 422 when the param is missing
//   - example:"..." surfaces in the generated OpenAPI and Swagger UI
//   - doc:"..." is the human description
//
// Field names use exact Python spellings to keep the OpenAPI shape
// identical for frontend consumption.
type traceInput struct {
	Path []string `query:"path,explode" required:"true" example:"/var/log/app.log" doc:"File or directory path(s) to search"`
	// Regexp uses the Python-compatible name (singular); the engine
	// accepts multiple via repeated ?regexp=... params.
	Regexp []string `query:"regexp,explode" required:"true" example:"error" doc:"Regex pattern(s) to search for"`
	// MaxResults: 0 sentinel means "not set" (huma doesn't allow pointer
	// query params). Valid user values start at 1.
	MaxResults     int    `query:"max_results" minimum:"0" example:"100" doc:"Maximum results to return. 0 = unlimited."`
	RequestID      string `query:"request_id" example:"01936c8e-7b2a-7000-8000-000000000001" doc:"Custom UUID v7 request ID"`
	HookOnFile     string `query:"hook_on_file" doc:"Webhook URL called with GET once per file after its scan (event file_scanned, payload as query parameters). Overrides RX_HOOK_ON_FILE_URL for this request."`
	HookOnMatch    string `query:"hook_on_match" doc:"Webhook URL called with GET once per match (event match_found, payload as query parameters). Requires max_results. Overrides RX_HOOK_ON_MATCH_URL for this request."`
	HookOnComplete string `query:"hook_on_complete" doc:"Webhook URL called with GET once when the trace completes (event trace_complete, payload as query parameters). Overrides RX_HOOK_ON_COMPLETE_URL for this request."`

	// The matching flags of trace.MatchingFlags, one boolean each, named
	// after ripgrep's long flags. huma reads a parameter's name from a
	// struct tag, which cannot be computed, so the five are spelled out
	// here and matchingFlags maps them back to the table.
	IgnoreCase   bool `query:"ignore_case" doc:"Match case-insensitively (ripgrep -i)"`
	WordRegexp   bool `query:"word_regexp" doc:"Match only whole words (ripgrep -w)"`
	LineRegexp   bool `query:"line_regexp" doc:"Match only whole lines (ripgrep -x)"`
	FixedStrings bool `query:"fixed_strings" doc:"Treat every pattern as literal text (ripgrep -F)"`
	PCRE2        bool `query:"pcre2" doc:"Use the PCRE2 engine, for look-around and backreferences (ripgrep -P)"`

	// The context window of `rx trace --context`, `--before` and
	// `--after` (-C, -B, -A), spelled as GET /v1/samples spells it.
	// context is 0 when absent, which is also the CLI's default.
	// before_context and after_context use -1 for "not given, take
	// context": huma forbids pointer query parameters, and a given 0
	// must still win over context, as --before=0 does on the command
	// line. The maximum is MaxTraceContextLines.
	Context       int `query:"context" minimum:"0" maximum:"100" default:"0" example:"3" doc:"Context lines before and after each match (rx trace --context). Fills context_lines, before_context and after_context of the answer."`
	BeforeContext int `query:"before_context" minimum:"-1" maximum:"100" default:"-1" doc:"Context lines before each match (rx trace --before); wins over context, 0 included. -1 = the context value."`
	AfterContext  int `query:"after_context" minimum:"-1" maximum:"100" default:"-1" doc:"Context lines after each match (rx trace --after); wins over context, 0 included. -1 = the context value."`

	// The switches of `rx trace --no-cache`, `--no-index` and
	// `--no-recursive`. Each changes how rx reaches the answer, or which
	// files a directory path covers, and never the answer for a file.
	NoCache     bool `query:"no_cache" doc:"Neither read nor write the trace cache (rx trace --no-cache)"`
	NoIndex     bool `query:"no_index" doc:"Read and write no line index; a match a capped scan left unnumbered is numbered by counting lines from the start of the file (rx trace --no-index)"`
	NoRecursive bool `query:"no_recursive" doc:"Search only the files directly inside a directory path, not its subdirectories (rx trace --no-recursive)"`
}

// contextWindow returns the lines to show before and after each match,
// resolved the way `rx trace` resolves --context, --before and --after:
// a given before_context or after_context wins over context, 0 included.
func (in *traceInput) contextWindow() (before, after int) {
	before, after = in.Context, in.Context
	if in.BeforeContext >= 0 {
		before = in.BeforeContext
	}
	if in.AfterContext >= 0 {
		after = in.AfterContext
	}
	return before, after
}

// matchingFlags reports which matching flags the query turned on, keyed
// by the long name trace.MatchingFlags uses.
func (in *traceInput) matchingFlags() map[string]bool {
	return map[string]bool{
		"ignore-case":   in.IgnoreCase,
		"word-regexp":   in.WordRegexp,
		"line-regexp":   in.LineRegexp,
		"fixed-strings": in.FixedStrings,
		"pcre2":         in.PCRE2,
	}
}

// selectedFlagNames lists the long names of the flags that are on, in the
// table's order, for the equivalent CLI command.
func selectedFlagNames(selected map[string]bool) []string {
	names := []string{}
	for _, flag := range trace.MatchingFlags {
		if selected[flag.Long] {
			names = append(names, flag.Long)
		}
	}
	return names
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
//  7. Call trace.Engine.RunWithOptions with this request's view of the
//     shared hook dispatcher.
//  8. Build response, fire on_complete hook, record metrics, return.
func registerTraceHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "trace",
		Method:      http.MethodGet,
		Path:        "/v1/trace",
		Summary:     "Search file for regex patterns (supports multiple patterns)",
		Description: "Uses ripgrep to scan one or more paths for one or more regex patterns, returning match offsets.",
		Tags:        []string{"Search"},
		Responses: errorResponses(api, http.StatusBadRequest, http.StatusForbidden,
			http.StatusNotFound, http.StatusUnprocessableEntity,
			http.StatusInternalServerError, http.StatusServiceUnavailable),
		// The results are named only so the deferred counter below can
		// read whatever this handler ends up returning; nothing assigns
		// them directly.
	}, func(ctx context.Context, in *traceInput) (traceResp *traceOutput, traceErr error) {
		// One counter increment per request, whichever of the handler's
		// many returns is taken.
		defer func() { recordEndpoint(prometheus.RecordTraceRequest, traceErr) }()

		if s.cfg.RipgrepPath == "" {
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
		// via the request-scoped hook_on_* query params. .
		//
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

		// 0 is huma's "not set" sentinel; the engine wants nil for that.
		var maxResultsPtr *int
		if in.MaxResults > 0 {
			m := in.MaxResults
			maxResultsPtr = &m
		}

		// This request's view of the shared hook dispatcher: its own URLs
		// and its own request_id. The engine keeps its no-hook fast path
		// (NoopHookFirer) when the request has no hook at all.
		reqHooks := requestHooks(s, hookConfig, reqID)
		var firer trace.HookFirer = trace.NoopHookFirer{}
		if reqHooks != nil {
			firer = reqHooks
		}

		// Run the engine.
		matchingFlags := in.matchingFlags()
		before, after := in.contextWindow()
		start := time.Now()
		resp, err := s.cfg.Engine.RunWithOptions(ctx, validatedPaths, in.Regexp, trace.Options{
			MaxResults:    maxResultsPtr,
			RgExtraArgs:   trace.RipgrepArgs(matchingFlags),
			ContextBefore: before,
			ContextAfter:  after,
			NoCache:       in.NoCache,
			NoIndex:       in.NoIndex,
			NoRecursive:   in.NoRecursive,
			HookFirer:     firer,
			RequestID:     reqID,
		})
		if err != nil {
			// A pattern ripgrep cannot compile is the caller's mistake,
			// not ours, and rg's message names the exact position.
			if errors.Is(err, trace.ErrInvalidPattern) {
				return nil, ErrInvalidRegex(err.Error())
			}
			return nil, ErrInternal(fmt.Sprintf("Internal error: %s", err.Error()))
		}
		resp.RequestID = reqID

		observeTraceDuration(validatedPaths, start)
		observeTraceResult(resp)

		// Fire on_complete hook if configured. resp.RequestID is set
		// above, so the payload and the response carry the same ID.
		if reqHooks != nil {
			reqHooks.OnComplete(resp)
		}

		// Attach CLI command equivalent. CLICommand is *string because a
		// schema-documented field must emit null rather than vanish, so
		// &cli converts the builder's string into a pointer.
		// The request ID and the hook URLs are the ones the request
		// gave, not the generated ID or the RX_HOOK_* fallbacks: the
		// command run elsewhere reads its own environment, as this
		// server did.
		cli := BuildCLICommand("trace", map[string]any{
			"path":             validatedPaths,
			"regexp":           in.Regexp,
			"matching_flags":   selectedFlagNames(matchingFlags),
			"max_results":      maxResultsPtr,
			"context":          in.Context,
			"before_context":   nilIfNegative(in.BeforeContext),
			"after_context":    nilIfNegative(in.AfterContext),
			"no_cache":         in.NoCache,
			"no_index":         in.NoIndex,
			"no_recursive":     in.NoRecursive,
			"request_id":       in.RequestID,
			"hook_on_file":     in.HookOnFile,
			"hook_on_match":    in.HookOnMatch,
			"hook_on_complete": in.HookOnComplete,
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

// requestHooks returns this request's view of the server's shared hook
// dispatcher, carrying the request's effective hook URLs and its
// request_id. It returns nil when the request has no hook URL at all, or
// when the server was built without a dispatcher (some tests); the
// caller then gives the engine trace.NoopHookFirer.
//
// The URLs come from the request, never from the dispatcher: one
// dispatcher serves every concurrent request, so it holds no
// request-level state.
func requestHooks(s *Server, cfg hooks.HookConfig, requestID string) *hooks.RequestHooks {
	if !cfg.HasAny() || s.cfg.Hooks == nil {
		return nil
	}
	return s.cfg.Hooks.ForRequest(cfg, requestID)
}
