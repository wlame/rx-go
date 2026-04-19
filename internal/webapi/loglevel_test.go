package webapi

import (
	"log/slog"
	"testing"
)

// TestRequestedLogLevelName_EnvFallbackWhenUnset covers the pre-fix
// behavior preserved as a fallback: when SetRequestedLogLevel has not
// been called (no explicit wiring), /health still surfaces the
// RX_LOG_LEVEL value the caller passes in. Tests pass their own env
// value to avoid global state coupling.
func TestRequestedLogLevelName_EnvFallbackWhenUnset(t *testing.T) {
	// Ensure a clean slate — this is important because the package
	// global may have been set by another test running earlier.
	SetRequestedLogLevel(nil)
	t.Cleanup(func() { SetRequestedLogLevel(nil) })

	cases := []struct {
		env  string
		want string
	}{
		{"", "INFO"},           // unset → default
		{"DEBUG", "DEBUG"},     // explicit debug
		{"debug", "DEBUG"},     // lowercase uppercased
		{"warning", "WARNING"}, // pass-through (Python-compat quirk: uppercased but unchanged)
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			got := requestedLogLevelName(tc.env)
			if got != tc.want {
				t.Errorf("requestedLogLevelName(%q) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// TestRequestedLogLevelName_ReflectsSetValue covers Stage 8 Reviewer 3
// High #13: when the server wires the actual configured log level via
// SetRequestedLogLevel, /health reports THAT value rather than an
// env-var echo that may be out of date.
func TestRequestedLogLevelName_ReflectsSetValue(t *testing.T) {
	SetRequestedLogLevel(nil)
	t.Cleanup(func() { SetRequestedLogLevel(nil) })

	cases := []struct {
		name  string
		level slog.Level
		want  string
	}{
		{"debug", slog.LevelDebug, "DEBUG"},
		{"info", slog.LevelInfo, "INFO"},
		{"warn", slog.LevelWarn, "WARN"},
		{"error", slog.LevelError, "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lvl := tc.level
			SetRequestedLogLevel(&lvl)

			// envFallback is now ignored because the level is explicitly
			// set. Pass a bogus value to prove the set wins.
			got := requestedLogLevelName("TOTALLY_WRONG")
			if got != tc.want {
				t.Errorf("level=%v: got %q, want %q (envFallback should be ignored when SetRequestedLogLevel was called)",
					tc.level, got, tc.want)
			}
		})
	}
}

// TestGetLogLevelName_UsesConfiguredLevel is the integration-level test:
// it confirms the /health handler (via getLogLevelName) routes through
// requestedLogLevelName so a programmatically-configured level is
// visible.
//
// Pre-fix getLogLevelName read os.Getenv("RX_LOG_LEVEL") directly,
// ignoring any programmatic slog configuration. Post-fix it consults
// the package-level requested-level pointer first.
func TestGetLogLevelName_UsesConfiguredLevel(t *testing.T) {
	SetRequestedLogLevel(nil)
	t.Cleanup(func() { SetRequestedLogLevel(nil) })

	// Set a programmatic level that does NOT match the env var.
	t.Setenv("RX_LOG_LEVEL", "INFO")
	level := slog.LevelDebug
	SetRequestedLogLevel(&level)

	got := getLogLevelName()
	if got != "DEBUG" {
		t.Errorf("getLogLevelName = %q, want DEBUG (env says INFO but programmatic level is DEBUG)", got)
	}
}
