// Package hooks implements the webhook system for file, match, and completion events.
//
// Hooks are HTTP GET requests with query parameters, fired asynchronously (fire-and-forget).
// Each hook call has a 3-second timeout. Hook URLs can come from request parameters or
// environment variables, with request params taking priority unless RX_DISABLE_CUSTOM_HOOKS
// is set.
package hooks

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// HookTimeoutSeconds is the maximum time to wait for a single hook HTTP call.
const HookTimeoutSeconds = 3

// HookCallbacks holds the three webhook URLs for trace lifecycle events.
// Each field is optional (empty string means "no hook for this event").
type HookCallbacks struct {
	OnFileScanned string // Called when a file scan completes.
	OnMatchFound  string // Called for each match found.
	OnComplete    string // Called when the entire trace request finishes.
}

// HasAny returns true if at least one hook URL is configured.
func (h HookCallbacks) HasAny() bool {
	return h.OnFileScanned != "" || h.OnMatchFound != "" || h.OnComplete != ""
}

// HasMatchHook returns true if the on_match_found hook is configured.
func (h HookCallbacks) HasMatchHook() bool {
	return h.OnMatchFound != ""
}

// GetEffectiveHooks resolves the final hook URLs by merging request-provided hooks
// with environment-configured hooks according to priority rules.
//
// Priority (when custom hooks are allowed):
//  1. Request-provided URL (requestHooks)
//  2. Environment-configured URL (envHooks)
//
// When disableCustom is true, only envHooks are used — request-provided hooks are ignored.
// This is controlled by the RX_DISABLE_CUSTOM_HOOKS environment variable.
func GetEffectiveHooks(requestHooks, envHooks HookCallbacks, disableCustom bool) HookCallbacks {
	if disableCustom {
		return envHooks
	}

	return HookCallbacks{
		OnFileScanned: firstNonEmpty(requestHooks.OnFileScanned, envHooks.OnFileScanned),
		OnMatchFound:  firstNonEmpty(requestHooks.OnMatchFound, envHooks.OnMatchFound),
		OnComplete:    firstNonEmpty(requestHooks.OnComplete, envHooks.OnComplete),
	}
}

// CallHook sends an HTTP GET request to hookURL with the given query parameters.
// It respects the provided context for cancellation and enforces a per-call timeout
// of HookTimeoutSeconds.
//
// This is fire-and-forget: errors are logged but not propagated to the caller.
// Returns nil on success, or the error encountered.
func CallHook(ctx context.Context, hookURL string, params map[string]string) error {
	if hookURL == "" {
		return nil
	}

	// Build the URL with query parameters.
	u, err := url.Parse(hookURL)
	if err != nil {
		slog.Warn("hook: invalid URL", "url", hookURL, "error", err)
		return err
	}

	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	// Create a timeout-scoped context for this single hook call.
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(HookTimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		slog.Warn("hook: failed to build request", "url", hookURL, "error", err)
		return err
	}

	client := &http.Client{
		// No global timeout — we rely on the context timeout above.
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("hook: call failed",
			"url", hookURL,
			"error", err,
			"request_id", params["request_id"],
		)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Warn("hook: bad status",
			"url", hookURL,
			"status", resp.StatusCode,
			"request_id", params["request_id"],
		)
	}

	return nil
}

// CallHookAsync fires the hook in a background goroutine (fire-and-forget).
// Errors are logged but never block the caller.
func CallHookAsync(ctx context.Context, hookURL string, params map[string]string) {
	if hookURL == "" {
		return
	}
	go func() {
		// Use a detached context so the hook call can finish even if the parent
		// request context is already done — but still enforce the hook timeout.
		_ = CallHook(context.Background(), hookURL, params)
	}()
}

// firstNonEmpty returns the first non-empty string, or "" if both are empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
