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

import "github.com/wlame/rx-go/internal/config"

// defaultWindowLines is the compiled-in fallback when no CLI flag,
// request param, or env var supplies a value.
const defaultWindowLines = config.DefaultAnalyzeWindowLines

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
	// Then the env var, which config reads by the rule every integer
	// setting follows: a value that is not a whole number or is below 1
	// gives the default, one above maxWindowLines gives maxWindowLines,
	// each with one warning per process.
	return config.AnalyzeWindowLines()
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
