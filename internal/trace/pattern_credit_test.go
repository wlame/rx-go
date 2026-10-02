package trace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// creditedLine is what an answer says about one line for one pattern:
// where the line starts and that pattern's own submatches on it.
type creditedLine struct {
	Offset              int64
	Line                int
	Submatches          []rxtypes.Submatch
	SubmatchesTruncated bool
}

// linesCreditedTo returns the lines resp credits to pattern pid, by
// offset.
func linesCreditedTo(resp *rxtypes.TraceResponse, pid string) []creditedLine {
	var out []creditedLine
	for _, m := range resp.Matches {
		if m.Pattern != pid {
			continue
		}
		out = append(out, creditedLine{
			Offset:              m.Offset,
			Line:                m.AbsoluteLineNumber,
			Submatches:          m.Submatches,
			SubmatchesTruncated: m.SubmatchesTruncated,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

// lineNumbersOf lists the line numbers of lines.
func lineNumbersOf(lines []creditedLine) []int {
	out := []int{}
	for _, l := range lines {
		out = append(out, l.Line)
	}
	return out
}

// creditsByPattern is the answer seen pattern by pattern: for each
// pattern's text, the lines credited to it.
func creditsByPattern(resp *rxtypes.TraceResponse) map[string][]creditedLine {
	out := map[string][]creditedLine{}
	for pid, pattern := range resp.Patterns {
		out[pattern] = linesCreditedTo(resp, pid)
	}
	return out
}

// requireEachPatternAnsweredAlone checks that the lines and submatches
// resp credits to each pattern are exactly what a trace of that pattern
// alone answers, under the same options.
func requireEachPatternAnsweredAlone(t *testing.T, path string, resp *rxtypes.TraceResponse, opts Options) {
	t.Helper()
	for pid, pattern := range resp.Patterns {
		alone := traceOnce(t, path, []string{pattern}, opts)
		got, want := linesCreditedTo(resp, pid), linesCreditedTo(alone, "p1")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("pattern %s %q: credited %+v, alone it matches %+v", pid, pattern, got, want)
		}
	}
}

// ripgrep runs the patterns of a search as one alternation and reports
// the spans of the alternation, so its answer cannot say which pattern
// matched a line. Each pattern is credited with exactly the lines it
// matches alone, as ripgrep matches them, with its own submatches,
// whatever the other patterns are and in whichever order they come.
func TestTraceCreditsEachPatternWithTheLinesItMatchesAlone(t *testing.T) {
	requireRipgrep(t)
	cases := []struct {
		name     string
		text     string
		patterns []string
		flags    []string
		// want lists, per pattern in order, the lines credited to it.
		want [][]int
	}{
		{"prefix overlap, longer first", "NEEDLE here\nNEED only\nnothing\n", []string{"NEEDLE", "NEED"}, nil, [][]int{{1}, {1, 2}}},
		{"prefix overlap, shorter first", "NEEDLE here\nNEED only\nnothing\n", []string{"NEED", "NEEDLE"}, nil, [][]int{{1, 2}, {1}}},
		{"same span from two regexes", "an ERROR here\nERRAR\n", []string{"ERR.R", "ERROR"}, nil, [][]int{{1, 2}, {1}}},
		{"overlapping spans, neither inside the other", "abcd\nabc\nbcd\n", []string{"abc", "bcd"}, nil, [][]int{{1, 2}, {1, 3}}},
		{"ignore case", "Error here\nerror\nnone\n", []string{"ERROR", "zzz"}, []string{"-i"}, [][]int{{1, 2}, {}}},
		{"whole word", "NEEDLES and NEED\nNEEDLE\n", []string{"NEEDLE", "NEED"}, []string{"-w"}, [][]int{{2}, {1}}},
		{"whole word, punctuation at the edge", "Agent: start\nclosed, done\n", []string{"Agent:", "closed,"}, []string{"-w"}, [][]int{{1}, {2}}},
		{"whole word, longer alternative", "ab cd\na\n", []string{"a|ab", "cd"}, []string{"-w"}, [][]int{{1, 2}, {1}}},
		{"fixed strings", "call foo( now\na.b\naxb\n", []string{"foo(", "a.b"}, []string{"-F"}, [][]int{{1}, {2}}},
		{"fixed strings, ignore case", "CALL FOO( NOW\nx\n", []string{"foo(", "x"}, []string{"-i", "-F"}, [][]int{{1}, {2}}},
		{"whole line", "ab\na\n", []string{"a|ab", "a"}, []string{"-x"}, [][]int{{1, 2}, {2}}},
		{"Unicode word class", "LINE 1 журнал ошибка done\nLINE 2 ZZZ\n", []string{`\w+ка`, "ZZZ"}, nil, [][]int{{1}, {2}}},
		{"Unicode word boundary", "x ошибка y\nZZZ\n", []string{`\bошибка\b`, "ZZZ"}, nil, [][]int{{1}, {2}}},
		{"Unicode digit", "code ٣٤ end\nZZZ\n", []string{`code \d+`, "ZZZ"}, nil, [][]int{{1}, {2}}},
		{"CRLF line ends", "LINE 1 foo\r\nLINE 2 bar\r\n", []string{`foo\r$`, "bar"}, nil, [][]int{{1}, {2}}},
		{"line that is not valid UTF-8", "\xff\xfe ERR bad\nZZZ\n", []string{`\W+ERR`, "ZZZ"}, nil, [][]int{{1}, {2}}},
		{"PCRE2 look-ahead", "error two\nerror one\nzzz\n", []string{"error(?= two)", "zzz"}, []string{"-P"}, [][]int{{1}, {3}}},
		{"PCRE2 look-behind", "an error\nthe error\n", []string{"(?<=an )error", "error"}, []string{"-P"}, [][]int{{1}, {1, 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if slices.Contains(tc.flags, "-P") {
				requirePCRE2(t)
			}
			path := writeTextFile(t, "credit.log", []byte(tc.text))
			opts := Options{RgExtraArgs: tc.flags, NoCache: true}

			resp := traceOnce(t, path, tc.patterns, opts)
			for i, wantLines := range tc.want {
				pid := patternIDOf(t, resp, tc.patterns[i])
				if got := lineNumbersOf(linesCreditedTo(resp, pid)); !slices.Equal(got, wantLines) {
					t.Errorf("pattern %q is credited lines %v, want %v", tc.patterns[i], got, wantLines)
				}
			}
			requireEachPatternAnsweredAlone(t, path, resp, opts)

			reversed := slices.Clone(tc.patterns)
			slices.Reverse(reversed)
			if got, want := creditsByPattern(traceOnce(t, path, reversed, opts)), creditsByPattern(resp); !reflect.DeepEqual(got, want) {
				t.Errorf("the patterns in reverse order credit %+v, want %+v", got, want)
			}
		})
	}
}

// patternIDOf is the ID resp gives the pattern text.
func patternIDOf(t *testing.T, resp *rxtypes.TraceResponse, pattern string) string {
	t.Helper()
	for pid, p := range resp.Patterns {
		if p == pattern {
			return pid
		}
	}
	t.Fatalf("pattern %q is not in the answer", pattern)
	return ""
}

// requirePCRE2 skips the test when ripgrep was built without PCRE2.
func requirePCRE2(t *testing.T) {
	t.Helper()
	if err := exec.Command("rg", "--pcre2-version").Run(); err != nil {
		t.Skipf("ripgrep has no PCRE2: %v", err)
	}
}

// A line longer than RX_MAX_LINE_TEXT_BYTES is answered with its first
// bytes, but which patterns it is credited to is decided on the whole
// line, read again from the file: a pattern that matches only past the
// cut is credited, one that matches nowhere on the line is not, and one
// that would match where the kept text ends (`x$`) but not where the
// line ends is not either. Each storage form reads the line its own way.
func TestTraceCreditsACutLineByTheWholeLine(t *testing.T) {
	requireRipgrep(t)
	text := []byte("LINE 1 " + strings.Repeat("x", 10_000) + " NEEDLE\nLINE 2 x\nLINE 3 NEEDLE\n")
	patterns := []string{"x", "NEEDLE", "x$", "ZZZ"}
	want := [][]int{{1, 2}, {1, 3}, {2}, {}}
	for _, form := range storedForms() {
		t.Run(form.name, func(t *testing.T) {
			path := form.write(t, text)
			setLineLimits(t, 4096, 100)
			resp := traceWithin(t, path, patterns, Options{})
			if !resp.Matches[0].LineTextTruncated {
				t.Fatal("the first line is not cut")
			}
			for i, wantLines := range want {
				if got := lineNumbersOf(linesCreditedTo(resp, patternIDOf(t, resp, patterns[i]))); !slices.Equal(got, wantLines) {
					t.Errorf("pattern %q is credited lines %v, want %v", patterns[i], got, wantLines)
				}
			}
			requireEachPatternAnsweredAlone(t, path, resp, Options{NoCache: true})
		})
	}
}

// A trace-cache hit labels each line with the patterns the scan
// credited it to, so the hit and the scan agree, and a pattern that
// shares a prefix with another keeps its lines on both.
func TestTraceCacheHitCreditsThePatternsTheScanCredited(t *testing.T) {
	largeFileCacheEnv(t)
	var b strings.Builder
	for i := 1; b.Len() < 3<<20; i++ {
		switch i % 500 {
		case 0:
			b.WriteString("LINE NEEDLE here журнал ошибка\n")
		case 250:
			b.WriteString("LINE NEED only\n")
		default:
			b.WriteString("LINE filler text\n")
		}
	}
	path := writeTextFile(t, "cached.log", []byte(b.String()))

	for _, patterns := range [][]string{{"NEEDLE", "NEED"}, {`\w+ка`, "ZZZ"}} {
		scanned := traceOnce(t, path, patterns, Options{})
		if _, err := os.Stat(CachePath(path, patterns, nil)); err != nil {
			t.Fatalf("no trace cache for %v: %v", patterns, err)
		}
		cached := cappedTraceFromCache(t, path, patterns, Options{})
		if got, want := patternLabels(cached), patternLabels(scanned); !reflect.DeepEqual(got, want) {
			t.Errorf("%v: the cache hit labels %d matches, the scan %d", patterns, len(got), len(want))
		}
		traceanswer.RequireSame(t, "cache hit", cached, scanned)
	}
	scanned := traceOnce(t, path, []string{"NEEDLE", "NEED"}, Options{})
	if needle, need := len(linesCreditedTo(scanned, "p1")), len(linesCreditedTo(scanned, "p2")); need != 2*needle {
		t.Errorf("NEEDLE is credited %d lines and NEED %d, want NEED on twice as many", needle, need)
	}
	if zzz := linesCreditedTo(traceOnce(t, path, []string{`\w+ка`, "ZZZ"}, Options{}), "p2"); len(zzz) != 0 {
		t.Errorf("ZZZ is credited %d lines, want none", len(zzz))
	}
}

// patternLabels is the set of (pattern, offset) pairs of an answer.
func patternLabels(resp *rxtypes.TraceResponse) map[string]bool {
	out := map[string]bool{}
	for _, m := range resp.Matches {
		out[m.Pattern+"@"+strconv.FormatInt(m.Offset, 10)] = true
	}
	return out
}

// A line ripgrep reported but no pattern matches alone cannot be
// credited to anyone. That only happens when the line changed between
// the scan and the check (here: a cut line, read again from the file,
// edited in place), and it is an error, never a line credited to every
// pattern.
func TestCreditPatterns_ALineNoPatternMatchesIsAnError(t *testing.T) {
	requireRipgrep(t)
	setLineLimits(t, 16, 100)
	text := []byte("LINE 1 NEEDLE and some more text\nLINE 2\n")
	path := writeTextFile(t, "changed.log", text)
	src := pinForTest(t, path)
	edited := []byte("LINE 1 nothing and some more text\nLINE 2\n")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatalf("edit: %v", err)
	}

	cut := MatchRaw{Offset: 0, End: int64(strings.IndexByte(string(text), '\n') + 1), LineText: "LINE 1 NEEDLE an", LineTextTruncated: true}
	got, err := creditPatterns(context.Background(), creditRequest{
		files:        []creditFile{{source: src, lines: []MatchRaw{cut}}},
		patternIDs:   map[string]string{"p1": "NEEDLE", "p2": "ZZZ"},
		patternOrder: []string{"p1", "p2"},
	})
	if err != nil {
		t.Fatalf("err = %v, want the failure kept to the file", err)
	}
	if !errors.Is(got[0].err, errLineMatchesNoPattern) {
		t.Fatalf("err = %v, want errLineMatchesNoPattern", got[0].err)
	}
}

// The whole lines of every file of a trace are checked together, so a
// directory of many small files costs one ripgrep run per pattern, not
// one per file and pattern. A file's cut lines are read again from that
// file, in a batch of their own.
func TestPlanCreditBatches_PoolsTheWholeLinesOfEveryFile(t *testing.T) {
	var files []creditFile
	for i := 0; i < 100; i++ {
		files = append(files, creditFile{lines: []MatchRaw{{LineText: "NEEDLE"}, {LineText: "NEED"}}})
	}
	files[7].lines = append(files[7].lines, MatchRaw{LineText: "LINE x", LineTextTruncated: true})

	batches := planCreditBatches(files)
	if len(batches) != 2 {
		t.Fatalf("%d batches, want 2 (every whole line, then file 7's cut line)", len(batches))
	}
	if whole := batches[0]; whole.fromSource || len(whole.lines) != 200 {
		t.Errorf("first batch: %d lines, from the file %v; want the 200 whole lines, from memory", len(whole.lines), whole.fromSource)
	}
	if cut := batches[1]; !cut.fromSource || len(cut.lines) != 1 || cut.lines[0] != (lineRef{file: 7, line: 2}) {
		t.Errorf("second batch: %+v, want file 7's line 2, read from the file", cut)
	}
}

// A directory search credits every file's lines as a search of that
// file alone does, and a file whose lines cannot be credited is skipped
// without losing the others.
func TestTraceCreditsEveryFileOfADirectory(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	for i, text := range []string{"NEEDLE here\nNEED only\n", "nothing\nNEED NEEDLE\n", "x ошибка\nNEED\n"} {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)+".log"), []byte(text), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	patterns := []string{"NEEDLE", "NEED", `\w+ка`}
	resp := traceOnce(t, dir, patterns, Options{NoCache: true})
	if len(resp.SkippedFiles) != 0 {
		t.Fatalf("skipped %v", resp.SkippedFiles)
	}
	for fid, path := range resp.Files {
		alone := traceOnce(t, path, patterns, Options{NoCache: true})
		for pid := range resp.Patterns {
			got := linesCreditedTo(onlyFile(resp, fid), pid)
			if want := linesCreditedTo(alone, pid); !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: credited %+v, the file alone %+v", path, pid, got, want)
			}
		}
	}
}

// A file whose lines cannot be credited (here: a cut line changed in
// place before it is read again) is reported as skipped, with no match,
// and the other files of the request keep theirs.
func TestCreditPatterns_LosesOnlyTheFileThatFails(t *testing.T) {
	requireRipgrep(t)
	setLineLimits(t, 16, 100)
	text := []byte("LINE 1 NEEDLE and some more text\n")
	path := writeTextFile(t, "changed.log", text)
	src := pinForTest(t, path)
	if err := os.WriteFile(path, []byte("LINE 1 nothing and some more text\n"), 0o600); err != nil {
		t.Fatalf("edit: %v", err)
	}
	cut := MatchRaw{End: int64(len(text)), LineText: "LINE 1 NEEDLE an", LineTextTruncated: true}

	got, err := creditPatterns(context.Background(), creditRequest{
		files:        []creditFile{{source: src, lines: []MatchRaw{cut}}, {lines: []MatchRaw{{LineText: "NEED"}}}},
		patternIDs:   map[string]string{"p1": "NEEDLE", "p2": "NEED"},
		patternOrder: []string{"p1", "p2"},
	})
	if err != nil {
		t.Fatalf("err = %v, want the failure kept to its file", err)
	}
	if !errors.Is(got[0].err, errLineMatchesNoPattern) {
		t.Errorf("first file: err = %v, want errLineMatchesNoPattern", got[0].err)
	}
	if got[1].err != nil || len(got[1].lines) != 1 || len(got[1].lines[0]) != 1 || got[1].lines[0][0].patternID != "p2" {
		t.Errorf("second file: %+v, want its line credited to p2", got[1])
	}
}

// onlyFile is resp with only the matches of file fid.
func onlyFile(resp *rxtypes.TraceResponse, fid string) *rxtypes.TraceResponse {
	out := *resp
	out.Matches = nil
	for _, m := range resp.Matches {
		if m.File == fid {
			out.Matches = append(out.Matches, m)
		}
	}
	return &out
}

// fileCounter counts OnFile calls and the matches they report.
type fileCounter struct {
	files, matches int
}

func (c *fileCounter) OnFile(_ context.Context, _ string, info FileInfo) {
	c.files++
	c.matches += info.MatchesCount
}

func (c *fileCounter) OnMatch(context.Context, string, MatchInfo) {}

// OnFile stays a progress report of a search of many files: with one
// pattern it fires as each file's scan ends; with several, the files are
// settled in groups, so it trails the scan by at most settleEveryFiles
// files. Every file is reported, with its matches.
func TestTraceReportsFilesWhileTheSearchGoesOn(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	const fileCount = 2*settleEveryFiles + 3
	for i := 0; i < fileCount; i++ {
		name := filepath.Join(dir, "f"+strconv.Itoa(1000+i)+".log")
		if err := os.WriteFile(name, []byte("NEEDLE here\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, patterns := range [][]string{{"NEEDLE"}, {"NEEDLE", "NEED"}} {
		counter := &fileCounter{}
		var behind []int // files scanned but not yet reported, at each scan's end
		scanned := 0
		opts := Options{NoCache: true, HookFirer: counter, afterScan: func(string) {
			scanned++
			behind = append(behind, scanned-1-counter.files)
		}}
		traceOnce(t, dir, patterns, opts)

		if counter.files != fileCount || counter.matches != fileCount*len(patterns) {
			t.Errorf("%v: %d files with %d matches reported, want %d with %d", patterns, counter.files, counter.matches, fileCount, fileCount*len(patterns))
		}
		limit := 0
		if len(patterns) > 1 {
			limit = settleEveryFiles - 1
		}
		if most := slices.Max(behind); most != limit {
			t.Errorf("%v: reports trailed the scan by up to %d files, want %d", patterns, most, limit)
		}
	}
}
