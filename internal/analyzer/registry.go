package analyzer

import (
	"sync/atomic"
)

// Package-local registry state. Populated at package init() time across
// the process; frozen at main() start; never mutated after.
var (
	analyzers []FileAnalyzer

	// lineDetectorFactories holds per-build "make me a fresh instance"
	// functions for every registered line detector. Keeping these separate
	// from the metadata-oriented analyzers slice is deliberate:
	//
	//   - analyzers[] is the metadata registry that /v1/detectors iterates.
	//     It holds one prototype per registered detector (shared, read-
	//     only — only its metadata methods are ever called).
	//
	//   - lineDetectorFactories is what the index builder calls to get a
	//     fresh LineDetector per scan. Each factory call returns a new
	//     instance whose streaming state (open runs, buffers, hashes) can
	//     safely accumulate without leaking to the next build.
	//
	// Both slices are populated in the same init() call (RegisterLineDetector)
	// so their orderings stay in lockstep.
	lineDetectorFactories []LineDetectorFactory

	frozen atomic.Bool
)

// LineDetectorFactory produces a fresh LineDetector instance. The factory
// is invoked by LineDetectorSnapshot once per call (i.e. once per build)
// so each build gets its own independent state.
type LineDetectorFactory func() LineDetector

// RegisterLineDetector registers a line detector by factory. It is the
// only way to add a detector, so every detector /v1/detectors lists is
// one the index builder runs.
//
// The factory is called once per index.Build to produce a fresh
// LineDetector. That is how each build gets its own streaming state: a
// shared instance would carry open runs, buffers and hashes from one
// file into the next.
//
// No mutex: the pre-Freeze window is single-goroutine by Go's package
// init semantics, so racy registrations are impossible. This is the
// property the rule rests on: readers can skip the mutex because
// writers can't happen after Freeze.
//
// The factory is invoked once immediately to register a prototype in the
// overall analyzers slice so metadata (Name/Version/Category/Description)
// surfaces in /v1/detectors.
//
// Typical init() pattern:
//
//	func init() {
//	    analyzer.RegisterLineDetector(func() analyzer.LineDetector { return New() })
//	}
//
// Panics if called after Freeze. This catches misuse at development
// time; in production, Freeze runs before any goroutine that might
// register.
func RegisterLineDetector(factory LineDetectorFactory) {
	if frozen.Load() {
		panic("analyzer.RegisterLineDetector called after Freeze: detectors must register during package init()")
	}
	// Call factory once to get a metadata prototype. This instance is only
	// used for its Name/Version/Category/Description methods — its
	// streaming state (if any) is never exercised.
	proto := factory()
	analyzers = append(analyzers, proto)
	lineDetectorFactories = append(lineDetectorFactories, factory)
}

// Freeze locks the registry. After Freeze returns, RegisterLineDetector
// panics.
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
	lineDetectorFactories = nil
}

// Snapshot returns a copy of the registry — useful for producing the
// /v1/detectors response.
func Snapshot() []FileAnalyzer {
	out := make([]FileAnalyzer, len(analyzers))
	copy(out, analyzers)
	return out
}

// LineDetectorSnapshot returns a freshly-instantiated LineDetector for
// every registered line-detector factory. Each call produces brand-new
// instances, so the returned slice is safe to hand to NewCoordinator
// without worrying about state from a previous build.
//
// Ordering matches registration order, which matches the order in the
// analyzers slice. Safe to call after Freeze (factories are read-only
// at that point).
func LineDetectorSnapshot() []LineDetector {
	out := make([]LineDetector, len(lineDetectorFactories))
	for i, factory := range lineDetectorFactories {
		out[i] = factory()
	}
	return out
}

// Len returns how many analyzers are registered. Tests and the
// /v1/detectors handler both use this.
func Len() int {
	return len(analyzers)
}
