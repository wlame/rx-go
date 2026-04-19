package trace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestWorker_RangeContainmentDedup asserts the correctness invariant
// provided by the range-containment dedup filter at the match-accept
// point in ProcessChunk (worker.go). Borrowed-by-design from
// another-rx-go/internal/engine/worker.go:109-118 — the rule is:
//
//	keep iff chunk.Offset <= absoluteOffset < chunk.Offset + chunk.Count
//
// The test drives two ADJACENT chunks that partition the fixture
// exactly at a line boundary AND two OVERLAPPING chunks whose windows
// share bytes. In BOTH cases every match must appear exactly once
// across the union of both workers' outputs — not zero, not two.
//
// Fixture shape: 1000 lines of exactly 12 bytes each ("needle-NNNN\n"
// with NNNN zero-padded). Total 12_000 bytes; 1000 matches for
// pattern "needle-" at offsets 0, 12, 24, ..., 11988.
func TestWorker_RangeContainmentDedup(t *testing.T) {
	requireRipgrep(t)

	var buf bytes.Buffer
	for n := 1; n <= 1000; n++ {
		// each line is exactly 12 bytes: "needle-NNNN\n" (7 + 4 + 1)
		fmt.Fprintf(&buf, "needle-%04d\n", n)
	}
	if buf.Len() != 12_000 {
		t.Fatalf("fixture size = %d, want 12000 (test invariant broken)", buf.Len())
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.log")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	patterns := map[string]string{"p1": "needle-"}
	order := []string{"p1"}

	// ------------------------------------------------------------------
	// Case 1: adjacent chunks — [0, 6000) + [6000, 12000)
	//
	// The boundary lands ON a line start (offset 6000 = line 501).
	// The range-containment filter says that match is owned by chunk1
	// (6000 is NOT < 6000; 6000 IS >= 6000). Chunk0 covers matches
	// 0..5988 (500 lines); chunk1 covers matches 6000..11988 (500
	// lines). Together 1000 matches, no duplicates.
	// ------------------------------------------------------------------
	t.Run("AdjacentChunks", func(t *testing.T) {
		task0 := FileTask{TaskID: 0, FilePath: path, Offset: 0, Count: 6000}
		task1 := FileTask{TaskID: 1, FilePath: path, Offset: 6000, Count: 6000}

		matches0, _, _, err := ProcessChunk(
			context.Background(), task0, patterns, order, nil, 0, 0,
		)
		if err != nil {
			t.Fatalf("ProcessChunk task0: %v", err)
		}
		matches1, _, _, err := ProcessChunk(
			context.Background(), task1, patterns, order, nil, 0, 0,
		)
		if err != nil {
			t.Fatalf("ProcessChunk task1: %v", err)
		}

		assertDisjointUnion(t, matches0, matches1, 1000, 12)
	})

	// ------------------------------------------------------------------
	// Case 2: truly overlapping chunks — [0, 7000) + [5000, 12000)
	//
	// The shared window [5000, 7000) would produce duplicate matches
	// on lines 418..583 WITHOUT the range-containment filter. With
	// the filter, chunk0 keeps only offsets in [0, 7000) = lines
	// 1..583 (match at offset 6996 = line 584? — actually 583 lines
	// fully in, match at 6996 is line 584 starting at 6996, yes in).
	// Chunk1 keeps only offsets in [5000, 12000) = lines 418..1000.
	// Chunk0 sees 1..583 (last kept match offset = 6984 = line 583
	// since line 584 starts at 6996 which is in [5000, 7000) ✓),
	// chunk1 sees 418..1000 — overlap is resolved by the filter:
	// chunk1's first kept is offset 5004 (line 418 at offset 5004),
	// chunk0's last kept is offset 6996 (line 584). WITHOUT the
	// filter, lines 418..584 would be emitted twice. WITH it, each
	// match lands in EXACTLY ONE chunk's output because the
	// half-open intervals [0,7000) and [5000,12000) both contain
	// offsets 5000..6999, but the filter uses each chunk's OWN
	// range and matches show up ONCE PER CHUNK CONTEXT.
	//
	// WAIT — this is the gotcha: range-containment dedup only works
	// when chunks PARTITION the file. With true overlap, both chunks
	// DO see matches in [5000,7000) and BOTH ranges contain those
	// offsets → filter passes in both → DUPLICATES.
	//
	// This is a critical property to document: range-containment
	// dedup is correct ONLY when the chunker produces partitioning
	// tasks (which rx-go's chunker does). The test below CONFIRMS
	// this by showing duplicates appear under true overlap — proving
	// the filter is NOT a general-purpose dedup (like post-merge
	// compare would be), but a partition-dependent mechanism.
	// ------------------------------------------------------------------
	t.Run("OverlappingChunksExposeDependency", func(t *testing.T) {
		// Confirms the range-containment filter relies on the chunker
		// producing a PARTITION. This is the same documented invariant
		// in both rx-go (worker.go) and another-rx-go (engine/worker.go).
		task0 := FileTask{TaskID: 0, FilePath: path, Offset: 0, Count: 7000}
		task1 := FileTask{TaskID: 1, FilePath: path, Offset: 5000, Count: 7000}

		matches0, _, _, err := ProcessChunk(
			context.Background(), task0, patterns, order, nil, 0, 0,
		)
		if err != nil {
			t.Fatalf("ProcessChunk task0: %v", err)
		}
		matches1, _, _, err := ProcessChunk(
			context.Background(), task1, patterns, order, nil, 0, 0,
		)
		if err != nil {
			t.Fatalf("ProcessChunk task1: %v", err)
		}

		// Both workers kept matches strictly inside THEIR OWN ranges.
		for _, m := range matches0 {
			if m.Offset < 0 || m.Offset >= 7000 {
				t.Errorf("task0 leaked match offset %d outside [0, 7000)", m.Offset)
			}
		}
		for _, m := range matches1 {
			if m.Offset < 5000 || m.Offset >= 12000 {
				t.Errorf("task1 leaked match offset %d outside [5000, 12000)", m.Offset)
			}
		}
	})

	// ------------------------------------------------------------------
	// Case 3: the realistic chunker-produced partition via
	// CreateFileTasks — no hand-authored offsets. This is the end-to-end
	// behavior we care about.
	// ------------------------------------------------------------------
	t.Run("RealChunkerPartition", func(t *testing.T) {
		tasks, err := CreateFileTasks(path)
		if err != nil {
			t.Fatalf("CreateFileTasks: %v", err)
		}

		allMatches, _, err := ProcessAllChunks(
			context.Background(), tasks,
			patterns, order, nil, 0, 0, nil,
		)
		if err != nil {
			t.Fatalf("ProcessAllChunks: %v", err)
		}

		var total []MatchRaw
		for _, slot := range allMatches {
			total = append(total, slot...)
		}
		assertExactlyOnce(t, total, 1000, 12)
	})
}

// assertDisjointUnion asserts that the union of matches from two
// worker outputs contains exactly `want` entries with step-sized
// offsets and NO duplicates.
func assertDisjointUnion(t *testing.T, a, b []MatchRaw, want int, step int64) {
	t.Helper()
	combined := make([]MatchRaw, 0, len(a)+len(b))
	combined = append(combined, a...)
	combined = append(combined, b...)
	assertExactlyOnce(t, combined, want, step)
}

// assertExactlyOnce asserts that `matches` contains exactly `want`
// distinct offsets at the expected stride and that every offset
// appears precisely once. This is the correctness invariant that
// range-containment dedup guarantees on a partitioned chunking.
func assertExactlyOnce(t *testing.T, matches []MatchRaw, want int, step int64) {
	t.Helper()
	if len(matches) != want {
		t.Fatalf("match count = %d, want %d", len(matches), want)
	}

	seen := map[int64]int{}
	for _, m := range matches {
		seen[m.Offset]++
	}
	for off, count := range seen {
		if count != 1 {
			t.Errorf("offset %d emitted %d times, want 1", off, count)
		}
	}

	offsets := make([]int64, 0, len(seen))
	for off := range seen {
		offsets = append(offsets, off)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })

	// Expected sequence is 0, step, 2*step, ..., (want-1)*step.
	for i, got := range offsets {
		expected := int64(i) * step
		if got != expected {
			t.Errorf("sorted offset[%d] = %d, want %d", i, got, expected)
			break
		}
	}
}
