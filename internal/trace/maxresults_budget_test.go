package trace

import (
	"context"
	"testing"
)

// TestMaxResultsOfOneOnAChunkedFile covers the smallest cap on a file
// split across chunks, where every line matches: the scan must return
// exactly one match rather than everything the first chunk holds.
//
// What the cap costs is measured on real files, not here — a fixture
// big enough to time is a fixture too slow to keep in a unit suite.
func TestMaxResultsOfOneOnAChunkedFile(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")

	path, _ := writeChunkedFixture(t, "capped.log", 8<<20)

	limit := 1
	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"line"},
		Options{MaxResults: &limit, NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.Matches) != limit {
		t.Fatalf("got %d matches, want %d", len(resp.Matches), limit)
	}
}

// TestMaxResultsCountsAreExact pins the counts a cap returns, which the
// early cancellation must not disturb.
func TestMaxResultsCountsAreExact(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")

	path, lines := writeChunkedFixture(t, "counts.log", 4<<20)
	needles := lines / 10 // every tenth line carries the needle

	uncapped, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"}, Options{NoCache: true},
	)
	if err != nil {
		t.Fatalf("uncapped scan: %v", err)
	}
	if len(uncapped.Matches) != needles {
		t.Fatalf("uncapped scan found %d matches, want %d", len(uncapped.Matches), needles)
	}

	for _, limit := range []int{1, 10, 100, 5000} {
		cap := limit
		resp, err := New().RunWithOptions(
			context.Background(), []string{path}, []string{"NEEDLE"},
			Options{MaxResults: &cap, NoCache: true},
		)
		if err != nil {
			t.Fatalf("cap %d: %v", cap, err)
		}
		want := cap
		if want > needles {
			want = needles
		}
		if len(resp.Matches) != want {
			t.Fatalf("cap %d returned %d matches, want %d", cap, len(resp.Matches), want)
		}
	}
}

// TestMaxResultsAboveTheMatchCountReturnsEverything covers the cap that
// never fires, which must behave exactly like no cap at all.
func TestMaxResultsAboveTheMatchCountReturnsEverything(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")

	path, lines := writeChunkedFixture(t, "roomy-cap.log", 4<<20)
	limit := lines * 10

	capped, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"},
		Options{MaxResults: &limit, NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	uncapped, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"}, Options{NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(capped.Matches) != len(uncapped.Matches) {
		t.Fatalf("a cap of %d returned %d matches, uncapped returns %d",
			limit, len(capped.Matches), len(uncapped.Matches))
	}
	for i := range capped.Matches {
		if capped.Matches[i].AbsoluteLineNumber != uncapped.Matches[i].AbsoluteLineNumber {
			t.Fatalf("match %d numbered %d under a cap, %d without one",
				i, capped.Matches[i].AbsoluteLineNumber, uncapped.Matches[i].AbsoluteLineNumber)
		}
	}
}
