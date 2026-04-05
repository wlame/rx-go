package analyze

import (
	"fmt"
	"sync"
)

// Detector is the interface that all anomaly detectors must implement.
//
// Each detector focuses on a single type of anomaly (e.g. stack traces,
// error keywords, format deviations). Detectors are stateless per-call:
// the FileAnalyzer provides all context via LineContext.
//
// To register a detector, call Register(d) at init time. The registry is
// global and thread-safe.
type Detector interface {
	// Name returns a unique identifier for this detector (e.g. "traceback", "error_keyword").
	Name() string

	// Category returns the anomaly category (e.g. "error", "format", "security", "timing").
	Category() string

	// Description returns a human-readable explanation of what this detector finds.
	Description() string

	// SeverityRange returns the minimum and maximum severity scores this detector
	// can produce. Used for API metadata and documentation.
	SeverityRange() (min, max float64)

	// CheckLine examines the current line and returns a severity score.
	// Return -1 if the line is not anomalous. Return a value in [0, 1] to
	// indicate the anomaly severity (0 = informational, 1 = critical).
	CheckLine(ctx LineContext) float64

	// ShouldMerge returns true if the current line should be merged with the
	// previous anomaly from this same detector. This enables multi-line anomalies
	// such as stack traces. prevSeverity is the severity of the previous line.
	ShouldMerge(ctx LineContext, prevSeverity float64) bool

	// PrescanPatterns returns regex patterns for ripgrep-based prescan optimization.
	// Return nil if this detector does not support prescan. Prescan runs ripgrep
	// over the file first to identify regions of interest before the full scan.
	PrescanPatterns() []PrescanPattern
}

// registry holds all registered detectors, keyed by Name().
// Access is protected by mu for concurrent safety.
var (
	mu       sync.RWMutex
	registry = make(map[string]Detector)
)

// Register adds a detector to the global registry. If a detector with the same
// Name() is already registered, Register returns an error (it does NOT overwrite).
// Typically called from init() functions in detector implementation packages.
func Register(d Detector) error {
	mu.Lock()
	defer mu.Unlock()

	name := d.Name()
	if _, exists := registry[name]; exists {
		return fmt.Errorf("detector %q is already registered", name)
	}
	registry[name] = d
	return nil
}

// List returns all registered detectors in no particular order.
// The returned slice is a snapshot -- callers may iterate freely.
func List() []Detector {
	mu.RLock()
	defer mu.RUnlock()

	out := make([]Detector, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	return out
}

// Get looks up a detector by name. Returns the detector and true if found,
// or (nil, false) if no detector with that name is registered.
func Get(name string) (Detector, bool) {
	mu.RLock()
	defer mu.RUnlock()

	d, ok := registry[name]
	return d, ok
}

// resetRegistry clears all registered detectors. Exported only for testing --
// production code should never call this.
func resetRegistry() {
	mu.Lock()
	defer mu.Unlock()
	registry = make(map[string]Detector)
}
