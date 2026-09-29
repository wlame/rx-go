package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/analyzer"
)

// analyzedFixture builds an analyzed index of a small file with one
// detector at the given version and window.
func analyzedFixture(t *testing.T, version string, window int) BuildOptions {
	t.Helper()
	return BuildOptions{
		Analyze:     true,
		WindowLines: window,
		Detectors:   []analyzer.LineDetector{&cueDetector{name: "cue", cue: "ERR", version: version}},
	}
}

func writeReuseFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reuse.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("ok\nERR boom\n", 50)), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestBuild_StampsTheAnalysisWindowAndDetectorSet(t *testing.T) {
	path := writeReuseFixture(t)

	idx, err := Build(path, analyzedFixture(t, "1.0.0", 100))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if idx.AnalysisWindowLines == nil || *idx.AnalysisWindowLines != 100 {
		t.Errorf("analysis_window_lines = %v, want 100", idx.AnalysisWindowLines)
	}
	if idx.AnalysisDetectorSet == nil || *idx.AnalysisDetectorSet != "cue@1.0.0" {
		t.Errorf("analysis_detector_set = %v, want cue@1.0.0", idx.AnalysisDetectorSet)
	}
}

func TestBuild_WithoutAnalysisLeavesTheAnalysisKeyNull(t *testing.T) {
	path := writeReuseFixture(t)

	idx, err := Build(path, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if idx.AnalysisWindowLines != nil || idx.AnalysisDetectorSet != nil {
		t.Errorf("analysis key = %v / %v, want both nil", idx.AnalysisWindowLines, idx.AnalysisDetectorSet)
	}
}

func TestSatisfiesBuild_ReusesAnAnalysisOnlyForTheSameWindowAndDetectors(t *testing.T) {
	path := writeReuseFixture(t)
	cached, err := Build(path, analyzedFixture(t, "1.0.0", 100))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	plain, err := Build(path, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	cases := []struct {
		name    string
		cached  bool // true: the analyzed index, false: the plain one
		request BuildOptions
		want    bool
	}{
		{"same window and detectors", true, analyzedFixture(t, "1.0.0", 100), true},
		{"another window", true, analyzedFixture(t, "1.0.0", 200), false},
		{"another detector version", true, analyzedFixture(t, "1.1.0", 100), false},
		{"no analysis asked", true, BuildOptions{}, true},
		{"analysis asked of a plain index", false, analyzedFixture(t, "1.0.0", 100), false},
		{"no analysis asked of a plain index", false, BuildOptions{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := plain
			if tc.cached {
				idx = cached
			}
			if got := SatisfiesBuild(idx, tc.request); got != tc.want {
				t.Errorf("SatisfiesBuild = %v, want %v", got, tc.want)
			}
		})
	}
}

// An analysis written before the index recorded its key cannot be
// shown to match any request, so it is never reused for one.
func TestSatisfiesBuild_AnAnalysisWithoutItsKeyIsNotReused(t *testing.T) {
	path := writeReuseFixture(t)
	idx, err := Build(path, analyzedFixture(t, "1.0.0", 100))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	idx.AnalysisWindowLines = nil
	idx.AnalysisDetectorSet = nil

	if SatisfiesBuild(idx, analyzedFixture(t, "1.0.0", 100)) {
		t.Error("an analysis without its window and detector set was reused")
	}
}
