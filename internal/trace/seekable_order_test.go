package trace

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A capped scan of a seekable file keeps the matches that come first in
// the file. Matches carry a line number counted within their own frame,
// so ordering by it put line 1 of every frame ahead of line 2 of the
// first, and the cap kept one match from each frame instead.
//
// Every line is 10 bytes and every frame 40, so frames end on line
// boundaries and all of them go to one batch; with one batch the scan
// finishes before the cap is checked, so the test is deterministic.
func TestProcessSeekable_CapKeepsTheEarliestMatchesInFileOrder(t *testing.T) {
	requireRipgrep(t)

	var content strings.Builder
	for line := 1; line <= 12; line++ {
		fmt.Fprintf(&content, "error %03d\n", line) // 10 bytes
	}
	p := writeSeekableZstdFile(t, []byte(content.String()), 40)

	limit := 3
	matches, _, _, err := ProcessSeekable(
		context.Background(), p,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, &limit,
	)
	if err != nil {
		t.Fatalf("ProcessSeekable: %v", err)
	}

	offsets := make([]int64, 0, len(matches))
	for _, m := range matches {
		offsets = append(offsets, m.Offset)
	}
	if want := []int64{0, 10, 20}; !slices.Equal(offsets, want) {
		t.Errorf("offsets %v, want %v (the first three lines)", offsets, want)
	}
}
