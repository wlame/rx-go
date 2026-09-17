package trace

import (
	"context"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// longLineLog is a log whose first line is "LINE 1 " followed by xCount
// x's, and whose other lines, up to lineCount, read "LINE n x short".
// With the pattern `x` every line matches, the first one at every
// character after its prefix.
func longLineLog(xCount, lineCount int) []byte {
	var b strings.Builder
	b.WriteString("LINE 1 " + strings.Repeat("x", xCount) + "\n")
	for i := 2; i <= lineCount; i++ {
		b.WriteString("LINE " + strconv.Itoa(i) + " x short\n")
	}
	return []byte(b.String())
}

// traceWithin runs one uncached trace under finishWithin and fails the
// test on an error or a skipped file.
func traceWithin(t *testing.T, path string, patterns []string, opts Options) *rxtypes.TraceResponse {
	t.Helper()
	opts.NoCache = true
	var resp *rxtypes.TraceResponse
	var err error
	finishWithin(t, func(ctx context.Context) {
		resp, err = New().RunWithOptions(ctx, []string{path}, patterns, opts)
	})
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if len(resp.SkippedFiles) != 0 {
		t.Fatalf("skipped_files = %v, want none", resp.SkippedFiles)
	}
	return resp
}

// A line longer than RX_MAX_LINE_TEXT_BYTES is answered with its first
// bytes and at most RX_MAX_SUBMATCHES_PER_LINE submatches, both marked,
// in every place the line appears: as a match and as a line of another
// match's window. Its offset and line number, and every other line of
// the answer, are what an unbounded trace gives. Each storage form takes
// its own path through the engine: plain chunks, a decompressing stream,
// seekable frames.
func TestTraceBoundsALongLineOnEveryStoredForm(t *testing.T) {
	requireRipgrep(t)
	const lineCount = 500
	text := longLineLog(300_000, lineCount)
	firstLine := text[:strings.IndexByte(string(text), '\n')]
	opts := Options{ContextBefore: 1, ContextAfter: 1}
	for _, form := range storedForms() {
		t.Run(form.name, func(t *testing.T) {
			path := form.write(t, text)
			setLineLimits(t, 1<<30, 1<<30)
			whole := traceWithin(t, path, []string{"x"}, opts)
			setLineLimits(t, 4096, 100)
			bounded := traceWithin(t, path, []string{"x"}, opts)

			if len(bounded.Matches) != lineCount || len(whole.Matches) != lineCount {
				t.Fatalf("%d and %d matches, want %d", len(bounded.Matches), len(whole.Matches), lineCount)
			}
			first, wholeFirst := bounded.Matches[0], whole.Matches[0]
			if first.Offset != 0 || first.AbsoluteLineNumber != 1 {
				t.Errorf("first match at offset %d line %d, want 0 and 1", first.Offset, first.AbsoluteLineNumber)
			}
			if *first.LineText != string(firstLine[:4096]) || !first.LineTextTruncated {
				t.Errorf("first match: %d bytes of text (truncated %v), want the line's first 4096, truncated", len(*first.LineText), first.LineTextTruncated)
			}
			if !reflect.DeepEqual(first.Submatches, wholeFirst.Submatches[:100]) || !first.SubmatchesTruncated {
				t.Errorf("first match: %d submatches (truncated %v), want the first 100 of the unbounded answer, truncated", len(first.Submatches), first.SubmatchesTruncated)
			}
			if *wholeFirst.LineText != string(firstLine) || wholeFirst.LineTextTruncated || len(wholeFirst.Submatches) != 300_000 || wholeFirst.SubmatchesTruncated {
				t.Errorf("unbounded first match: %d bytes, %d submatches, flags %v %v", len(*wholeFirst.LineText), len(wholeFirst.Submatches), wholeFirst.LineTextTruncated, wholeFirst.SubmatchesTruncated)
			}
			if !reflect.DeepEqual(bounded.Matches[1:], whole.Matches[1:]) {
				t.Error("the matches after the long line differ from the unbounded answer")
			}
			requireWindowsBoundOnlyTheLongLine(t, bounded, whole, string(firstLine[:4096]))
		})
	}
}

// requireWindowsBoundOnlyTheLongLine checks that bounded's context
// windows hold the long line (offset 0) cut and marked wherever it
// appears, and every other line as whole has it.
func requireWindowsBoundOnlyTheLongLine(t *testing.T, bounded, whole *rxtypes.TraceResponse, cutText string) {
	t.Helper()
	if len(bounded.ContextLines) != len(whole.ContextLines) {
		t.Fatalf("%d windows, want %d", len(bounded.ContextLines), len(whole.ContextLines))
	}
	seen := 0
	for key, window := range bounded.ContextLines {
		wholeWindow := whole.ContextLines[key]
		if len(window) != len(wholeWindow) {
			t.Fatalf("window %s: %d lines, want %d", key, len(window), len(wholeWindow))
		}
		for i, line := range window {
			if line.AbsoluteOffset != 0 {
				if line != wholeWindow[i] {
					t.Errorf("window %s line %d: %+v, want %+v", key, i, line, wholeWindow[i])
				}
				continue
			}
			seen++
			if line.LineText != cutText || !line.LineTextTruncated || line.AbsoluteLineNumber != 1 {
				t.Errorf("window %s: the long line has %d bytes (truncated %v), line %d", key, len(line.LineText), line.LineTextTruncated, line.AbsoluteLineNumber)
			}
		}
	}
	if seen < 2 {
		t.Errorf("the long line is in %d windows, want 2 (its own and the next line's)", seen)
	}
}

// A line on which the pattern matches every character makes an event
// about fifty times the line's size. Under the bounds the trace holds a
// small fixed amount per line instead, on every path. (Without them,
// this 3 MB line is an event of about 150 MB, which the trace read and
// decoded whole: several hundred MB of allocations.)
func TestTraceMemoryStaysBoundedForALineThatMatchesEveryCharacter(t *testing.T) {
	requireRipgrep(t)
	text := longLineLog(3_000_000, 100)
	for _, form := range storedForms() {
		t.Run(form.name, func(t *testing.T) {
			path := form.write(t, text)
			setLineLimits(t, 64*1024, 1000)

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			resp := traceWithin(t, path, []string{"x"}, Options{})
			runtime.ReadMemStats(&after)

			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("the trace allocated %d MiB", allocated>>20)
			if allocated > 16<<20 {
				t.Errorf("the trace allocated %d MiB, want under 16 MiB", allocated>>20)
			}
			if len(resp.Matches) != 100 || len(resp.Matches[0].Submatches) != 1000 || !resp.Matches[0].SubmatchesTruncated {
				t.Errorf("%d matches, the first with %d submatches; want 100 and 1000", len(resp.Matches), len(resp.Matches[0].Submatches))
			}
		})
	}
}

// A long line the bound cuts is credited to the patterns that match the
// whole line, as an unbounded trace credits it: a pattern that matches
// only past the cut is not dropped.
func TestTraceCreditsAPatternThatMatchesOnlyPastTheCut(t *testing.T) {
	requireRipgrep(t)
	text := []byte("LINE 1 " + strings.Repeat("x", 10_000) + " NEEDLE\nLINE 2 x\nLINE 3 NEEDLE\n")
	path := writeTextFile(t, "cut.log", text)
	patterns := []string{"x", "NEEDLE"}

	setLineLimits(t, 1<<30, 1<<30)
	whole := traceWithin(t, path, patterns, Options{})
	setLineLimits(t, 4096, 100)
	bounded := traceWithin(t, path, patterns, Options{})

	pairs := func(resp *rxtypes.TraceResponse) []string {
		var out []string
		for _, m := range resp.Matches {
			out = append(out, m.Pattern+"@"+strconv.FormatInt(m.Offset, 10))
		}
		return out
	}
	if got, want := pairs(bounded), pairs(whole); !reflect.DeepEqual(got, want) {
		t.Errorf("matches %v, want %v", got, want)
	}
}

// A trace whose answer cuts a line is not written to the trace cache: a
// cache hit rebuilds a line's submatches from the text it keeps, so a
// submatch that runs past the cut (`x+` here, which ripgrep reports
// with its true end) would come back ending at the cut. The second
// trace scans again and gives the same answer.
func TestTraceWithACutLineIsNotCached(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	setLineLimits(t, 4096, 100)
	text := longLineLog(1_500_000, 2000)
	path := writeTextFile(t, "cut.log", text)
	patterns := []string{"x+"}

	first := traceOnce(t, path, patterns, Options{ContextBefore: 1, ContextAfter: 1})
	if _, err := os.Stat(CachePath(path, patterns, nil)); err == nil {
		t.Fatal("the trace cache was written for an answer with a cut line")
	}
	second := traceOnce(t, path, patterns, Options{ContextBefore: 1, ContextAfter: 1})
	traceanswer.RequireSame(t, "second trace", second, first)
	if sm := first.Matches[0].Submatches[0]; sm.End != 7+1_500_000 {
		t.Errorf("first submatch ends at %d, want the true end %d", sm.End, 7+1_500_000)
	}
}

// A trace cache written while the bounds were higher (or by a version
// without them) holds a line the bounds now cut. A hit reads that line
// in bounded memory and answers like a scan under the bounds in force.
func TestTraceCacheHitBoundsALineTheCacheHoldsWhole(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	var b strings.Builder
	b.WriteString("LINE 1 NEEDLE " + strings.Repeat("y", 1_500_000) + "\n")
	for i := 2; i <= 3000; i++ {
		b.WriteString("LINE " + strconv.Itoa(i) + " short\n")
	}
	b.WriteString("LINE 3001 NEEDLE\n")
	path := writeTextFile(t, "cached.log", []byte(b.String()))

	setLineLimits(t, 1<<30, 1<<30)
	traceOnce(t, path, []string{"NEEDLE"}, Options{ContextBefore: 1, ContextAfter: 1})
	requireTraceCache(t, path)

	setLineLimits(t, 4096, 100)
	scanned := traceWithin(t, path, []string{"NEEDLE"}, Options{ContextBefore: 1, ContextAfter: 1})
	cached := cappedTraceFromCache(t, path, []string{"NEEDLE"}, Options{ContextBefore: 1, ContextAfter: 1})
	if !scanned.Matches[0].LineTextTruncated {
		t.Fatal("the scan did not cut the long line")
	}
	traceanswer.RequireSame(t, "cache hit", cached, scanned)
}

// Numbering the lines a capped scan left unknown reads a line at a time;
// a line of many megabytes passes through without being held.
func TestCountLinesToOffsets_HoldsNoLongLine(t *testing.T) {
	text := []byte("LINE 1\n" + strings.Repeat("x", 16<<20) + "\nLINE 3\n")
	src := pinForTest(t, writeTextFile(t, "long.log", text))
	wanted := []int64{int64(len(text) - len("LINE 3\n"))}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := countLinesToOffsets(src, rxtypes.LineIndexEntry{LineNumber: 1}, wanted)
	runtime.ReadMemStats(&after)

	if got[wanted[0]] != 3 {
		t.Errorf("numbered %v, want line 3", got)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Errorf("counting allocated %d MiB, want under 2 MiB", allocated>>20)
	}
}

// A cache hit rebuilds at most the cap of submatches and says when it
// left some out.
func TestSubmatchesFromPattern_StopsAtTheCap(t *testing.T) {
	cases := []struct {
		maxHits    int
		wantCount  int
		wantCapped bool
	}{{3, 3, true}, {5, 5, false}, {9, 5, false}}
	for _, tc := range cases {
		subs, capped := submatchesFromPattern("x", "xxxxx", matchFlagsFrom(nil), tc.maxHits)
		if len(subs) != tc.wantCount || capped != tc.wantCapped {
			t.Errorf("cap %d: %d submatches, capped %v; want %d, %v", tc.maxHits, len(subs), capped, tc.wantCount, tc.wantCapped)
		}
	}
}
