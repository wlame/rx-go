package analyzer

import (
	"sync/atomic"
)

// Package-local registry state. Populated at package init() time across
// the process; frozen at main() start; never mutated after.
var (
	analyzers []FileAnalyzer
	frozen    atomic.Bool
)

// Register adds an analyzer to the global registry. MUST be called
// before Freeze — typically from an init() block.
//
// Panics if called after Freeze. This catches misuse at development
// time; in production, Freeze runs before any goroutine that might
// call Register.
//
// No mutex: the pre-Freeze window is single-goroutine by Go's package
// init semantics, so racy Register calls are impossible. This is the
// key property user decision 6.9.4 rests on: readers can skip the
// mutex because writers can't happen after Freeze.
func Register(a FileAnalyzer) {
	if frozen.Load() {
		panic("analyzer.Register called after Freeze: analyzers must register during package init()")
	}
	analyzers = append(analyzers, a)
}

// Freeze locks the registry. After Freeze returns, Register panics.
// Reads are lock-free from this point on.
//
// Freeze is idempotent — calling it twice is a no-op.
func Freeze() {
	frozen.Store(true)
}

// IsFrozen reports whether Freeze has been called. Tests use this to
// restore a clean state between cases.
func IsFrozen() bool {
	return frozen.Load()
}

// unfreezeForTest resets the registry to the pre-Freeze state.
// ONLY tests in this package may call this — production code must
// treat the registry as single-use. The function is unexported so
// external packages can't accidentally escape the Freeze contract.
func unfreezeForTest() {
	frozen.Store(false)
	analyzers = nil
}

// ApplicableFor walks the registry and returns every analyzer whose
// Supports returns true for the given input. Caller retains ordering
// (registration order) so results are deterministic.
//
// Lock-free read: plain slice iteration. Safe after Freeze because the
// slice header can't change and the Supports method is required to be
// safe for concurrent calls.
func ApplicableFor(input Input) []FileAnalyzer {
	out := make([]FileAnalyzer, 0, len(analyzers))
	for _, a := range analyzers {
		if a.Supports(input.Path, input.MimeHint, input.FileSize) {
			out = append(out, a)
		}
	}
	return out
}

// Snapshot returns a copy of the registry — useful for producing the
// /v1/detectors response.
func Snapshot() []FileAnalyzer {
	out := make([]FileAnalyzer, len(analyzers))
	copy(out, analyzers)
	return out
}

// Len returns how many analyzers are registered. Tests and the
// /v1/detectors handler both use this.
func Len() int {
	return len(analyzers)
}
