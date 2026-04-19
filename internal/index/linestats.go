// Constant-memory line-length statistics.
//
// This file replaces the previous O(N) slice-based accumulator with two
// online algorithms that together keep memory use flat regardless of input
// size:
//
//  1. Welford's online algorithm for mean and sample variance — exact to
//     floating-point precision, numerically stable (no catastrophic
//     cancellation like naive sum-of-squares would have).
//
//  2. Reservoir sampling (Vitter 1985, Algorithm R) for median / p95 / p99
//     estimation — keeps exactly reservoirCap uniformly-random samples
//     regardless of total observation count.
//
// Memory footprint is O(reservoirCap) = ~80 KB for the default 10 000-sample
// reservoir. Previously the builder accumulated every non-empty line's length
// into a []int64, which on a 1.3 GB / 15 M-line file ballooned to ~120 MB
// just for the slice header-plus-backing-array.
//
// Borrowed from another-rx-go/internal/index/builder.go:23-120.
package index

import (
	"math"
	"math/rand/v2"
	"sort"
)

// defaultReservoirSize balances percentile accuracy (≈ ±1% for p99 per
// reservoir-sampling theory) and memory (10 000 × 8 bytes = 80 KB per build).
const defaultReservoirSize = 10_000

// lineStatsAccumulator tracks line-length statistics in constant memory.
//
// The zero value is NOT usable — always construct via newLineStatsAccumulator.
type lineStatsAccumulator struct {
	// Welford's algorithm state.
	//
	// count is the number of non-empty lines observed so far. mean is the
	// running arithmetic mean of their lengths. m2 is the running sum of
	// squared deviations from the mean, from which sample variance is
	// computed at finish-time as m2 / (count - 1).
	count int64
	mean  float64
	m2    float64

	// Reservoir sampling state.
	//
	// reservoir holds up to reservoirCap uniformly-random samples of observed
	// line lengths. Once full, each new observation has probability
	// (reservoirCap / count) of displacing a random existing sample; this
	// preserves uniformity over the entire stream.
	reservoir    []int
	reservoirCap int
	rng          *rand.Rand

	// Extremes tracked independently of the reservoir (which is too noisy
	// in the tails for exact max detection).
	maxLength     int
	maxLineNumber int
	maxLineOffset int64

	// emptyCount counts lines that are whitespace-only; they are excluded
	// from length statistics but reported separately.
	emptyCount int
}

// newLineStatsAccumulator returns an accumulator with the given reservoir
// capacity. Pass 0 to use the default (10 000 samples).
//
// The RNG is seeded deterministically so repeated builds on the same input
// produce identical percentile outputs — useful for testing and cache
// stability. We don't need cryptographic randomness for percentile
// estimation.
func newLineStatsAccumulator(reservoirCap int) *lineStatsAccumulator {
	if reservoirCap <= 0 {
		reservoirCap = defaultReservoirSize
	}
	return &lineStatsAccumulator{
		reservoir:    make([]int, 0, reservoirCap),
		reservoirCap: reservoirCap,
		// math/rand/v2 PCG: fast, deterministic-per-seed, adequate for
		// percentile estimation (we aren't generating secrets here).
		// #nosec G404 -- reservoir sampling doesn't need cryptographic RNG.
		rng: rand.New(rand.NewPCG(0x1234567890abcdef, 0xdeadbeefcafe1234)),
	}
}

// observe records a single line's length.
//
// Parameters:
//   - lineLen:    content length (bytes of the line excluding trailing CR/LF)
//   - isEmpty:    true if the line is whitespace-only; such lines increment
//     emptyCount and are EXCLUDED from length statistics
//   - lineNumber: 1-based line number (used only for max tracking)
//   - lineOffset: byte offset of the line's start in the source file
//     (used only for max tracking)
//
// Per-call cost is O(1): two comparisons, one RNG call, and a handful of
// floating-point ops — no allocations after the reservoir reaches its cap.
func (a *lineStatsAccumulator) observe(lineLen int, isEmpty bool, lineNumber int, lineOffset int64) {
	if isEmpty {
		a.emptyCount++
		return
	}

	// Welford's online update — numerically stable running mean + M2.
	a.count++
	delta := float64(lineLen) - a.mean
	a.mean += delta / float64(a.count)
	delta2 := float64(lineLen) - a.mean
	a.m2 += delta * delta2

	// Reservoir sampling (Algorithm R, Vitter 1985).
	//
	// Phase 1: fill the reservoir with the first reservoirCap samples.
	// Phase 2: for each subsequent sample, pick a uniform random index j
	//          in [0, count); if j < reservoirCap, replace slot j. This
	//          yields uniform sampling without replacement over the full
	//          (unknown-length) stream.
	if len(a.reservoir) < a.reservoirCap {
		a.reservoir = append(a.reservoir, lineLen)
	} else {
		j := a.rng.IntN(int(a.count))
		if j < a.reservoirCap {
			a.reservoir[j] = lineLen
		}
	}

	if lineLen > a.maxLength {
		a.maxLength = lineLen
		a.maxLineNumber = lineNumber
		a.maxLineOffset = lineOffset
	}
}

// lineStatsSnapshot is the finalized view of the accumulator — a pure data
// struct the caller can read freely. Basic types only, no references back
// into the accumulator's internal state.
type lineStatsSnapshot struct {
	Count         int64
	EmptyCount    int
	Mean          float64
	StdDev        float64
	Max           int
	MaxLineNumber int
	MaxLineOffset int64
	Median        float64
	P95           float64
	P99           float64
}

// finish computes the final statistics.
//
// The reservoir is sorted into a LOCAL copy; the accumulator itself is
// left unchanged so repeated finish() calls produce identical output
// (useful for tests and defensive code paths).
func (a *lineStatsAccumulator) finish() lineStatsSnapshot {
	snap := lineStatsSnapshot{
		Count:         a.count,
		EmptyCount:    a.emptyCount,
		Mean:          a.mean,
		Max:           a.maxLength,
		MaxLineNumber: a.maxLineNumber,
		MaxLineOffset: a.maxLineOffset,
	}

	if a.count == 0 {
		return snap
	}

	// Sample standard deviation from Welford's M2 (n-1 denominator matches
	// Python's statistics.stdev). Undefined for n<2; leave StdDev at zero.
	if a.count > 1 {
		snap.StdDev = math.Sqrt(a.m2 / float64(a.count-1))
	}

	if len(a.reservoir) > 0 {
		sorted := make([]int, len(a.reservoir))
		copy(sorted, a.reservoir)
		sort.Ints(sorted)
		snap.Median = reservoirPercentile(sorted, 50)
		snap.P95 = reservoirPercentile(sorted, 95)
		snap.P99 = reservoirPercentile(sorted, 99)
	}

	return snap
}

// reservoirPercentile returns the pth percentile (0-100) of a sorted int
// slice using linear interpolation between adjacent ranks.
//
// This matches both NumPy's default "linear" method and Python's inline
// _percentile helper in rx-python/src/rx/unified_index.py, so JSON output
// is interchangeable between the Python and Go builders when both see the
// same data.
//
// Naming note: this is an internal helper distinct from the package-level
// percentile() in builder.go (which operates on []int64 — the legacy
// slice-of-lengths path). Once all callers route through the accumulator
// the legacy helper can be deleted.
func reservoirPercentile(sorted []int, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return float64(sorted[0])
	}
	if p >= 100 {
		return float64(sorted[len(sorted)-1])
	}
	rank := (p / 100) * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return float64(sorted[lo])
	}
	frac := rank - float64(lo)
	return float64(sorted[lo])*(1-frac) + float64(sorted[hi])*frac
}
