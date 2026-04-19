package analyzer

import "context"

// NoopAnalyzer is a concrete FileAnalyzer used ONLY by contract tests.
// It's not auto-registered; test code imports it and calls Register
// explicitly to verify the registry machinery. Production ships with
// zero analyzers — see user-instructions.md.
type NoopAnalyzer struct {
	NameValue    string
	VersionValue string
}

// Name returns the configured identifier.
func (a NoopAnalyzer) Name() string { return a.NameValue }

// Version returns the configured semver string.
func (a NoopAnalyzer) Version() string { return a.VersionValue }

// Category is always "noop" for this placeholder.
func (a NoopAnalyzer) Category() string { return "noop" }

// Description is a fixed human string.
func (a NoopAnalyzer) Description() string { return "no-op analyzer for contract tests" }

// Supports always returns true — contract tests wire up this analyzer
// specifically because they want Supports to say yes.
func (a NoopAnalyzer) Supports(_ string, _ string, _ int64) bool { return true }

// Analyze returns an empty report, never an error.
func (a NoopAnalyzer) Analyze(_ context.Context, _ Input) (*Report, error) {
	return &Report{
		Name:          a.NameValue,
		Version:       a.VersionValue,
		SchemaVersion: 1,
		Result:        map[string]any{},
	}, nil
}
