// merge.go implements heap-based sorted merge of match results from multiple chunks.
//
// After parallel chunk workers produce their results, we need to merge them into a
// single sorted stream ordered by (FileID, AbsoluteOffset). A min-heap merge is used
// so we don't need to concatenate and re-sort — each source is already sorted by offset.
package engine

import (
	"container/heap"

	"github.com/wlame/rx/internal/models"
)

// MergeResults merges multiple sorted match slices into a single sorted slice.
//
// Each input slice is expected to be sorted by (File, Offset). The merge uses a min-heap
// to efficiently produce the globally sorted output in O(N log K) time where N is the
// total number of matches and K is the number of sources.
//
// Special cases:
//   - Empty input returns nil.
//   - Single source is returned directly (no copy, no heap overhead).
func MergeResults(results [][]models.Match) []models.Match {
	if len(results) == 0 {
		return nil
	}

	// Filter out empty slices.
	var nonEmpty [][]models.Match
	for _, r := range results {
		if len(r) > 0 {
			nonEmpty = append(nonEmpty, r)
		}
	}

	if len(nonEmpty) == 0 {
		return nil
	}

	// Single source fast path — skip the heap entirely.
	if len(nonEmpty) == 1 {
		return nonEmpty[0]
	}

	// Count total matches for pre-allocation.
	total := 0
	for _, r := range nonEmpty {
		total += len(r)
	}

	// Initialize the min-heap with the first element from each source.
	h := &matchHeap{}
	heap.Init(h)
	for i, r := range nonEmpty {
		heap.Push(h, heapEntry{
			match:    r[0],
			sourceID: i,
			index:    0,
		})
	}

	merged := make([]models.Match, 0, total)

	for h.Len() > 0 {
		// Pop the smallest match.
		entry := heap.Pop(h).(heapEntry)
		merged = append(merged, entry.match)

		// Push the next match from the same source, if any.
		nextIdx := entry.index + 1
		if nextIdx < len(nonEmpty[entry.sourceID]) {
			heap.Push(h, heapEntry{
				match:    nonEmpty[entry.sourceID][nextIdx],
				sourceID: entry.sourceID,
				index:    nextIdx,
			})
		}
	}

	return merged
}

// TruncateResults truncates a match slice to at most limit entries.
// If limit <= 0 or the slice is already shorter, it is returned unchanged.
func TruncateResults(matches []models.Match, limit int) []models.Match {
	if limit <= 0 || len(matches) <= limit {
		return matches
	}
	return matches[:limit]
}

// --- min-heap implementation for merge ---

// heapEntry tracks a match and its position within its source slice.
type heapEntry struct {
	match    models.Match
	sourceID int // Which source slice this match came from.
	index    int // Current index within that source slice.
}

// matchHeap implements heap.Interface for heapEntry values, ordered by (File, Offset).
type matchHeap []heapEntry

func (h matchHeap) Len() int { return len(h) }

func (h matchHeap) Less(i, j int) bool {
	// Primary sort: file ID (lexicographic, so "f1" < "f10" < "f2" — matches Python's sort).
	if h[i].match.File != h[j].match.File {
		return h[i].match.File < h[j].match.File
	}
	// Secondary sort: byte offset within the file.
	return h[i].match.Offset < h[j].match.Offset
}

func (h matchHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *matchHeap) Push(x interface{}) {
	*h = append(*h, x.(heapEntry))
}

func (h *matchHeap) Pop() interface{} {
	old := *h
	n := len(old)
	entry := old[n-1]
	*h = old[:n-1]
	return entry
}
