package webapi

import (
	"log/slog"
	"strings"
	"sync/atomic"
)

// Package-private coupling between the serve command and /health's
// LOG_LEVEL reporting. Before this indirection, /health simply echoed
// RX_LOG_LEVEL, which meant an operator who set the level
// programmatically (via slog.SetDefault or a custom handler) would see
// a stale value in /health.
//
// Now the wiring is:
//
//  1. On serve startup, SetRequestedLogLevel is called with whatever
//     level the operator requested (from flags, env var, or default).
//  2. /health reads the latest value via requestedLogLevelName.
//  3. If a test or advanced embedder mutates the level at runtime,
//     they call SetRequestedLogLevel to keep /health honest.
//
// This is a minimal fix for Stage 8 Reviewer 3 High #13: the previous
// implementation was a Python-style "read env var" shortcut that
// ignored runtime state changes.

// requestedLogLevel stores the level the user asked for. Stored as an
// atomic pointer so Set/Get can happen concurrently without locks.
// nil pointer means "unset — fall back to env var behavior".
var requestedLogLevel atomic.Pointer[slog.Level]

// SetRequestedLogLevel records the level the slog.Default() handler was
// configured with. The server command (cmd/rx serve) calls this
// during startup so /health can report accurately.
//
// NIL-SENTINEL SEMANTIC: calling SetRequestedLogLevel(nil) does NOT
// "clear" or "unset" the level in a way that silences /health. It
// restores the original env-var-fallback behavior: subsequent calls
// to requestedLogLevelName read RX_LOG_LEVEL from the environment (or
// default "INFO" when unset). Tests that need to isolate state between
// cases rely on this to reset without touching os.Setenv. Production
// callers should either call this helper exactly once at startup with
// a non-nil pointer, or not at all — mixing a mid-flight nil store
// with non-env level changes will confuse operators reading /health.
//
// Exported so cmd/rx and tests can wire it from the outside.
func SetRequestedLogLevel(level *slog.Level) {
	requestedLogLevel.Store(level)
}

// requestedLogLevelName returns the level as a Python-style string
// ("DEBUG" | "INFO" | "WARN" | "ERROR"). If SetRequestedLogLevel has
// never been called, falls back to reading RX_LOG_LEVEL directly —
// preserving the legacy behavior for callers that haven't wired the
// level through yet.
func requestedLogLevelName(envFallback string) string {
	ptr := requestedLogLevel.Load()
	if ptr == nil {
		// No explicit level configured — surface whatever RX_LOG_LEVEL
		// reports. The caller passes the env value so this helper
		// stays test-friendly (no os.Getenv coupling here).
		if envFallback == "" {
			return "INFO"
		}
		return strings.ToUpper(envFallback)
	}
	switch {
	case *ptr <= slog.LevelDebug:
		return "DEBUG"
	case *ptr <= slog.LevelInfo:
		return "INFO"
	case *ptr <= slog.LevelWarn:
		return "WARN"
	default:
		return "ERROR"
	}
}
