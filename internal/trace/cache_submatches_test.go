package trace

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A trace-cache hit gives every match the submatches the scan gave it.
// The entry stores each match's spans, so a hit never runs the pattern
// again with an engine other than ripgrep's: PCRE2 look-around, Unicode
// \b, \w and -w, -w around punctuation, `\r$` on a CRLF line and a line
// that is not valid UTF-8 all come back as the scan found them.

// submatchFixtureText is a log of about size bytes whose lines exercise
// the places where Go's regexp and ripgrep disagree: Cyrillic words,
// words that end in punctuation, CRLF line breaks, bytes that are not
// UTF-8, and a line that two patterns match at different places. Each
// line also holds random hex, so a seekable-zstd copy stays over the
// 1 MB a compressed file needs before its scan is cached.
func submatchFixtureText(size int) []byte {
	shapes := []string{
		"LINE %d %x журнал ошибка NEEDLE x\n",
		"LINE %d %x Agent: started Agent:x\n",
		"LINE %d %x crlf line ОШИБКА\r\n",
		"LINE %d %x cut \xff ERROR bad\n",
		"LINE %d %x x AAA y BBB\n",
		"LINE %d %x filler text\n",
		"LINE %d %x ошибкаслитно word\n",
	}
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // reproducible fixture bytes, not a secret
	// Long lines keep the match count, and so the test's run time, low.
	hex := make([]byte, 96)
	var b bytes.Buffer
	for line := 1; b.Len() < size; line++ {
		_, _ = rng.Read(hex)
		fmt.Fprintf(&b, shapes[line%len(shapes)], line, hex)
	}
	return b.Bytes()
}

// submatchCases are the searches whose hit used to differ from the scan,
// and two that always agreed (-i on Cyrillic, plain two patterns).
var submatchCases = []struct {
	name     string
	patterns []string
	flags    []string
}{
	{"two patterns on one line", []string{"BBB", "AAA"}, nil},
	{"pcre2 look-ahead", []string{"NEEDLE(?= x)"}, []string{"-P"}},
	{"pcre2 look-behind and look-ahead in two patterns", []string{"(?<=y )BBB", "NEEDLE(?= x)"}, []string{"-P"}},
	{"unicode word boundary", []string{`\bошибка\b`}, nil},
	{"whole word, cyrillic", []string{"ошибка"}, []string{"-w"}},
	{"ignore case, cyrillic", []string{"ОШИБКА"}, []string{"-i"}},
	{"whole word ending in punctuation", []string{"Agent:"}, []string{"-w"}},
	{"unicode word class beside an ascii pattern", []string{`\w+ка`, "ERROR"}, nil},
	{"carriage return at the end of a crlf line", []string{`\r$`}, nil},
	{"pattern across a byte that is not utf-8", []string{`cut (?-u:.) ERROR`}, nil},
}

// requireSubmatches fails t unless every match of the answer has at least
// one submatch, so a case cannot pass on two answers that both lost them.
func requireSubmatches(t *testing.T, resp *rxtypes.TraceResponse) {
	t.Helper()
	if len(resp.Matches) == 0 {
		t.Fatal("the fixture has no match")
	}
	for _, m := range resp.Matches {
		if len(m.Submatches) == 0 {
			t.Fatalf("the scan's match at offset %d has no submatch", m.Offset)
		}
	}
}

func TestCacheHitGivesEveryMatchTheScansSubmatches(t *testing.T) {
	path := writeTextFile(t, "submatches.log", submatchFixtureText(3<<20))
	for _, tc := range submatchCases {
		t.Run(tc.name, func(t *testing.T) {
			largeFileCacheEnv(t)
			opts := Options{RgExtraArgs: tc.flags, ContextBefore: 1, ContextAfter: 1}
			written := traceOnce(t, path, tc.patterns, opts)
			if written.FileChunks["f1"] < 2 {
				t.Fatalf("fixture scanned in %d chunks; the test needs several", written.FileChunks["f1"])
			}

			hit := traceThroughCache(t, path, tc.patterns, opts)
			noCache := opts
			noCache.NoCache = true
			scan := traceOnce(t, path, tc.patterns, noCache)
			requireEveryPatternMatches(t, scan)
			requireSubmatches(t, scan)
			traceanswer.RequireSame(t, "cache hit", hit, scan)
		})
	}
}

// A seekable-zstd file's hit, read through the frames its entry names,
// gives the scan's submatches too.
func TestSeekableCacheHitGivesEveryMatchTheScansSubmatches(t *testing.T) {
	largeFileCacheEnv(t)
	path := filepath.Join(t.TempDir(), "submatches.log.zst")
	text := submatchFixtureText(3 << 20)
	seekablefile.Write(t, path, seekablefile.SplitEvery(text, 256<<10))
	if info, err := os.Stat(path); err != nil || info.Size() < 1<<20 {
		t.Fatalf("the seekable fixture must be at least 1 MB to be cached (stat: %v, %v)", info, err)
	}
	opts := Options{RgExtraArgs: []string{"-P"}}
	patterns := []string{"NEEDLE(?= x)", `\w+ка`}

	traceOnce(t, path, patterns, opts)
	hit := traceThroughCache(t, path, patterns, opts)
	noCache := opts
	noCache.NoCache = true
	scan := traceOnce(t, path, patterns, noCache)
	requireEveryPatternMatches(t, scan)
	requireSubmatches(t, scan)
	traceanswer.RequireSame(t, "seekable cache hit", hit, scan)
}

// An entry whose records carry no spans (written before entries stored
// them) or carry spans no line can hold is a miss: the trace scans and
// answers as the scan does.
func TestCacheEntryWithoutUsableSubmatchSpansIsAMiss(t *testing.T) {
	patterns := []string{"NEEDLE(?= x)"}
	flags := []string{"-P"}
	cases := []struct {
		name   string
		change func(*rxtypes.TraceCacheData)
	}{
		{"no spans on any record", func(d *rxtypes.TraceCacheData) {
			for i := range d.Matches {
				d.Matches[i].Submatches = nil
			}
		}},
		{"no spans on one record", func(d *rxtypes.TraceCacheData) {
			d.Matches[len(d.Matches)/2].Submatches = nil
		}},
		{"a span that starts before the line", func(d *rxtypes.TraceCacheData) {
			d.Matches[0].Submatches = [][2]int{{-1, 3}}
		}},
		{"a span that ends before it starts", func(d *rxtypes.TraceCacheData) {
			d.Matches[0].Submatches = [][2]int{{5, 2}}
		}},
		{"spans out of order", func(d *rxtypes.TraceCacheData) {
			d.Matches[0].Submatches = [][2]int{{9, 12}, {2, 4}}
		}},
		{"overlapping spans", func(d *rxtypes.TraceCacheData) {
			d.Matches[0].Submatches = [][2]int{{2, 6}, {4, 8}}
		}},
	}
	path := writeTextFile(t, "submatches.log", submatchFixtureText(3<<20))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			largeFileCacheEnv(t)
			opts := Options{RgExtraArgs: flags}
			traceOnce(t, path, patterns, opts)
			rewriteCacheEntry(t, CachePath(path, patterns, flags), tc.change)

			if _, err := GetCachedScan(path, patterns, flags); err == nil {
				t.Fatal("the entry was accepted")
			}
			got := traceOnce(t, path, patterns, opts)
			opts.NoCache = true
			scan := traceOnce(t, path, patterns, opts)
			traceanswer.RequireSame(t, "trace over an entry without usable spans", got, scan)
		})
	}
}

// A span that runs past the end of its line names bytes the line does not
// have. The hit cannot answer it and reports the file as skipped rather
// than drop the match or invent its text.
func TestCacheHitWithASpanPastItsLineSkipsTheFile(t *testing.T) {
	largeFileCacheEnv(t)
	path := writeTextFile(t, "submatches.log", submatchFixtureText(3<<20))
	patterns := []string{"NEEDLE"}
	traceOnce(t, path, patterns, Options{})
	rewriteCacheEntry(t, CachePath(path, patterns, nil), func(d *rxtypes.TraceCacheData) {
		d.Matches[0].Submatches = [][2]int{{0, 1 << 20}}
	})

	got := traceOnce(t, path, patterns, Options{})
	if len(got.SkippedFiles) != 1 || len(got.Matches) != 0 {
		t.Fatalf("skipped %v with %d matches; want the file skipped and no match", got.SkippedFiles, len(got.Matches))
	}
}

// denseFiller pads each line of the dense fixtures, so they pass the
// 1 MB cache threshold with few lines and few matches.
var denseFiller = strings.Repeat(".", 1000)

// denseLineLog is a log of about size bytes in which every line holds
// perLine matches of `x`.
func denseLineLog(size, perLine int) []byte {
	var b strings.Builder
	for line := 1; b.Len() < size; line++ {
		fmt.Fprintf(&b, "LINE %d %s %s\n", line, denseFiller, strings.Repeat("x ", perLine))
	}
	return []byte(b.String())
}

// A scan that leaves submatches out of a match (more than
// RX_MAX_SUBMATCHES_PER_LINE on one line) is not written to the cache:
// an entry holds every span of every match, so a hit under any bound
// answers as a scan under that bound. The second trace scans again and
// gives the same answer.
func TestTraceThatLeavesSubmatchesOutIsNotCached(t *testing.T) {
	largeFileCacheEnv(t)
	setLineLimits(t, 1<<20, 3)
	path := writeTextFile(t, "dense.log", denseLineLog(3<<19, 5))
	patterns := []string{"x"}

	first := traceOnce(t, path, patterns, Options{})
	if !first.Matches[0].SubmatchesTruncated {
		t.Fatal("the scan kept every submatch; the fixture tests nothing")
	}
	if _, err := os.Stat(CachePath(path, patterns, nil)); err == nil {
		t.Fatal("the trace cache was written for an answer that leaves submatches out")
	}
	second := traceOnce(t, path, patterns, Options{})
	traceanswer.RequireSame(t, "second trace", second, first)
}

// Each match carries its own pattern's submatches, and one pattern can
// find more spans on a line than all the patterns together: on "xxxxxx",
// `xx|x` finds three spans and `x` alone six. Under a bound of three the
// answer leaves some of x's out although the scan's own list is whole, so
// the trace is not cached either.
func TestTraceThatLeavesOnePatternsSubmatchesOutIsNotCached(t *testing.T) {
	largeFileCacheEnv(t)
	setLineLimits(t, 1<<20, 3)
	var b strings.Builder
	for line := 1; b.Len() < 3<<19; line++ {
		fmt.Fprintf(&b, "LINE %d %s xxxxxx\n", line, denseFiller)
	}
	path := writeTextFile(t, "dense.log", []byte(b.String()))
	patterns := []string{"xx", "x"}

	first := traceOnce(t, path, patterns, Options{})
	if !slices.ContainsFunc(first.Matches, func(m rxtypes.Match) bool { return m.SubmatchesTruncated }) {
		t.Fatal("no match left submatches out; the fixture tests nothing")
	}
	if _, err := os.Stat(CachePath(path, patterns, nil)); err == nil {
		t.Fatal("the trace cache was written for an answer that leaves submatches out")
	}
	second := traceOnce(t, path, patterns, Options{})
	traceanswer.RequireSame(t, "second trace", second, first)
}

// An entry written under a higher submatch bound holds more spans per
// line than the bound in force when it is read. The hit keeps the first
// ones and marks the list, as a scan under that bound does.
func TestCacheHitUnderALowerSubmatchBoundAnswersAsTheScan(t *testing.T) {
	largeFileCacheEnv(t)
	path := writeTextFile(t, "dense.log", denseLineLog(3<<19, 5))
	patterns := []string{"x"}
	traceOnce(t, path, patterns, Options{})

	setLineLimits(t, 1<<20, 3)
	hit := cappedTraceFromCache(t, path, patterns, Options{})
	scan := traceOnce(t, path, patterns, Options{NoCache: true})
	if !scan.Matches[0].SubmatchesTruncated || len(scan.Matches[0].Submatches) != 3 {
		t.Fatalf("the scan kept %d submatches; the fixture tests nothing", len(scan.Matches[0].Submatches))
	}
	traceanswer.RequireSame(t, "cache hit", hit, scan)
}

// An entry written while the line bound was higher holds a span that runs
// past the point where the bound now cuts its line. The hit keeps the
// span's true start and end and the part of its text the cut line holds,
// as a scan under that bound does.
func TestCacheHitKeepsASubmatchThatCrossesTheCutAsTheScanDoes(t *testing.T) {
	largeFileCacheEnv(t)
	path := writeTextFile(t, "cut.log", longLineLog(1_500_000, 2000))
	patterns := []string{"x+"}
	setLineLimits(t, 1<<30, 1<<30)
	traceOnce(t, path, patterns, Options{})
	if _, err := os.Stat(CachePath(path, patterns, nil)); err != nil {
		t.Fatalf("no trace cache: %v", err)
	}

	setLineLimits(t, 4096, 100)
	hit := cappedTraceFromCache(t, path, patterns, Options{})
	scan := traceWithin(t, path, patterns, Options{})
	if sm := scan.Matches[0].Submatches[0]; sm.End != 7+1_500_000 || len(sm.Text) >= 4096 {
		t.Fatalf("the scan's first submatch is [%d, %d) with %d bytes of text; the fixture tests nothing", sm.Start, sm.End, len(sm.Text))
	}
	traceanswer.RequireSame(t, "cache hit", hit, scan)
}
