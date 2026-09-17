package samples

import (
	"os"
	"path/filepath"
	"testing"
)

// The batch offset resolver answers every offset from one pass, and the
// answer must not depend on the order the caller listed them in.
//
// TestBudget_ManyOffsets_OnePassOverTheFile pins the cost; these pin the
// content, which is the half a byte budget cannot see.

const (
	budgetLineBytes = 150
	budgetLineCount = 10_000
)

// offsetsNearTheEnd returns count offsets spread over the last tenth of
// the fixture — the region a per-offset scan has to re-read every time.
func offsetsNearTheEnd(count int) []int64 {
	firstLine := int64(budgetLineCount - budgetLineCount/10)
	step := (int64(budgetLineCount) - firstLine) / int64(count)
	if step < 1 {
		step = 1
	}
	out := make([]int64, 0, count)
	for n := int64(0); n < int64(count); n++ {
		out = append(out, (firstLine+n*step)*budgetLineBytes)
	}
	return out
}

// The batch answer is the one-at-a-time answer, for offsets given in
// order, out of order and repeated.
func TestBatchOffsets_AgreeWithOneAtATime(t *testing.T) {
	path, _ := makeLargeFixture(t, budgetLineCount)
	offsets := offsetsNearTheEnd(8)

	together, err := lineNumbersForOffsets(pinForTest(t, path), offsets, nil)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, offset := range offsets {
		alone, aErr := lineNumberForOffset(pinForTest(t, path), offset)
		if aErr != nil {
			t.Fatalf("offset %d: %v", offset, aErr)
		}
		if together[offset] != alone {
			t.Errorf("offset %d: batch says line %d, one-at-a-time says %d",
				offset, together[offset], alone)
		}
	}

	shuffled := append([]int64(nil), offsets...)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	shuffled = append(shuffled, offsets[0], offsets[2])

	jumbled, err := lineNumbersForOffsets(pinForTest(t, path), shuffled, nil)
	if err != nil {
		t.Fatalf("shuffled batch: %v", err)
	}
	for _, offset := range offsets {
		if jumbled[offset] != together[offset] {
			t.Errorf("offset %d: order changed the answer (%d vs %d)",
				offset, jumbled[offset], together[offset])
		}
	}
}

// A file whose last line has no trailing newline still numbers its
// offsets: the final partial line is a line.
func TestBatchOffsets_UnterminatedLastLine(t *testing.T) {
	path := writeExactFixture(t, "alpha\nbeta\ngamma")

	got, err := lineNumbersForOffsets(pinForTest(t, path), []int64{0, 6, 11}, nil)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	want := map[int64]int64{0: 1, 6: 2, 11: 3}
	for offset, wantLine := range want {
		if got[offset] != wantLine {
			t.Errorf("offset %d: got line %d, want %d", offset, got[offset], wantLine)
		}
	}
}

// writeExactFixture writes exact content to a temporary file and returns
// its path. Used for the cases makeLargeFixture's uniform lines cannot
// express, such as a file with no trailing newline.
func writeExactFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}
