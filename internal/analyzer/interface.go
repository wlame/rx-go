// Package analyzer hosts the detector registry. Every detector is a
// LineDetector registered by factory (RegisterLineDetector); the index
// builder drives a fresh set of them through a Coordinator during the
// one pass that also builds the line index, so a new detector is added
// without reshaping the core.
//
// Design points:
//
//   - Detectors register themselves from package init() via
//     RegisterLineDetector.
//   - main() calls Freeze() exactly once, before starting the HTTP
//     server. Freeze flips a latch; subsequent registrations PANIC.
//     After Freeze the registry is read-only and lock-free.
//   - Because registration happens only at startup (never in hot
//     paths), the registry uses plain slices + an atomic bool, not a
//     mutex. Reads do zero-cost indexing.
//   - The pre-Freeze window is a single goroutine (package init order),
//     so registration itself doesn't need a mutex either. We check
//     frozen on each call to catch misuse at development time.
//
// Contract summary:
//
//	func init() {
//	    analyzer.RegisterLineDetector(func() analyzer.LineDetector { return New() })
//	}
//	...
//	func main() {
//	    analyzer.Freeze()                 // call once, after init
//	    http.ListenAndServe(...)          // reads are lock-free
//	}
package analyzer

// FileAnalyzer is the metadata every detector reports: what
// /v1/detectors lists and what an analysis records. It has no method
// that inspects a file. The work is done by LineDetector's OnLine and
// Finalize, which the index builder drives through a Coordinator.
//
// All methods MUST be safe for concurrent calls: the registry keeps one
// prototype per detector and reads it from any goroutine.
type FileAnalyzer interface {
	// Name is the stable identifier reported in /v1/detectors, in each
	// anomaly's `detector` field and in an analysis's detector set.
	// Must be globally unique among registered detectors.
	Name() string

	// Version is a semver string. A cached analysis records the version
	// of every detector that made it, so a change here makes the next
	// analysis request rebuild it.
	Version() string

	// Category is a human-readable bucket name — "log-pattern",
	// "security", "format", etc. Reported by /v1/detectors.
	Category() string

	// Description is a human-readable sentence shown in /v1/detectors.
	Description() string
}

// SeverityRanger is implemented by an analyzer that can state the band
// of severities its anomalies carry, so /v1/detectors can publish the
// band before any anomaly exists and a client can scale its indicator
// by it. An analyzer without it is reported as the full 0..1 scale.
type SeverityRanger interface {
	// SeverityRange returns the lowest and highest severity the
	// analyzer's anomalies can have, each within 0..1.
	SeverityRange() (lowest, highest float64)
}

// Anomaly is one flagged line range.
//
// Two name-like fields:
//
//   - DetectorName is the producing detector's Name() — stamped by the
//     coordinator's Finalize before the anomaly leaves the worker.
//     This is what cross-worker Deduplicate keys on and what the wire
//     contract's `detector` field reports.
//
//   - Category is the SEMANTIC bucket the detector chose (e.g.
//     "log-traceback", "secrets", "format"). Multiple detectors can
//     share a category. Never overwritten by the coordinator — the
//     category taxonomy is exposed via /v1/detectors so UIs can group
//     findings across detectors.
type Anomaly struct {
	StartLine    int64   `json:"start_line"`
	EndLine      int64   `json:"end_line"`
	StartOffset  int64   `json:"start_offset"`
	EndOffset    int64   `json:"end_offset"`
	Severity     float64 `json:"severity"`
	Category     string  `json:"category"`
	Description  string  `json:"description"`
	DetectorName string  `json:"detector_name,omitempty"`
}
