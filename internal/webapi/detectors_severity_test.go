package webapi

import (
	"testing"

	"github.com/wlame/rx-go/internal/analyzer"
)

// fakeDetector is the least a FileAnalyzer needs to be listed.
type fakeDetector struct{ name string }

func (f fakeDetector) Name() string        { return f.name }
func (f fakeDetector) Version() string     { return "0.0.0" }
func (f fakeDetector) Category() string    { return "format" }
func (f fakeDetector) Description() string { return "a fake" }

// rangedDetector also states the severities its anomalies carry.
type rangedDetector struct {
	fakeDetector
	min, max float64
}

func (r rangedDetector) SeverityRange() (float64, float64) { return r.min, r.max }

// A client scales its severity indicator by severity_range before any
// anomaly exists, so the range has to be the detector's real one. A
// detector that does not state one is reported as the full scale.
func TestDetectorsResponse_ReportsEachDetectorsOwnSeverityRange(t *testing.T) {
	registered := []analyzer.FileAnalyzer{
		rangedDetector{fakeDetector{"secrets-scan"}, 1.0, 1.0},
		rangedDetector{fakeDetector{"long-line"}, 0.3, 0.3},
		fakeDetector{"unranged"},
	}

	resp := detectorsResponseFrom(registered)

	want := map[string][2]float64{
		"secrets-scan": {1.0, 1.0},
		"long-line":    {0.3, 0.3},
		"unranged":     {0.0, 1.0},
	}
	for _, d := range resp.Detectors {
		got := [2]float64{d.SeverityRange.Min, d.SeverityRange.Max}
		if got != want[d.Name] {
			t.Errorf("%s severity_range = %v, want %v", d.Name, got, want[d.Name])
		}
	}
	if len(resp.Detectors) != len(want) {
		t.Errorf("got %d detectors, want %d", len(resp.Detectors), len(want))
	}
}
