//go:build !race

// The race detector instruments memory accesses and can report
// allocations the plain build does not make, so this guard runs only
// without -race (`go test ./internal/index/`).

package index

import (
	"bufio"
	"bytes"
	"runtime"
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

// allocatedBy returns how many bytes of heap f allocates. The test that
// calls it does not run in parallel, so the count is f's own.
func allocatedBy(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The line-ending sample keeps counts, not bytes, so a 10 MiB line costs
// the walk the buffer that joins its pieces and no copy of it. The cost
// of joining is measured on its own, with the walk's own nextLine over
// a read buffer of the walk's size, and the walk may spend only a
// little more than that.
func TestWalkLines_ALongLineIsNotCopiedForTheLineEndingSample(t *testing.T) {
	text := append(bytes.Repeat([]byte("x"), 10<<20), "\r\nnext line\r\n"...)
	joining := allocatedBy(func() {
		br := bufio.NewReaderSize(bytes.NewReader(text), 256*1024)
		var joined []byte
		for {
			line, err := nextLine(br, &joined)
			if err != nil {
				t.Fatalf("nextLine: %v", err)
			}
			if len(line) == 0 {
				return
			}
		}
	})
	walking := allocatedBy(func() {
		if _, err := walkLines(bytes.NewReader(text), 1<<20, nil, nil); err != nil {
			t.Fatalf("walkLines: %v", err)
		}
	})
	t.Logf("joining the line allocated %d KiB, the walk %d KiB", joining>>10, walking>>10)
	const slack = 2 << 20
	if walking > joining+slack {
		t.Errorf("the walk allocated %d KiB, %d KiB more than joining the line; want at most %d KiB more",
			walking>>10, (walking-joining)>>10, slack>>10)
	}
}
