// Package analyzer hosts the pluggable file-analyzer registry. rx-go
// ships WITHOUT any built-in analyzers at v1 (per user instructions);
// the registry and interface are defined now so analyzers can be added
// one at a time in future releases without reshaping the core.
//
// Design points (from spec §7 and decision 6.9.4):
//
//   - Analyzers register themselves from package init() via Register().
//   - main() calls Freeze() exactly once, before starting the HTTP
//     server. Freeze flips a latch; subsequent Register calls PANIC.
//     After Freeze the registry is read-only and lock-free.
//   - Because Register happens only at startup (never in hot paths),
//     the registry uses a plain slice + atomic bool, not a mutex.
//     Reads do zero-cost indexing.
//   - The pre-Freeze window is a single goroutine (package init order),
//     so Register itself doesn't need a mutex either. We check frozen
//     on each call to catch misuse at development time.
//
// Contract summary:
//
//	func init() {
//	    analyzer.Register(myAnalyzer{})  // allowed
//	}
//	...
//	func main() {
//	    analyzer.Freeze()                 // call once, after init
//	    http.ListenAndServe(...)          // reads are lock-free
//	}
package analyzer

import "context"

// FileAnalyzer is the plug-in contract. All methods MUST be safe for
// concurrent calls; the engine may run Analyze on multiple files in
// parallel.
type FileAnalyzer interface {
	// Name is the stable identifier used in cache keys, the detectors
	// endpoint, and the analyzer namespace (decision 5.3). Must be
	// globally unique among registered analyzers.
	Name() string

	// Version is a semver string. When it changes, the analyzer's
	// cached output is invalidated (cache keys include the version
	// segment per decision 5.3).
	Version() string

	// Category is a human-readable bucket name — "log-pattern",
	// "security", "format", etc. Reported by /v1/detectors.
	Category() string

	// Description is a human-readable sentence shown in /v1/detectors.
	Description() string

	// Supports decides whether Analyze should run for the given file.
	// Called once per file before Analyze. fileSize lets analyzers opt
	// out of files that are too large for their algorithm.
	Supports(path string, mimeHint string, fileSize int64) bool

	// Analyze inspects the file and returns a Report. The Report body
	// is analyzer-specific JSON; the framework only guarantees the
	// envelope.
	Analyze(ctx context.Context, input Input) (*Report, error)
}

// Input is the handoff from the engine to the analyzer.
//
// Named Input (not AnalyzerInput) to avoid stutter at the call site:
// `analyzer.Input` reads better than `analyzer.AnalyzerInput`.
type Input struct {
	// Path is the absolute path to the file being analyzed.
	Path string

	// MimeHint is a best-effort MIME-like string ("text/plain", etc.)
	// determined by the engine. Analyzers may ignore or cross-check.
	MimeHint string

	// FileSize is os.Stat's Size() for Path.
	FileSize int64
}

// Report is the unit of analyzer output.
//
// The Result field is free-form — analyzers define their own schema
// and record the schema version via SchemaVersion so callers can
// migrate when a report changes shape.
type Report struct {
	// Name matches the producing FileAnalyzer.Name().
	Name string `json:"name"`

	// Version matches the producing FileAnalyzer.Version().
	Version string `json:"version"`

	// SchemaVersion increments whenever Result's shape changes. Gives
	// readers (and the frontend) a cheap compatibility check.
	SchemaVersion int `json:"schema_version"`

	// Result is the analyzer-specific payload. Kept as a map to avoid
	// needing a dependency on any concrete analyzer package.
	Result map[string]any `json:"result"`

	// Anomalies are an optional flat list of anomalies; populated when
	// the analyzer produces them in a frontend-compatible shape.
	// Otherwise nil.
	Anomalies []Anomaly `json:"anomalies,omitempty"`
}

// Anomaly is one flagged line range.
type Anomaly struct {
	StartLine   int64   `json:"start_line"`
	EndLine     int64   `json:"end_line"`
	StartOffset int64   `json:"start_offset"`
	EndOffset   int64   `json:"end_offset"`
	Severity    float64 `json:"severity"`
	Category    string  `json:"category"`
	Description string  `json:"description"`
}
