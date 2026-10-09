//go:build !race

// The race detector instruments memory accesses and can report
// allocations the plain build does not make, so this guard runs only
// without -race (`go test ./internal/index/`).

package index

import (
	"testing"
)

// observeBenchLine is a line of an application log with ISO timestamps
// to the millisecond.
var observeBenchLine = []byte("2025-12-10 07:00:04.574 INFO [http-nio-8080-exec-3] c.e.Gateway - request served in 12 ms")

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
