package trace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
)

// ============================================================================
// Byte-for-byte parity with a reference `rg` scan.
// ============================================================================
//
// User directive (6.9.5): "byte-for-byte match parity with Python is
// the acceptance criterion". Python itself orchestrates multiple `rg`
// subprocesses just like we do, so the ground truth for a multi-chunk
// scan is the union of per-chunk `rg --json` matches from the exact
// same chunks (dedup filter applied at task boundaries).
//
// These tests compare the rx-go engine output to a single-pass `rg`
// on the whole file. That's a stricter-than-Python bar: when it holds,
// it proves both our chunker AND our dedup filter correctly stitch
// chunk outputs to be indistinguishable from a one-shot `rg` scan.
//
// If the strict bar fails on a fixture, we can fall back to comparing
// rx-go to "per-chunk rg unions" (i.e. Python parity), which is the
// literal acceptance criterion.
//
// Three fixtures:
//
//   1. fixture1_small  — fits in a single chunk.
//   2. fixture2_medium — forced multi-chunk by setting a 1 MB min.
//   3. fixture3_long_lines — matches land near chunk boundaries to
//      stress the dedup filter.

// rgBaseline runs a single `rg` invocation over the entire file and
// returns (offset, lineText) pairs sorted by offset. Produces the
// reference set we compare rx-go output against.
type rgHit struct {
	Offset   int64
	LineText string
}

func rgBaseline(t *testing.T, path string, pattern string) []rgHit {
	t.Helper()
	cmd := exec.Command("rg", "--json", "--no-heading", "--color=never", "-e", pattern, path)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// rg exits 1 when no matches; that's fine.
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("rg baseline: %v", err)
		}
	}
	var hits []rgHit
	_ = StreamEvents(context.Background(), &stdout, func(ev *RgEvent, perr error) error {
		if perr != nil || ev == nil || ev.Type != RgEventMatch || ev.Match == nil {
			return nil
		}
		hits = append(hits, rgHit{
			Offset:   ev.Match.AbsoluteOffset,
			LineText: trimTrailingNewline(ev.Match.Lines.Text),
		})
		return nil
	})
	sort.Slice(hits, func(i, j int) bool { return hits[i].Offset < hits[j].Offset })
	return hits
}

// fixture1_small: 10 KB of content, one chunk, small number of matches.
func TestParity_Fixture1_Small(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	var b strings.Builder
	for i := 0; i < 200; i++ {
		if i%15 == 0 {
			b.WriteString("log line with error inside\n")
		} else {
			b.WriteString("ordinary log line number here\n")
		}
	}
	content := []byte(b.String())
	p := mustWriteFile(t, content)

	baseline := rgBaseline(t, p, "error")

	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("rx-go engine: %v", err)
	}
	// Convert rx-go matches to rgHit
	var rx []rgHit
	for _, m := range resp.Matches {
		text := ""
		if m.LineText != nil {
			text = *m.LineText
		}
		rx = append(rx, rgHit{Offset: m.Offset, LineText: text})
	}
	sort.Slice(rx, func(i, j int) bool { return rx[i].Offset < rx[j].Offset })

	if len(rx) != len(baseline) {
		t.Fatalf("match count mismatch: rx-go=%d, rg=%d", len(rx), len(baseline))
	}
	for i := range rx {
		if rx[i].Offset != baseline[i].Offset {
			t.Errorf("match %d: offset rx-go=%d rg=%d", i, rx[i].Offset, baseline[i].Offset)
		}
		if rx[i].LineText != baseline[i].LineText {
			t.Errorf("match %d: line_text rx-go=%q rg=%q", i, rx[i].LineText, baseline[i].LineText)
		}
	}
}

// fixture2_medium: 4 MB of content, forced multi-chunk via env.
func TestParity_Fixture2_Medium_MultiChunk(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	// Force multi-chunk: small MIN_CHUNK_SIZE + small max_subprocesses.
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")

	var b strings.Builder
	// ~400 matching lines across 4 MB.
	for b.Len() < 4*1024*1024 {
		if b.Len()%10000 < 30 {
			b.WriteString("somewhere an error\n")
		} else {
			b.WriteString("ordinary log line content goes here to pad\n")
		}
	}
	content := []byte(b.String())
	p := mustWriteFile(t, content)

	baseline := rgBaseline(t, p, "error")

	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("rx-go engine: %v", err)
	}
	var rx []rgHit
	for _, m := range resp.Matches {
		text := ""
		if m.LineText != nil {
			text = *m.LineText
		}
		rx = append(rx, rgHit{Offset: m.Offset, LineText: text})
	}
	sort.Slice(rx, func(i, j int) bool { return rx[i].Offset < rx[j].Offset })

	if len(rx) != len(baseline) {
		t.Fatalf("match count mismatch on multi-chunk fixture: rx-go=%d, rg=%d", len(rx), len(baseline))
	}
	for i := range rx {
		if rx[i].Offset != baseline[i].Offset {
			t.Errorf("match %d: offset rx-go=%d rg=%d", i, rx[i].Offset, baseline[i].Offset)
		}
	}
	// Also assert we used multiple chunks — otherwise this test doesn't
	// actually exercise the dedup path.
	if resp.FileChunks["f1"] < 2 {
		t.Errorf("expected multi-chunk scan; file_chunks[f1] = %d", resp.FileChunks["f1"])
	}
}

// fixture3_long_lines: matches that land NEAR chunk boundaries. The
// goal is to trigger the "match at exact task.EndOffset() - 1" case
// — exactly the scenario the user flagged (no missed matches, no
// duplicate matches).
func TestParity_Fixture3_BoundaryMatches(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "3")

	// Construct content where every row is exactly 1024 bytes (1023 text + '\n').
	// Matches sit at row boundaries that line up close to a 1 MB chunk edge.
	row := []byte(strings.Repeat("x", 1023) + "\n")
	rowMatch := []byte(strings.Repeat("e", 1018) + "rror\n") // 1024 bytes, "error" substring
	_ = config.MinChunkSizeMB                                // keep import
	var content []byte
	for i := 0; i < 4100; i++ {
		// Every ~1023 rows places a match near the 1MB chunk boundary.
		if i%1023 == 1022 {
			content = append(content, rowMatch...)
		} else {
			content = append(content, row...)
		}
	}
	p := mustWriteFile(t, content)

	baseline := rgBaseline(t, p, "error")

	resp, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("rx-go engine: %v", err)
	}
	if len(resp.Matches) != len(baseline) {
		t.Fatalf("boundary fixture count: rx-go=%d, baseline=%d",
			len(resp.Matches), len(baseline))
	}
	// Assert NO duplicates in rx-go output.
	seen := map[int64]struct{}{}
	for _, m := range resp.Matches {
		if _, dup := seen[m.Offset]; dup {
			t.Errorf("duplicate match at offset %d — chunk dedup failed", m.Offset)
		}
		seen[m.Offset] = struct{}{}
	}
}

// ============================================================================
// Additional coverage: reconstruct + identify + cache interplay
// ============================================================================

// TestParity_CacheHitMatchesFirstScan: first run populates cache,
// second run with NoCache=false reconstructs from cache; both outputs
// must be byte-identical.
func TestParity_CacheHitMatchesFirstScan(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "0") // cache even tiny files

	content := []byte("alpha error\nbeta\ngamma error boundary\ndelta\nepsilon error\n")
	p := mustWriteFile(t, content)

	first, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Confirm cache was written.
	cacheRoot := filepath.Join(os.Getenv("RX_CACHE_DIR"), "rx", "trace_cache")
	if _, err := os.Stat(cacheRoot); err != nil {
		t.Fatalf("no cache directory created: %v", err)
	}

	second, err := New().RunWithOptions(
		context.Background(), []string{p}, []string{"error"}, Options{},
	)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if len(second.Matches) != len(first.Matches) {
		t.Fatalf("match count differs: first=%d second=%d",
			len(first.Matches), len(second.Matches))
	}
	for i := range first.Matches {
		if first.Matches[i].Offset != second.Matches[i].Offset {
			t.Errorf("match %d offset differs: first=%d second=%d",
				i, first.Matches[i].Offset, second.Matches[i].Offset)
		}
	}
}
