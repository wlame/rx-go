package analyzer

// This file implements the single resolver that turns the user-visible
// analyzer-window-lines configuration into a concrete int used by the
// coordinator. Centralizing the precedence logic here keeps the CLI,
// HTTP handler, and any programmatic callers consistent.
//
// Precedence (highest wins):
//
//  1. URL query / request-body parameter (urlParam)
//  2. CLI flag (cliFlag)
//  3. Environment variable RX_ANALYZE_WINDOW_LINES
//  4. Compiled-in default (defaultWindowLines)
//
// A value of 0 from either cliFlag or urlParam means "not set, fall
// through" — the natural default for Go's zero-valued flag fields and
// JSON-omitempty integers. Negative values are also treated as "not
// set" since they cannot represent a valid window size.
//
// The final value is clamped to [1, maxWindowLines]: maxWindowLines is
// declared in window.go and bounds the fixed-size array inside Window.
// Anything larger would be silently truncated inside NewWindow anyway;
// clamping here gives callers a consistent, observable result.

import (
	"os"
	"strconv"
)

// defaultWindowLines is the compiled-in fallback when no CLI flag,
// request param, or env var supplies a value. Chosen large enough to
// cover multi-line tracebacks and small JSON blobs, small enough that
// per-worker memory (size * slot overhead) is negligible.
const defaultWindowLines = 128

// envWindowLinesVar is the name of the environment variable read by
// envWindowLines. Exported as a constant so tests and docs can refer
// to a single source of truth for the name.
const envWindowLinesVar = "RX_ANALYZE_WINDOW_LINES"

// ResolveWindowLines returns the effective window size for the
// coordinator, applying the documented precedence and clamping.
//
// cliFlag and urlParam use <= 0 as the "not set" sentinel. Typical
// wiring:
//
//	size := analyzer.ResolveWindowLines(opts.CLIWindowLines, req.URLWindowLines)
//	coord := analyzer.NewCoordinator(size, detectors)
//
// The return value is always in [1, maxWindowLines].
func ResolveWindowLines(cliFlag, urlParam int) int {
	// URL param wins if it's a real positive value.
	if urlParam > 0 {
		return clampWindowLines(urlParam)
	}
	// Then the CLI flag.
	if cliFlag > 0 {
		return clampWindowLines(cliFlag)
	}
	// Then the env var. Invalid values (non-integer, zero, negative)
	// are silently ignored and we fall through to the default —
	// startup-time config errors shouldn't kill the process.
	if v, ok := envWindowLines(); ok {
		return clampWindowLines(v)
	}
	return defaultWindowLines
}

// envWindowLines reads RX_ANALYZE_WINDOW_LINES and returns its parsed
// integer value. The second return is false when the variable is
// unset, empty, or cannot be parsed as a positive integer — callers
// should fall back to the next precedence layer.
//
// Separated from ResolveWindowLines so the env-parsing branch can be
// unit-tested in isolation.
func envWindowLines() (int, bool) {
	raw, ok := os.LookupEnv(envWindowLinesVar)
	if !ok || raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	// Only accept positive values from the env. Zero and negatives are
	// treated as "not set" so a misconfigured env doesn't silently
	// clamp to 1 and hide the problem from the user.
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// clampWindowLines squeezes v into the legal range [1, maxWindowLines].
// Callers outside this file typically use ResolveWindowLines which
// applies this implicitly; exposed here as a small helper for tests.
func clampWindowLines(v int) int {
	if v < 1 {
		return 1
	}
	if v > maxWindowLines {
		return maxWindowLines
	}
	return v
}
