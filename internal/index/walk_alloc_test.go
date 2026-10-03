//go:build !race

// The race detector instruments memory accesses and can report
// allocations the plain build does not make, so this guard runs only
// without -race (`go test ./internal/index/`).

package index

import (
	"bytes"
	"testing"
)

// The walk reads each line where the read buffer holds it, so the
// number of allocations follows the checkpoints and the fixed buffers,
// not the number of lines.
func TestWalkLines_DoesNotAllocatePerLine(t *testing.T) {
	text := numberedText(20_000, "request served in 12 ms")
	allocs := testing.AllocsPerRun(3, func() {
		if _, err := walkLines(bytes.NewReader(text), 1<<20, nil, nil); err != nil {
			t.Fatalf("walkLines: %v", err)
		}
	})
	if allocs > 200 {
		t.Errorf("walkLines allocated %.0f times for 20,000 lines; want it independent of the line count", allocs)
	}
}
