// Stage 9 Round 5 — R5-B2 byte-budget test.
//
// Verifies that ProcessAllChunks cooperatively cancels sibling chunks
// once the maxResults cap has been reached. Budget enforced both by
// counting resulting matches AND by observing that only a fraction of
// the file's chunks actually complete their rg scan.
//
// The key claim: on a file that spans many chunks, asking for
// maxResults=3 must NOT scan every chunk to the end — it must stop as
// soon as the first chunk (or few chunks) produce the cap.

package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
)

// TestProcessAllChunks_MaxResultsCancelsSiblings is the R5-B2 regression
// guard. Build a large-ish file spanning many chunks. Every line is a
// match (so each chunk produces 1000s of matches). With maxResults=5:
//
//  1. The first chunk to complete reports its N matches via tally.
//  2. N >= 5 so the accumulator fires cancel().
//  3. Already-running rg subprocesses in other chunks receive SIGKILL
//     via exec.CommandContext.
//  4. Queued chunks see gctx.Err() != nil and skip ProcessChunk entirely.
//  5. The caller receives all results from the chunk that ran, plus
//     nil error (the cooperative cancel is NOT surfaced as an error).
func TestProcessAllChunks_MaxResultsCancelsSiblings(t *testing.T) {
	requireRipgrep(t)
	// Force more than one chunk. Stage-6 plan default MinChunkSizeMB is
	// 20. A ~20 MB file at 1 MB min-chunk would create many tasks —
	// but we can't change config mid-test safely without env vars.
	// Instead we manually craft the FileTask slice with small sizes.
	dir := t.TempDir()
	path := filepath.Join(dir, "many-matches.txt")
	// Each line is a match; 50 KB per chunk × 8 chunks = 400 KB.
	// Line: "MATCH line <N>\n" (17 bytes avg including newline).
	var sb strings.Builder
	for i := 1; i <= 30_000; i++ {
		fmt.Fprintf(&sb, "MATCH line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Build 8 chunks by hand (bypass CreateFileTasks so we get
	// predictable chunk boundaries regardless of config defaults).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	const numChunks = 8
	chunkSize := fi.Size() / numChunks
	tasks := make([]FileTask, 0, numChunks)
	for i := 0; i < numChunks; i++ {
		off := int64(i) * chunkSize
		count := chunkSize
		if i == numChunks-1 {
			count = fi.Size() - off
		}
		tasks = append(tasks, FileTask{
			TaskID: i, FilePath: path, Offset: off, Count: count,
		})
	}

	maxResults := 5
	start := time.Now()
	allMatches, _, err := ProcessAllChunks(
		context.Background(), tasks,
		map[string]string{"p1": "MATCH"}, []string{"p1"},
		nil, 0, 0,
		&maxResults,
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ProcessAllChunks: %v", err)
	}

	// --- Cap enforcement: total matches ACROSS chunks must exceed cap
	// (because the first chunk alone produces 3000+ matches) — but the
	// number of CHUNKS THAT EXECUTED should be far fewer than numChunks.
	totalMatches := 0
	chunksWithAnyMatches := 0
	chunksWithEmpty := 0
	for _, slot := range allMatches {
		if len(slot) > 0 {
			chunksWithAnyMatches++
			totalMatches += len(slot)
		} else {
			chunksWithEmpty++
		}
	}
	if totalMatches < maxResults {
		t.Fatalf("totalMatches=%d < maxResults=%d — did any chunk run?",
			totalMatches, maxResults)
	}
	// The whole file has 30000 matches. If we ran every chunk we'd see
	// totalMatches = 30000. We expect FAR less because cancel() fires
	// after the first chunk returns.
	if totalMatches > 20_000 {
		t.Fatalf("R5-B2 regression: totalMatches=%d — cancel() fired too late "+
			"(or didn't fire); expected <20k to prove short-circuit",
			totalMatches)
	}
	// At least one chunk skipped entirely (returned without running).
	if chunksWithEmpty < 1 {
		t.Fatalf("R5-B2 regression: %d/%d chunks actually ran; cooperative cancel "+
			"is not skipping sibling work", chunksWithAnyMatches, numChunks)
	}
	// Wall-clock sanity: should finish well under 2s on a modest machine.
	// The intent isn't strict timing but to catch the "all chunks completed"
	// regression where the test would take many-x longer.
	if elapsed > 10*time.Second {
		t.Fatalf("ProcessAllChunks took %v — cooperative cancel not working?",
			elapsed)
	}
	t.Logf("cap=%d, total=%d matches, chunks-with-matches=%d, chunks-empty=%d, elapsed=%v",
		maxResults, totalMatches, chunksWithAnyMatches, chunksWithEmpty, elapsed)
}

// TestProcessAllChunks_NilMaxResultsScansEverything confirms the
// non-cap code path still runs every chunk. Regression guard against
// "my cancel logic accidentally fires when maxResults is nil".
func TestProcessAllChunks_NilMaxResultsScansEverything(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "full-scan.txt")
	var sb strings.Builder
	for i := 1; i <= 1_000; i++ {
		fmt.Fprintf(&sb, "MATCH line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, _ := os.Stat(path)

	const numChunks = 4
	chunkSize := fi.Size() / numChunks
	tasks := make([]FileTask, 0, numChunks)
	for i := 0; i < numChunks; i++ {
		off := int64(i) * chunkSize
		count := chunkSize
		if i == numChunks-1 {
			count = fi.Size() - off
		}
		tasks = append(tasks, FileTask{
			TaskID: i, FilePath: path, Offset: off, Count: count,
		})
	}

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
	// All 1000 lines match. Due to chunk-boundary alignment our
	// hand-rolled offsets don't fall exactly on newlines; ProcessChunk's
	// dedup filter may drop a few at boundaries. Floor of 900 is safe.
	if total < 900 {
		t.Fatalf("nil-cap scan: total=%d, want >=900 (did we skip chunks?)", total)
	}
	t.Logf("nil-cap scan produced %d matches across %d chunks", total, numChunks)
}

// TestProcessAllChunks_MaxResultsLargerThanFile is the non-cap edge
// case: cap set but so large the scan runs to completion anyway. Must
// behave identically to nil-cap (no spurious cancel).
func TestProcessAllChunks_MaxResultsLargerThanFile(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cap-too-big.txt")
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "MATCH line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, _ := os.Stat(path)
	tasks := []FileTask{
		{TaskID: 0, FilePath: path, Offset: 0, Count: fi.Size()},
	}
	cap := 100_000
	allMatches, _, err := ProcessAllChunks(
		context.Background(), tasks,
		map[string]string{"p1": "MATCH"}, []string{"p1"},
		nil, 0, 0,
		&cap,
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
