package analyzer

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// namesOf lists the names of a set of detectors in order.
func namesOf[T interface{ Name() string }](detectors []T) []string {
	out := make([]string, len(detectors))
	for i, d := range detectors {
		out[i] = d.Name()
	}
	return out
}

// Every detector the registry lists in /v1/detectors is one the index
// builder runs: the registration call fills both from one factory.
func TestRegistry_EveryListedDetectorIsRun(t *testing.T) {
	resetRegistry(t)
	for _, name := range []string{"first", "second"} {
		RegisterLineDetector(func() LineDetector { return newTrackingDetector(name) })
	}
	Freeze()

	listed, run := namesOf(Snapshot()), namesOf(LineDetectorSnapshot())

	if !slices.Equal(listed, run) || len(listed) != 2 {
		t.Errorf("listed %v, run %v; want the same two detectors", listed, run)
	}
}

// The analyzers page tells a developer how to add a detector. The call
// it names must be the one whose detectors run.
func TestAnalyzersDoc_NamesTheRegistrationThatRuns(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "concepts", "analyzers.md"))
	if err != nil {
		t.Fatalf("read analyzers.md: %v", err)
	}
	doc := string(raw)

	if !strings.Contains(doc, "analyzer.RegisterLineDetector(") {
		t.Error("analyzers.md does not name analyzer.RegisterLineDetector")
	}
	if strings.Contains(doc, "analyzer.Register(") {
		t.Error("analyzers.md names analyzer.Register, whose detectors are listed and never run")
	}
}
