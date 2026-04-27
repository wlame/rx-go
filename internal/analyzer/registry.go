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
	//     It holds one FileAnalyzer per registered detector (shared, read-
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
//
// NOTE: line-oriented detectors should use RegisterLineDetector instead,
// which registers a factory so each build gets a fresh instance.
// Register is kept for metadata-only FileAnalyzers (no streaming state).
func Register(a FileAnalyzer) {
	if frozen.Load() {
		panic("analyzer.Register called after Freeze: analyzers must register during package init()")
	}
	analyzers = append(analyzers, a)
}

// RegisterLineDetector registers a line detector by factory. The factory
// is called once per index.Build to produce a fresh LineDetector — this
// is how we guarantee per-build state isolation (finding 6 of the
// analyzers review: shared instances leak streaming state across files).
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
// Panics if called after Freeze, same as Register.
func RegisterLineDetector(factory LineDetectorFactory) {
	if frozen.Load() {
		panic("analyzer.RegisterLineDetector called after Freeze: detectors must register during package init()")
	}
	// Call factory once to get a metadata prototype. This instance is only
	// used for its Name/Version/Category/Description/Supports methods —
	// its streaming state (if any) is never exercised.
	proto := factory()
	analyzers = append(analyzers, proto)
	lineDetectorFactories = append(lineDetectorFactories, factory)
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
	lineDetectorFactories = nil
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
