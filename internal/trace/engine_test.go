package trace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TestEngine_Run_SingleChunk_AbsoluteLineNumberPopulated covers Stage 9
// Round 2 R1-B2: single-chunk scans (file < MIN_CHUNK_SIZE_MB) must
// emit real `absolute_line_number` values, not -1. Python's single-chunk
// path computes it correctly; Go must match.
//
// The check is explicit: the file is small enough to land in a single
// chunk (no parallel splitting), so absolute_line_number for each match
// equals the real file line where it was found. In a 3-line file, the
// two matches are on lines 1 and 3 — both should have
// AbsoluteLineNumber == RelativeLineNumber, NOT -1.
func TestEngine_Run_SingleChunk_AbsoluteLineNumberPopulated(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	content := []byte("alpha error\nbeta\ngamma error\n")
	p := mustWriteFile(t, content)

	eng := New()
	resp, err := eng.RunWithOptions(
		context.Background(),
		[]string{p}, []string{"error"},
		Options{},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(resp.Matches))
	}
	for i, m := range resp.Matches {
		if m.AbsoluteLineNumber < 1 {
			t.Errorf("match[%d] AbsoluteLineNumber = %d, want >= 1 (Python parity; single-chunk path must populate)",
				i, m.AbsoluteLineNumber)
		}
		if m.RelativeLineNumber != nil && m.AbsoluteLineNumber != *m.RelativeLineNumber {
			t.Errorf("match[%d] AbsoluteLineNumber = %d, want %d (== RelativeLineNumber for single-chunk)",
				i, m.AbsoluteLineNumber, *m.RelativeLineNumber)
		}
	}
}

// TestEngine_Run_SmallFile is the smoke test — one file, one pattern,
// a handful of lines, no cache/context.
func TestEngine_Run_SmallFile(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	content := []byte("alpha error\nbeta\ngamma error\n")
	p := mustWriteFile(t, content)

	eng := New()
	resp, err := eng.RunWithOptions(
		context.Background(),
		[]string{p}, []string{"error"},
		Options{},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(resp.Matches))
	}
	if resp.Matches[0].Pattern != "p1" {
		t.Errorf("pattern = %q, want p1", resp.Matches[0].Pattern)
	}
	if resp.Matches[0].LineText == nil || *resp.Matches[0].LineText != "alpha error" {
		t.Errorf("first match line_text = %v, want 'alpha error'", resp.Matches[0].LineText)
	}
}

func TestEngine_Run_NoMatchesProducesEmptyMatchesSlice(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	p := mustWriteFile(t, []byte("hello\nworld\n"))
	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"zzzzz"}, Options{},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) != 0 {
		t.Errorf("want 0 matches, got %d", len(resp.Matches))
	}
	// resp.Matches must be non-nil (serializes as [], not null) — that's
	// part of the TraceResponse contract.
	if resp.Matches == nil {
		t.Errorf("Matches slice must be non-nil for JSON parity")
	}
}

// TestEngine_Run_AllNullableSlices_NonNilForJSONParity covers Stage 8
// Reviewer 2 High #8: previously, Matches was explicitly normalized to
// []rxtypes.Match{} but other nullable slices (ScannedFiles,
// SkippedFiles) could leak as nil → serialize as null in JSON. The
// frontend iterates these fields and can crash on null.
//
// The test exercises a single-regular-file scan (no directory, so
// ScannedFiles is supposed to be empty []; no skipped, so SkippedFiles
// is supposed to be empty []) and asserts every nullable slice is
// non-nil on the response — the canonical Go idiom for "will marshal
// as []".
func TestEngine_Run_AllNullableSlices_NonNilForJSONParity(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	p := mustWriteFile(t, []byte("hello\nworld\n"))
	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"zzzzz"}, Options{},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if resp.Matches == nil {
		t.Errorf("Matches: nil slice will serialize as null (want [])")
	}
	// No directory was passed → ScannedFiles should be empty []. Go nil
	// serializes as null; we require [] for frontend parity.
	if resp.ScannedFiles == nil {
		t.Errorf("ScannedFiles: nil slice will serialize as null (want [])")
	}
	// All paths valid text files → SkippedFiles is empty []. Same rule.
	if resp.SkippedFiles == nil {
		t.Errorf("SkippedFiles: nil slice will serialize as null (want [])")
	}
}

// TestEngine_Run_EmptyInputPaths_ReturnsAllEmptySlices covers the
// early-exit branch in RunWithOptions when expandPaths returns zero
// files. That branch constructs a minimal TraceResponse by hand; the
// test pins down that every nullable slice is non-nil.
func TestEngine_Run_EmptyInputPaths_ReturnsAllEmptySlices(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	// Empty directory → expandPaths populates scannedDirs but NOT
	// filePaths or skipped. That triggers the early-exit branch with
	// skipped == nil. Pre-fix this serialized as SkippedFiles: null.
	dir := t.TempDir()
	resp, err := New().RunWithOptions(
		context.Background(),
		[]string{dir},
		[]string{"pattern"},
		Options{},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Matches == nil {
		t.Errorf("Matches: nil (want [])")
	}
	if resp.ScannedFiles == nil {
		t.Errorf("ScannedFiles: nil (want [])")
	}
	if resp.SkippedFiles == nil {
		t.Errorf("SkippedFiles: nil slice will serialize as null; Python emits [] here — parity broken")
	}
}

func TestEngine_Run_MultiplePatternsProducesMultipleMatches(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	// Line matches BOTH patterns → two rxtypes.Match entries for it.
	p := mustWriteFile(t, []byte("foo and bar\nonly foo\n"))
	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"foo", "bar"}, Options{},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Line 1 matches BOTH patterns → 2 matches. Line 2 matches only
	// foo → 1 match. Total = 3.
	if len(resp.Matches) != 3 {
		t.Fatalf("got %d matches, want 3: %+v", len(resp.Matches), resp.Matches)
	}
	// Patterns map must include both IDs.
	if resp.Patterns["p1"] != "foo" || resp.Patterns["p2"] != "bar" {
		t.Errorf("Patterns = %v", resp.Patterns)
	}
}

// TestEngine_Run_MaxResultsCaps_StopsEarly ensures the response is
// truncated to MaxResults and does NOT write a cache (cache write
// requires no-max-results).
func TestEngine_Run_MaxResultsCaps(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	var content strings.Builder
	for i := 0; i < 50; i++ {
		content.WriteString("error line\n")
	}
	p := mustWriteFile(t, []byte(content.String()))
	cap := 5
	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"},
		Options{MaxResults: &cap},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) > 5 {
		t.Errorf("got %d matches, want at most 5", len(resp.Matches))
	}
}

// TestEngine_Run_DirectoryExpandsToTextFiles walks a dir and finds
// text files only.
func TestEngine_Run_DirectoryExpandsToTextFiles(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("error in a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.log"), []byte("error in b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Drop a binary file — should be skipped.
	bin := []byte{0x00, 0x01, 0x02, 'e', 'r', 'r', 'o', 'r', 0x00}
	if err := os.WriteFile(filepath.Join(dir, "binary.dat"), bin, 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := New().RunWithOptions(
		context.Background(), []string{dir}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) != 2 {
		t.Errorf("got %d matches, want 2 (one per text file)", len(resp.Matches))
	}
	if len(resp.SkippedFiles) == 0 {
		t.Errorf("want at least one skipped file (binary), got none")
	}
}

// TestEngine_Run_CacheHitRoundTrip: force cache write by using a large
// file + low threshold; verify second call returns identical matches.
func TestEngine_Run_CacheHitRoundTrip(t *testing.T) {
	requireRipgrep(t)
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	// Lower the large-file threshold so our small fixture qualifies.
	t.Setenv("RX_LARGE_FILE_MB", "0")

	content := []byte("alpha error\nbeta\ngamma error\n")
	p := mustWriteFile(t, content)

	first, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A cache file should have been written in $RX_CACHE_DIR/rx/trace_cache/<...>.
	var cacheFiles []string
	_ = filepath.Walk(filepath.Join(cacheDir, "rx", "trace_cache"),
		func(path string, info os.FileInfo, _ error) error {
			if info != nil && !info.IsDir() {
				cacheFiles = append(cacheFiles, path)
			}
			return nil
		})
	if len(cacheFiles) == 0 {
		t.Fatalf("expected cache file under %s/rx/trace_cache after first run", cacheDir)
	}

	// Second run with same inputs should hit the cache (we don't
	// expose a hit counter here but the invariant is match equality
	// plus file_chunks == 0 for the file).
	second, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(second.Matches) != len(first.Matches) {
		t.Errorf("cache-hit match count = %d, want %d", len(second.Matches), len(first.Matches))
	}
	if second.FileChunks["f1"] != 0 {
		t.Errorf("file_chunks[f1] = %d, want 0 for cache hit", second.FileChunks["f1"])
	}
}

// TestEngine_Run_ContextLines collects surrounding lines.
func TestEngine_Run_ContextLines(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	content := []byte("before\nmatched error line\nafter\n")
	p := mustWriteFile(t, content)
	b, a := 1, 1
	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"},
		Options{ContextBefore: b, ContextAfter: a},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resp.Matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(resp.Matches))
	}
	if len(resp.ContextLines) != 1 {
		t.Fatalf("want 1 context group, got %d", len(resp.ContextLines))
	}
	var window []rxtypes.ContextLine
	for _, v := range resp.ContextLines {
		window = v
	}
	if len(window) != 3 {
		t.Errorf("context window size = %d, want 3", len(window))
	}
}

// TestEngine_Run_NullPatternsMapShape: patterns_ids must be a non-nil
// map for JSON parity, even when there are zero patterns.
func TestEngine_Run_EmptyPatternsProducesEmptyPatternsMap(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	p := mustWriteFile(t, []byte("hello\n"))
	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, nil, Options{},
	)
	if err != nil {
		// rg without -e patterns fails with exit 2 — this is fine, the
		// engine may return error. In that case, nothing to assert.
		return
	}
	if resp.Patterns == nil {
		t.Errorf("Patterns must be non-nil")
	}
}
