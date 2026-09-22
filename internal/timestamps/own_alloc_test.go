//go:build !race

// The race detector instruments memory accesses and can report
// allocations the plain build does not make, so this guard runs only
// without -race (`go test ./internal/timestamps/`).

package timestamps

import "testing"

// TestOwn_AllocatesNothing guards the per-line path of an index build.
func TestOwn_AllocatesNothing(t *testing.T) {
	for _, bl := range ownBenchLines {
		p := mustParser(t, bl.format, mtime2025)
		line := []byte(bl.line)
		if allocs := testing.AllocsPerRun(100, func() { p.Own(line) }); allocs != 0 {
			t.Errorf("%s: Own allocates %.1f times per call; want 0", bl.format.Family, allocs)
		}
		if allocs := testing.AllocsPerRun(100, func() { p.Locate(line) }); allocs != 0 {
			t.Errorf("%s: Locate allocates %.1f times per call; want 0", bl.format.Family, allocs)
		}
	}
}
