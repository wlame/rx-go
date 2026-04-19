package index

import (
	"math"
	"testing"
)

// TestLineStats_KnownDistribution checks Welford produces exact mean + stddev
// for a small hand-calculable sample: {10, 20, 30} → mean=20, sample stddev=10.
func TestLineStats_KnownDistribution(t *testing.T) {
	acc := newLineStatsAccumulator(0)
	acc.observe(10, false, 1, 0)
	acc.observe(20, false, 2, 10)
	acc.observe(30, false, 3, 30)

	if acc.count != 3 {
		t.Errorf("count: got %d want 3", acc.count)
	}
	if math.Abs(acc.mean-20.0) > 1e-9 {
		t.Errorf("mean: got %f want 20.0", acc.mean)
	}
	stddev := math.Sqrt(acc.m2 / float64(acc.count-1))
	if math.Abs(stddev-10.0) > 1e-9 {
		t.Errorf("stddev: got %f want 10.0", stddev)
	}
}

// TestLineStats_EmptyLinesExcluded asserts empty lines increment emptyCount
// but don't pollute length statistics.
func TestLineStats_EmptyLinesExcluded(t *testing.T) {
	acc := newLineStatsAccumulator(0)
	acc.observe(100, false, 1, 0)
	acc.observe(0, true, 2, 100)
	acc.observe(200, false, 3, 100)

	if acc.count != 2 {
		t.Errorf("count: got %d want 2", acc.count)
	}
	if acc.emptyCount != 1 {
		t.Errorf("emptyCount: got %d want 1", acc.emptyCount)
	}
	if math.Abs(acc.mean-150.0) > 1e-9 {
		t.Errorf("mean: got %f want 150.0", acc.mean)
	}
}

// TestLineStats_ConstantMemory is the headline invariant: observing 10 M samples
// must NOT grow the reservoir beyond its cap. This is the memory-floor guarantee
// that justifies the entire refactor.
func TestLineStats_ConstantMemory(t *testing.T) {
	const reservoirCap = 10_000
	acc := newLineStatsAccumulator(reservoirCap)
	for i := 1; i <= 10_000_000; i++ {
		acc.observe(i%500, false, i, int64(i*500))
	}
	if len(acc.reservoir) > reservoirCap {
		t.Errorf("reservoir exceeded cap: got %d want <= %d", len(acc.reservoir), reservoirCap)
	}
	if acc.count != 10_000_000 {
		t.Errorf("count: got %d want 10_000_000", acc.count)
	}
}

// TestLineStats_MaxTracking asserts max length + its metadata (line number,
// byte offset) is captured exactly. The reservoir is too noisy for tails, so
// we track the max independently.
func TestLineStats_MaxTracking(t *testing.T) {
	acc := newLineStatsAccumulator(0)
	acc.observe(50, false, 1, 0)
	acc.observe(500, false, 2, 50)
	acc.observe(100, false, 3, 550)
	if acc.maxLength != 500 {
		t.Errorf("maxLength: got %d want 500", acc.maxLength)
	}
	if acc.maxLineNumber != 2 {
		t.Errorf("maxLineNumber: got %d want 2", acc.maxLineNumber)
	}
	if acc.maxLineOffset != 50 {
		t.Errorf("maxLineOffset: got %d want 50", acc.maxLineOffset)
	}
}

// TestLineStats_Finish_EmptyAccumulator guards the zero-samples edge case —
// dividing by (count-1) would NaN without the guard.
func TestLineStats_Finish_EmptyAccumulator(t *testing.T) {
	acc := newLineStatsAccumulator(0)
	snap := acc.finish()
	if snap.Count != 0 || snap.Mean != 0 || snap.StdDev != 0 || snap.P99 != 0 {
		t.Errorf("empty accumulator should produce zeros: %+v", snap)
	}
}

// TestLineStats_Finish_PercentilesApproxCorrect sanity-checks the reservoir
// against a known uniform distribution (1..1_000_000). Reservoir sampling is
// stochastic so we allow a loose tolerance (~2% of range).
func TestLineStats_Finish_PercentilesApproxCorrect(t *testing.T) {
	acc := newLineStatsAccumulator(10_000)
	for i := 1; i <= 1_000_000; i++ {
		acc.observe(i, false, i, int64(i))
	}
	snap := acc.finish()

	// Welford's mean is exact to FP precision, so a very tight bound.
	if math.Abs(snap.Mean-500_000.5) > 500 {
		t.Errorf("mean should be ~500_000.5 (Welford is exact up to FP): got %f", snap.Mean)
	}
	// Reservoir-based percentiles: loose tolerance (±2% of range).
	if math.Abs(snap.Median-500_000) > 20_000 {
		t.Errorf("median should be ~500_000: got %f", snap.Median)
	}
	if math.Abs(snap.P95-950_000) > 20_000 {
		t.Errorf("p95 should be ~950_000: got %f", snap.P95)
	}
	if math.Abs(snap.P99-990_000) > 20_000 {
		t.Errorf("p99 should be ~990_000: got %f", snap.P99)
	}
	// Max is tracked exactly (independent of reservoir).
	if snap.Max != 1_000_000 {
		t.Errorf("max should be exact: got %d", snap.Max)
	}
}
