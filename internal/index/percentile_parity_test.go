package index

import (
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

// Line-length percentiles are fields of the shared index format and of
// GET /v1/index, so the two backends have to agree about them. The
// definition is linear interpolation over the sorted sample, the one
// Python's statistics.quantiles(method="inclusive") uses, and it is
// written down in docs/concepts/analyzers.md.
//
// The fixture is shared: rx-python has the same file at
// tests/data/line-lengths.log and asserts the same numbers.

// The lengths in testdata/line-lengths.log. An even count, so the median
// falls between two samples; a long tail, so p95 and p99 interpolate
// rather than land on one; and a repeated value.
var fixtureLineLengths = []int{1, 2, 3, 5, 8, 13, 21, 34, 34, 55, 89, 144}

const (
	wantMax    = 144.0
	wantAvg    = 34.083333333333336
	wantMedian = 17.0
	wantP95    = 113.75
	wantP99    = 137.95
	wantStdDev = 43.44998866687442
)

func closeEnough(got, want float64) bool {
	if want == 0 {
		return math.Abs(got) < 1e-9
	}
	return math.Abs(got-want)/math.Abs(want) < 1e-9
}

// Below the reservoir cap every line is kept, so the sample is the whole
// file and the numbers are exact — not estimates.
func TestPercentiles_MatchTheSharedFixture(t *testing.T) {
	acc := newLineStatsAccumulator(0)
	for i, length := range fixtureLineLengths {
		acc.observe(length, false, i+1, 0)
	}
	got := acc.finish()

	cases := []struct {
		name      string
		got, want float64
	}{
		{"max", float64(got.Max), wantMax},
		{"avg", got.Mean, wantAvg},
		{"median", got.Median, wantMedian},
		{"p95", got.P95, wantP95},
		{"p99", got.P99, wantP99},
		{"stddev", got.StdDev, wantStdDev},
	}
	for _, tc := range cases {
		if !closeEnough(tc.got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// The fixture on disk is the one the numbers were computed from, so a
// later edit to it fails here rather than silently changing what the
// test proves.
func TestPercentiles_FixtureFileMatchesTheExpectedLengths(t *testing.T) {
	idx, err := Build(filepath.Join("..", "..", "testdata", "line-lengths.log"),
		BuildOptions{Analyze: false})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.LineCount == nil || *idx.LineCount != int64(len(fixtureLineLengths)) {
		t.Fatalf("line_count: got %v, want %d", idx.LineCount, len(fixtureLineLengths))
	}
	for name, pair := range map[string]struct {
		got  *float64
		want float64
	}{
		"avg":    {idx.LineLengthAvg, wantAvg},
		"median": {idx.LineLengthMedian, wantMedian},
		"p95":    {idx.LineLengthP95, wantP95},
		"p99":    {idx.LineLengthP99, wantP99},
		"stddev": {idx.LineLengthStddev, wantStdDev},
	} {
		if pair.got == nil {
			t.Errorf("%s is null", name)
			continue
		}
		if !closeEnough(*pair.got, pair.want) {
			t.Errorf("%s: got %v, want %v", name, *pair.got, pair.want)
		}
	}
	if idx.LineLengthMax == nil || *idx.LineLengthMax != int64(wantMax) {
		t.Errorf("max: got %v, want %v", idx.LineLengthMax, wantMax)
	}
}

// A file with no lines, and one with a single line, are the two shapes a
// percentile function gets wrong first.
func TestPercentiles_DegenerateInputs(t *testing.T) {
	empty := newLineStatsAccumulator(0).finish()
	for name, value := range map[string]float64{
		"mean": empty.Mean, "median": empty.Median,
		"p95": empty.P95, "p99": empty.P99, "stddev": empty.StdDev,
	} {
		if value != 0 {
			t.Errorf("empty file %s: got %v, want 0", name, value)
		}
	}

	single := newLineStatsAccumulator(0)
	single.observe(13, false, 1, 0)
	got := single.finish()
	for name, value := range map[string]float64{
		"mean": got.Mean, "median": got.Median, "p95": got.P95, "p99": got.P99,
	} {
		if value != 13 {
			t.Errorf("single line %s: got %v, want 13", name, value)
		}
	}
	if got.StdDev != 0 {
		t.Errorf("single line stddev: got %v, want 0", got.StdDev)
	}
}

// Above the reservoir cap the percentiles are estimates from a uniform
// sample, so the two backends cannot agree exactly and the guarantee is
// a tolerance instead. This pins that the sampling is unbiased enough to
// keep the estimate close to the true value.
func TestPercentiles_AboveTheReservoirStayWithinTolerance(t *testing.T) {
	const lineCount = 60_000
	source := rand.New(rand.NewSource(11)) //nolint:gosec // deterministic fixture, not security

	lengths := make([]int, 0, lineCount)
	sampled := newLineStatsAccumulator(0)
	exact := newLineStatsAccumulator(lineCount)
	for i := 0; i < lineCount; i++ {
		length := source.Intn(200) + 1
		lengths = append(lengths, length)
		sampled.observe(length, false, i+1, 0)
		exact.observe(length, false, i+1, 0)
	}

	estimate := sampled.finish()
	truth := exact.finish()

	// The documented tolerance: within 2% of the true value. Measured
	// worst case across six seeds is 1%, so this leaves room without
	// admitting a real regression.
	for name, pair := range map[string]struct{ got, want float64 }{
		"median": {estimate.Median, truth.Median},
		"p95":    {estimate.P95, truth.P95},
		"p99":    {estimate.P99, truth.P99},
	} {
		if drift := math.Abs(pair.got-pair.want) / pair.want; drift > 0.02 {
			t.Errorf("%s drifted %.2f%%: sampled %v, exact %v",
				name, drift*100, pair.got, pair.want)
		}
	}

	// The mean and the standard deviation are not sampled: Welford sees
	// every line, so they are exact whatever the reservoir holds.
	var sum float64
	for _, length := range lengths {
		sum += float64(length)
	}
	if !closeEnough(estimate.Mean, sum/float64(lineCount)) {
		t.Errorf("mean: got %v, want %v", estimate.Mean, sum/float64(lineCount))
	}
}
