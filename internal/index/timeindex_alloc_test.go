//go:build !race

// The race detector instruments memory accesses and can report
// allocations the plain build does not make, so this guard runs only
// without -race (`go test ./internal/index/`).

package index

import (
	"testing"

	"github.com/wlame/rx-go/internal/timestamps"
)

// observeBenchLine is a line of the shape middleware.log writes.
var observeBenchLine = []byte("2025-12-10 07:00:04.574 INFO [http-nio-8080-exec-3] c.e.Gateway - request served in 12 ms")

func isoIndexer(tb testing.TB) *timeIndexer {
	tb.Helper()
	ti, err := newTimeIndexer(timestamps.Format{Family: timestamps.FamilyISO, Anchored: true}, 0)
	if err != nil {
		tb.Fatalf("newTimeIndexer: %v", err)
	}
	return ti
}

// TestTimeIndexerObserve_AllocatesNothing guards the per-line path the
// time section adds to every index build.
func TestTimeIndexerObserve_AllocatesNothing(t *testing.T) {
	ti := isoIndexer(t)
	n := int64(0)
	allocs := testing.AllocsPerRun(1000, func() {
		n++
		ti.observe(observeBenchLine, n, n*100, n*100+100)
	})
	if allocs != 0 {
		t.Errorf("observe allocates %.1f times per line; want 0", allocs)
	}
}

// BenchmarkTimeIndexerObserve is the cost the time section adds to each
// line of an index build (`go test -bench=Observe -benchmem`).
func BenchmarkTimeIndexerObserve(b *testing.B) {
	ti := isoIndexer(b)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		n := int64(i + 1)
		ti.observe(observeBenchLine, n, n*100, n*100+100)
	}
}
