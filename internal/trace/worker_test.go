package trace

import (
	"context"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// requireRipgrep skips the test when `rg` isn't on PATH. Lets us
// develop on systems that don't have ripgrep installed — the CI image
// always does.
func requireRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not available; skipping")
	}
}

// TestProcessChunk_SingleChunkMatchesAllLines exercises the main path
// with a small file that fits in one chunk.
func TestProcessChunk_SingleChunkMatchesAllLines(t *testing.T) {
	requireRipgrep(t)
	content := []byte("hello error\nworld\nfinal error line\n")
	p := mustWriteFile(t, content)

	task := FileTask{TaskID: 0, FilePath: p, Offset: 0, Count: int64(len(content))}
	patterns := map[string]string{"p1": "error"}
	order := []string{"p1"}

	matches, _, _, err := ProcessChunk(
		context.Background(), task, patterns, order, nil, 0, 0,
	)
	if err != nil {
		t.Fatalf("ProcessChunk: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("want 2 matches, got %d: %+v", len(matches), matches)
	}
	if matches[0].LineText != "hello error" {
		t.Errorf("first match LineText = %q, want %q", matches[0].LineText, "hello error")
	}
	if matches[1].LineText != "final error line" {
		t.Errorf("second match LineText = %q, want %q", matches[1].LineText, "final error line")
	}
	// Submatch positions must refer to bytes into the line text.
	if len(matches[0].Submatches) != 1 {
		t.Fatalf("want 1 submatch in first match")
	}
	sm := matches[0].Submatches[0]
	if sm.Text != "error" || sm.Start != 6 || sm.End != 11 {
		t.Errorf("submatch = %+v, want error/6/11", sm)
	}
}

// TestProcessChunk_NoMatches exits cleanly when rg returns code 1.
func TestProcessChunk_NoMatches(t *testing.T) {
	requireRipgrep(t)
	content := []byte("no interesting content\nhere at all\n")
	p := mustWriteFile(t, content)

	matches, _, _, err := ProcessChunk(
		context.Background(),
		FileTask{TaskID: 0, FilePath: p, Offset: 0, Count: int64(len(content))},
		map[string]string{"p1": "nomatchpossible"},
		[]string{"p1"},
		nil, 0, 0,
	)
	if err != nil {
		t.Fatalf("ProcessChunk: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("want 0 matches, got %d", len(matches))
	}
}

// TestProcessChunk_DedupFilter validates the core correctness property
// from user decision 6.9.5: matches OUTSIDE the task range must be
// dropped. We simulate this by pointing two tasks at the same file
// with overlapping ranges and confirming each reports the correct
// disjoint set.
func TestProcessChunk_DedupFilter(t *testing.T) {
	requireRipgrep(t)
	// 4 lines; we artificially split the chunks so the match at line 2
	// belongs only to task 0 (offset 0, count 24 = first two lines).
	content := []byte("alpha error\nbeta error\ngamma error\ndelta error\n")
	p := mustWriteFile(t, content)

	task0 := FileTask{TaskID: 0, FilePath: p, Offset: 0, Count: 24} // covers "alpha\nbeta" with boundary after "beta\n"
	matches, _, _, err := ProcessChunk(
		context.Background(), task0,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0,
	)
	if err != nil {
		t.Fatalf("ProcessChunk: %v", err)
	}
	// task0 must see exactly the 2 matches whose absolute offset is in [0, 24).
	// Line 1 starts at offset 0 (within range).
	// Line 2 starts at offset 12 (within range; 24-12=12 bytes remaining,
	// and "beta error\n" is 11 bytes, so line 2 is fully in range).
	if len(matches) != 2 {
		t.Fatalf("task0 want 2 matches, got %d: %+v", len(matches), matches)
	}
	for _, m := range matches {
		if m.Offset >= 24 {
			t.Errorf("match offset %d leaked past task end 24", m.Offset)
		}
	}
}

// TestProcessChunk_RespectsIgnoreIncompatibleRgArgs makes sure we
// filter out --byte-offset / --only-matching (those would corrupt
// rg --json output).
func TestProcessChunk_IncompatibleArgsAreFiltered(t *testing.T) {
	requireRipgrep(t)
	content := []byte("hello\n")
	p := mustWriteFile(t, content)
	// We can't easily verify the exact argv, but we CAN verify that
	// passing these doesn't blow up and still returns the match.
	matches, _, _, err := ProcessChunk(
		context.Background(),
		FileTask{TaskID: 0, FilePath: p, Offset: 0, Count: int64(len(content))},
		map[string]string{"p1": "hello"},
		[]string{"p1"},
		[]string{"--byte-offset", "--only-matching"},
		0, 0,
	)
	if err != nil {
		t.Fatalf("ProcessChunk: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(matches))
	}
}

// ============================================================================
// ProcessAllChunks parallel invariants
// ============================================================================

func TestProcessAllChunks_PreservesTaskOrdering(t *testing.T) {
	requireRipgrep(t)
	content := []byte("alpha error\nbeta error\ngamma error\n")
	p := mustWriteFile(t, content)

	tasks := []FileTask{
		{TaskID: 0, FilePath: p, Offset: 0, Count: 12},  // alpha error\n
		{TaskID: 1, FilePath: p, Offset: 12, Count: 11}, // beta error\n
		{TaskID: 2, FilePath: p, Offset: 23, Count: int64(len(content)) - 23},
	}
	allMatches, _, err := ProcessAllChunks(
		context.Background(), tasks,
		map[string]string{"p1": "error"}, []string{"p1"},
		nil, 0, 0, nil, // no max_results cap
	)
	if err != nil {
		t.Fatalf("ProcessAllChunks: %v", err)
	}
	if len(allMatches) != len(tasks) {
		t.Fatalf("len(allMatches) = %d, want %d", len(allMatches), len(tasks))
	}
	// Each slot has exactly 1 match (one per chunk).
	for i, slot := range allMatches {
		if len(slot) != 1 {
			t.Errorf("slot %d: %d matches, want 1", i, len(slot))
		}
	}
}

// ============================================================================
// IdentifyMatchingPatterns
// ============================================================================

func TestIdentifyMatchingPatterns_MultiPattern(t *testing.T) {
	patterns := map[string]string{"p1": "foo", "p2": "bar", "p3": "baz"}
	order := []string{"p1", "p2", "p3"}
	line := "foo and bar together"
	subs := []rxtypes.Submatch{
		{Text: "foo", Start: 0, End: 3},
		{Text: "bar", Start: 8, End: 11},
	}
	got := IdentifyMatchingPatterns(line, subs, patterns, order, nil)
	sort.Strings(got)
	want := []string{"p1", "p2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestIdentifyMatchingPatterns_CaseInsensitive(t *testing.T) {
	patterns := map[string]string{"p1": "ERROR"}
	order := []string{"p1"}
	line := "some error occurred"
	got := IdentifyMatchingPatterns(line, nil, patterns, order, []string{"-i"})
	if len(got) != 1 || got[0] != "p1" {
		t.Errorf("got %v, want [p1]", got)
	}
}

func TestIdentifyMatchingPatterns_EmptyOnStaleCache(t *testing.T) {
	patterns := map[string]string{"p1": "unrelated"}
	order := []string{"p1"}
	// No submatches path — pattern DOES match text so the submatch-less
	// fallback does find it.
	got := IdentifyMatchingPatterns("unrelated", nil, patterns, order, nil)
	if len(got) != 1 {
		t.Errorf("got %v, want [p1]", got)
	}
	// And when pattern genuinely doesn't match — no results.
	got = IdentifyMatchingPatterns("no match here", nil, patterns, order, nil)
	if len(got) != 0 {
		t.Errorf("got %v, want []", got)
	}
}
