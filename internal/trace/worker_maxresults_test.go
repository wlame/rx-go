// Byte-budget and short-circuit tests for ProcessAllChunks.
//
// The claim under test: on a file that spans many chunks, a small
// max_results must NOT scan every chunk to the end. Once the cap is
// reached the engine cancels the shared context, and chunks that have
// not started yet must return without spawning an rg subprocess.

package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
)

// writeAllMatchingFile writes a file whose every line matches "MATCH"
// and returns its path and size. Tests use it so that a chunk producing
// zero matches can only mean "this chunk never ran to completion".
func writeAllMatchingFile(t *testing.T, name string, lines int) (string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	var sb strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&sb, "MATCH line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return path, fi.Size()
}

// splitIntoTasks builds n FileTasks covering the whole file by hand,
// bypassing CreateFileTasks so the chunk boundaries do not depend on the
// configured minimum chunk size.
func splitIntoTasks(path string, size int64, n int) []FileTask {
	chunkSize := size / int64(n)
	tasks := make([]FileTask, 0, n)
	for i := range n {
		off := int64(i) * chunkSize
		count := chunkSize
		if i == n-1 {
			count = size - off
		}
		tasks = append(tasks, FileTask{
			TaskID: i, FilePath: path, Offset: off, Count: count,
		})
	}
	return tasks
}

// TestProcessAllChunks_MaxResultsCancelsSiblings guarantees that
// max_results cancels sibling chunks before they finish.
//
// The engine sizes its worker pool from RX_WORKERS (falling back to
// NumCPU), so the test pins it to 1. That matters for more than speed:
// with a pool as wide as the chunk count every chunk starts before the
// accumulator goroutine has drained a single tally value, so no
// cancellation can un-run them and the test would be measuring how many
// cores the machine has. Serialized, the chunks form a queue, and
// "queued work sees a canceled context and skips" becomes an
// observable property rather than the outcome of a race.
//
// Every line of the fixture matches, so a chunk reporting zero matches
// can only be a chunk that never ran to completion.
func TestProcessAllChunks_MaxResultsCancelsSiblings(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_WORKERS", "1")

	const numChunks = 8
	path, size := writeAllMatchingFile(t, "many-matches.txt", 30_000)
	tasks := splitIntoTasks(path, size, numChunks)

	maxResults := 5
	allMatches, _, err := ProcessAllChunks(
		context.Background(), tasks,
		map[string]string{"p1": "MATCH"}, []string{"p1"},
		nil, 0, 0,
		&maxResults,
	)
	if err != nil {
		t.Fatalf("ProcessAllChunks: %v", err)
	}

	totalMatches := 0
	chunksThatCompleted := 0
	for _, slot := range allMatches {
		if len(slot) > 0 {
			chunksThatCompleted++
			totalMatches += len(slot)
		}
	}

	// The first chunk alone holds thousands of matches, so the cap is
	// exceeded by whichever chunk runs first. Nothing running at all
	// would mean the cancel fired before any work started.
	if totalMatches < maxResults {
		t.Fatalf("totalMatches=%d < maxResults=%d — did any chunk run?",
			totalMatches, maxResults)
	}
	// The contract: at least one chunk must have been skipped. Scanning
	// all eight would mean the cap never reached the workers.
	if chunksThatCompleted == numChunks {
		t.Fatalf("all %d chunks produced matches (%d total) — reaching max_results "+
			"did not stop the remaining chunks", numChunks, totalMatches)
	}
	t.Logf("cap=%d, total=%d matches, chunks that ran=%d/%d",
		maxResults, totalMatches, chunksThatCompleted, numChunks)
}

// TestProcessAllChunks_CanceledContextSkipsEveryChunk pins down the
// half of the mechanism the test above can only observe indirectly: a
// worker whose context is already canceled must return without
// scanning. Canceling before the call removes the race entirely, so
// the expected result is exact — no chunk may report a match even
// though every line of the file is one.
func TestProcessAllChunks_CanceledContextSkipsEveryChunk(t *testing.T) {
	requireRipgrep(t)

	const numChunks = 4
	path, size := writeAllMatchingFile(t, "never-scanned.txt", 4_000)
	tasks := splitIntoTasks(path, size, numChunks)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	allMatches, _, err := ProcessAllChunks(
		ctx, tasks,
		map[string]string{"p1": "MATCH"}, []string{"p1"},
		nil, 0, 0,
		nil,
	)
	// A cooperative cancel is not an error: the caller asked to stop.
	if err != nil {
		t.Fatalf("ProcessAllChunks: %v", err)
	}
	for i, slot := range allMatches {
		if len(slot) > 0 {
			t.Fatalf("chunk %d returned %d matches on an already-canceled "+
				"context — the skip fast-path did not fire", i, len(slot))
		}
	}
}

// TestProcessAllChunks_NilMaxResultsScansEverything confirms the
// non-cap code path still runs every chunk, guarding against a cancel
// that fires when max_results is nil.
func TestProcessAllChunks_NilMaxResultsScansEverything(t *testing.T) {
	requireRipgrep(t)

	const numChunks = 4
	path, size := writeAllMatchingFile(t, "full-scan.txt", 1_000)
	tasks := splitIntoTasks(path, size, numChunks)

	allMatches, _, err := ProcessAllChunks(
		context.Background(), tasks,
		map[string]string{"p1": "MATCH"}, []string{"p1"},
		nil, 0, 0,
		nil, // NIL cap — scan everything
	)
	if err != nil {
		t.Fatalf("ProcessAllChunks: %v", err)
	}
	total := 0
	for _, s := range allMatches {
		total += len(s)
	}
	// All 1000 lines match. The hand-rolled offsets do not land on
	// newlines, so ProcessChunk's dedup filter drops a few at the chunk
	// boundaries; a floor of 900 leaves room for that.
	if total < 900 {
		t.Fatalf("nil-cap scan: total=%d, want >=900 (did we skip chunks?)", total)
	}
	t.Logf("nil-cap scan produced %d matches across %d chunks", total, numChunks)
}

// TestProcessAllChunks_MaxResultsLargerThanFile is the non-cap edge
// case: a cap so large the scan runs to completion anyway. It must
// behave identically to a nil cap, with no spurious cancel.
func TestProcessAllChunks_MaxResultsLargerThanFile(t *testing.T) {
	requireRipgrep(t)

	path, size := writeAllMatchingFile(t, "cap-too-big.txt", 500)
	tasks := []FileTask{{TaskID: 0, FilePath: path, Offset: 0, Count: size}}

	limit := 100_000
	allMatches, _, err := ProcessAllChunks(
		context.Background(), tasks,
		map[string]string{"p1": "MATCH"}, []string{"p1"},
		nil, 0, 0,
		&limit,
	)
	if err != nil {
		t.Fatalf("ProcessAllChunks: %v", err)
	}
	if len(allMatches[0]) != 500 {
		t.Fatalf("got %d matches, want 500", len(allMatches[0]))
	}
}

// Helper for other tests — silence unused-import warnings.
var _ = config.MinChunkSizeMB
