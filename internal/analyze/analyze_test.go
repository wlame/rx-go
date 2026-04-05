package analyze

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// stubDetector implements Detector for testing purposes.
// ---------------------------------------------------------------------------

// stubDetector is a minimal Detector that flags lines containing a keyword.
type stubDetector struct {
	name     string
	category string
	desc     string
	minSev   float64
	maxSev   float64
	keyword  string        // if line contains this, it is anomalous
	severity float64       // severity returned when anomalous
	merge    bool          // whether ShouldMerge always returns true
	prescan  []PrescanPattern // prescan patterns to return
}

func (d *stubDetector) Name() string                          { return d.name }
func (d *stubDetector) Category() string                      { return d.category }
func (d *stubDetector) Description() string                   { return d.desc }
func (d *stubDetector) SeverityRange() (min, max float64)     { return d.minSev, d.maxSev }
func (d *stubDetector) PrescanPatterns() []PrescanPattern     { return d.prescan }
func (d *stubDetector) ShouldMerge(_ LineContext, _ float64) bool { return d.merge }

func (d *stubDetector) CheckLine(ctx LineContext) float64 {
	if d.keyword == "" {
		return -1
	}
	// Simple substring check for testing.
	for i := 0; i <= len(ctx.Line)-len(d.keyword); i++ {
		if ctx.Line[i:i+len(d.keyword)] == d.keyword {
			return d.severity
		}
	}
	return -1
}

// newStub creates a stubDetector with sensible defaults. Override fields after creation.
func newStub(name, keyword string, severity float64) *stubDetector {
	return &stubDetector{
		name:     name,
		category: "test",
		desc:     "stub detector for " + name,
		minSev:   0.0,
		maxSev:   1.0,
		keyword:  keyword,
		severity: severity,
	}
}

// ---------------------------------------------------------------------------
// Registry tests (7.2)
// ---------------------------------------------------------------------------

func TestRegister_SingleDetector(t *testing.T) {
	resetRegistry()

	d := newStub("test_detector", "ERROR", 0.7)
	err := Register(d)
	require.NoError(t, err)

	got, ok := Get("test_detector")
	assert.True(t, ok)
	assert.Equal(t, "test_detector", got.Name())
}

func TestRegister_MultipleDetectors(t *testing.T) {
	resetRegistry()

	d1 := newStub("alpha", "A", 0.5)
	d2 := newStub("beta", "B", 0.6)
	d3 := newStub("gamma", "C", 0.7)

	require.NoError(t, Register(d1))
	require.NoError(t, Register(d2))
	require.NoError(t, Register(d3))

	all := List()
	assert.Len(t, all, 3)

	// Verify each can be retrieved by name.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		_, ok := Get(name)
		assert.True(t, ok, "detector %q should be found", name)
	}
}

func TestRegister_DuplicateName_ReturnsError(t *testing.T) {
	resetRegistry()

	d1 := newStub("dup", "X", 0.5)
	d2 := newStub("dup", "Y", 0.6)

	require.NoError(t, Register(d1))

	err := Register(d2)
	assert.Error(t, err, "registering a duplicate name should fail")
	assert.Contains(t, err.Error(), "dup")

	// The original should still be there, unchanged.
	got, ok := Get("dup")
	assert.True(t, ok)
	assert.Equal(t, "X", got.(*stubDetector).keyword)
}

func TestGet_NotFound(t *testing.T) {
	resetRegistry()

	got, ok := Get("nonexistent")
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestList_EmptyRegistry(t *testing.T) {
	resetRegistry()

	all := List()
	assert.Empty(t, all)
}

// ---------------------------------------------------------------------------
// Detector interface tests (7.2)
// ---------------------------------------------------------------------------

func TestDetector_SeverityRange(t *testing.T) {
	d := &stubDetector{
		name:   "range_test",
		minSev: 0.3,
		maxSev: 0.8,
	}
	min, max := d.SeverityRange()
	assert.Equal(t, 0.3, min)
	assert.Equal(t, 0.8, max)
}

func TestDetector_PrescanPatterns_Nil(t *testing.T) {
	d := newStub("no_prescan", "", 0)
	assert.Nil(t, d.PrescanPatterns())
}

func TestDetector_PrescanPatterns_NonNil(t *testing.T) {
	d := &stubDetector{
		name: "with_prescan",
		prescan: []PrescanPattern{
			{Pattern: "ERROR|FATAL", DetectorName: "with_prescan"},
		},
	}
	patterns := d.PrescanPatterns()
	require.Len(t, patterns, 1)
	assert.Equal(t, "ERROR|FATAL", patterns[0].Pattern)
	assert.Equal(t, "with_prescan", patterns[0].DetectorName)
}

func TestDetector_CheckLine_NoKeyword(t *testing.T) {
	d := newStub("empty", "", 0.5)
	sev := d.CheckLine(LineContext{Line: "anything"})
	assert.Equal(t, float64(-1), sev)
}

func TestDetector_CheckLine_MatchesKeyword(t *testing.T) {
	d := newStub("err", "ERROR", 0.7)
	sev := d.CheckLine(LineContext{Line: "2024-01-01 ERROR something broke"})
	assert.Equal(t, 0.7, sev)
}

func TestDetector_CheckLine_NoMatch(t *testing.T) {
	d := newStub("err", "ERROR", 0.7)
	sev := d.CheckLine(LineContext{Line: "2024-01-01 INFO all good"})
	assert.Equal(t, float64(-1), sev)
}

// ---------------------------------------------------------------------------
// FileAnalyzer skeleton tests (7.3)
// ---------------------------------------------------------------------------

// createTestFile writes content to a temp file and returns the path.
func createTestFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte(content), 0o644)
	require.NoError(t, err)
	return path
}

func TestAnalyze_ZeroDetectors_ReturnsEmpty(t *testing.T) {
	path := createTestFile(t, "empty.log", "line one\nline two\nline three\n")

	result, err := Analyze(context.Background(), path, nil)
	require.NoError(t, err)

	assert.Equal(t, path, result.FilePath)
	assert.Empty(t, result.Anomalies)
	assert.Equal(t, 0, result.LinesScanned, "zero detectors means no scanning")
}

func TestAnalyze_EmptyDetectorSlice_ReturnsEmpty(t *testing.T) {
	path := createTestFile(t, "empty2.log", "data\n")

	result, err := Analyze(context.Background(), path, []Detector{})
	require.NoError(t, err)

	assert.Empty(t, result.Anomalies)
	assert.Equal(t, 0, result.LinesScanned)
}

func TestAnalyze_SingleDetector_FindsAnomalies(t *testing.T) {
	content := "INFO starting up\nERROR disk full\nINFO recovered\nERROR timeout\n"
	path := createTestFile(t, "app.log", content)

	d := newStub("error_kw", "ERROR", 0.7)
	result, err := Analyze(context.Background(), path, []Detector{d})
	require.NoError(t, err)

	assert.Equal(t, 4, result.LinesScanned)
	assert.Len(t, result.Anomalies, 2, "should find 2 ERROR lines")

	for _, a := range result.Anomalies {
		assert.Equal(t, "error_kw", a.Detector)
		assert.Equal(t, "test", a.Category)
		assert.Equal(t, 0.7, a.Severity)
		assert.Equal(t, a.StartLine, a.EndLine, "single-line anomalies")
	}
}

func TestAnalyze_MultipleDetectors(t *testing.T) {
	content := "ERROR critical\nWARNING minor\nINFO ok\n"
	path := createTestFile(t, "multi.log", content)

	errDet := newStub("error_kw", "ERROR", 0.8)
	warnDet := newStub("warn_kw", "WARNING", 0.4)
	result, err := Analyze(context.Background(), path, []Detector{errDet, warnDet})
	require.NoError(t, err)

	assert.Equal(t, 3, result.LinesScanned)
	assert.Len(t, result.Anomalies, 2, "one ERROR + one WARNING")

	// Verify each detector contributed one anomaly.
	detectors := map[string]bool{}
	for _, a := range result.Anomalies {
		detectors[a.Detector] = true
	}
	assert.True(t, detectors["error_kw"])
	assert.True(t, detectors["warn_kw"])
}

func TestAnalyze_MergeConsecutiveLines(t *testing.T) {
	// Three consecutive ERROR lines should merge into one range when merge=true.
	content := "ERROR line 1\nERROR line 2\nERROR line 3\nINFO ok\n"
	path := createTestFile(t, "merge.log", content)

	d := newStub("error_kw", "ERROR", 0.6)
	d.merge = true // enable merging

	result, err := Analyze(context.Background(), path, []Detector{d})
	require.NoError(t, err)

	assert.Equal(t, 4, result.LinesScanned)
	require.Len(t, result.Anomalies, 1, "consecutive lines should merge into one range")

	a := result.Anomalies[0]
	assert.Equal(t, 1, a.StartLine)
	assert.Equal(t, 3, a.EndLine)
	assert.Equal(t, 0.6, a.Severity)
}

func TestAnalyze_NoMerge_SeparateAnomalies(t *testing.T) {
	content := "ERROR one\nERROR two\n"
	path := createTestFile(t, "nomerge.log", content)

	d := newStub("error_kw", "ERROR", 0.5)
	d.merge = false // no merging

	result, err := Analyze(context.Background(), path, []Detector{d})
	require.NoError(t, err)

	assert.Len(t, result.Anomalies, 2, "without merge, each line is separate")
}

func TestAnalyze_EmptyFile(t *testing.T) {
	path := createTestFile(t, "empty.log", "")

	d := newStub("error_kw", "ERROR", 0.7)
	result, err := Analyze(context.Background(), path, []Detector{d})
	require.NoError(t, err)

	assert.Equal(t, 0, result.LinesScanned)
	assert.Empty(t, result.Anomalies)
}

func TestAnalyze_FileNotFound_ReturnsError(t *testing.T) {
	_, err := Analyze(context.Background(), "/nonexistent/file.log", []Detector{
		newStub("x", "X", 0.5),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "analyze")
}

func TestAnalyze_ContextCancelled_ReturnsError(t *testing.T) {
	// Create a file with enough lines so the scanner has work to do.
	var content string
	for i := 0; i < 100; i++ {
		content += "some log line\n"
	}
	path := createTestFile(t, "cancel.log", content)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := Analyze(ctx, path, []Detector{newStub("x", "X", 0.5)})
	assert.Error(t, err, "should respect cancelled context")
}

func TestAnalyze_RecordsDuration(t *testing.T) {
	path := createTestFile(t, "dur.log", "line\n")

	d := newStub("x", "NOPE", 0.5)
	result, err := Analyze(context.Background(), path, []Detector{d})
	require.NoError(t, err)

	assert.Greater(t, result.Duration.Nanoseconds(), int64(0),
		"duration should be positive")
}

func TestAnalyze_AnomaliesHaveNonNilSlice(t *testing.T) {
	// Even with no anomalies found, the slice should be non-nil (for JSON serialization).
	path := createTestFile(t, "clean.log", "all good\n")

	d := newStub("x", "NOPE", 0.5)
	result, err := Analyze(context.Background(), path, []Detector{d})
	require.NoError(t, err)

	assert.NotNil(t, result.Anomalies, "anomalies should be [] not nil")
	assert.Empty(t, result.Anomalies)
}

func TestAnalyze_MergeTakesMaxSeverity(t *testing.T) {
	// When merging, the range should keep the highest severity.
	content := "ERROR low\nERROR high\n"
	path := createTestFile(t, "maxsev.log", content)

	// A detector that returns different severities per line.
	d := &escalatingDetector{
		name:       "escalating",
		keyword:    "ERROR",
		severities: []float64{0.3, 0.9},
		callIdx:    0,
	}

	result, err := Analyze(context.Background(), path, []Detector{d})
	require.NoError(t, err)

	require.Len(t, result.Anomalies, 1)
	assert.Equal(t, 0.9, result.Anomalies[0].Severity, "merged severity should be max")
}

// escalatingDetector returns increasing severity on successive matches and always merges.
type escalatingDetector struct {
	name       string
	keyword    string
	severities []float64
	callIdx    int
}

func (d *escalatingDetector) Name() string                      { return d.name }
func (d *escalatingDetector) Category() string                  { return "test" }
func (d *escalatingDetector) Description() string               { return "escalating stub" }
func (d *escalatingDetector) SeverityRange() (float64, float64) { return 0, 1 }
func (d *escalatingDetector) ShouldMerge(_ LineContext, _ float64) bool { return true }
func (d *escalatingDetector) PrescanPatterns() []PrescanPattern { return nil }

func (d *escalatingDetector) CheckLine(ctx LineContext) float64 {
	for i := 0; i <= len(ctx.Line)-len(d.keyword); i++ {
		if ctx.Line[i:i+len(d.keyword)] == d.keyword {
			idx := d.callIdx
			d.callIdx++
			if idx < len(d.severities) {
				return d.severities[idx]
			}
			return d.severities[len(d.severities)-1]
		}
	}
	return -1
}
