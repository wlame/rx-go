package analyzer

// NoopAnalyzer supplies the FileAnalyzer metadata for the test
// detectors in this package, which embed it and add OnLine and
// Finalize.
type NoopAnalyzer struct {
	NameValue    string
	VersionValue string
}

// Name returns the configured identifier.
func (a NoopAnalyzer) Name() string { return a.NameValue }

// Version returns the configured semver string.
func (a NoopAnalyzer) Version() string { return a.VersionValue }

// Category is always "noop".
func (a NoopAnalyzer) Category() string { return "noop" }

// Description is a fixed human string.
func (a NoopAnalyzer) Description() string { return "no-op detector for tests" }
