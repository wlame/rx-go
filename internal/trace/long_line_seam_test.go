package trace

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A line longer than the chunker's newline lookahead that lies across a
// tentative chunk boundary must stay one line: the chunk boundary moves
// to the end of that line, so each match carries the line's first byte
// and its whole text, and a chunked scan answers exactly as a scan of
// the file in one piece, and as single-process ripgrep.

// seamLookahead is the length of one read of the chunker's newline
// search. A fixture line is long enough to matter when a tentative
// boundary has no newline within this many bytes after it.
const seamLookahead = 256 << 10

// seamLongLine is a long line of a seam fixture: the tentative boundary
// (1-based) it starts just before, and its length. A line that starts
// seamLeadIn bytes before a boundary and is longer than
// seamLeadIn+seamLookahead has no newline within the lookahead of that
// boundary.
type seamLongLine struct {
	seam   int
	length int
}

// seamLeadIn is how far before its boundary a long line starts.
const seamLeadIn = 16 << 10

// seamLayouts are the files the seam tests search, each about 4.2 MiB,
// which chunkedTraceEnv cuts into four tentative chunks of about
// 1.05 MiB.
var seamLayouts = []struct {
	name  string
	longs []seamLongLine
	// seams are the tentative boundaries (1-based) that lie inside a
	// long line, with no newline within the lookahead after them.
	seams []int
	// cacheable is set when every line is shorter than the line text an
	// answer keeps whole, so the scan writes a trace cache.
	cacheable bool
}{
	{
		name:      "one long line on each boundary",
		longs:     []seamLongLine{{1, 320 << 10}, {2, 320 << 10}, {3, 320 << 10}},
		seams:     []int{1, 2, 3},
		cacheable: true,
	},
	{
		// 1.4 MiB from just before the first boundary to past the
		// lookahead of the second.
		name:  "a line longer than a whole chunk",
		longs: []seamLongLine{{1, 1<<20 + 400<<10}},
		seams: []int{1, 2},
	},
}

// seamSize is the size of a seam fixture, and seamChunks the number of
// chunks chunkedTraceEnv plans for a file of that size.
const (
	seamSize   = 4<<20 + 200<<10
	seamChunks = 4
)

// seamText writes the text of a seam fixture: short numbered lines, with
// each long line "LINE <n> HEAD xxx… TAIL" starting seamLeadIn bytes
// before its tentative boundary.
func seamText(longs []seamLongLine) []byte {
	var b bytes.Buffer
	line := 1
	short := func() {
		fmt.Fprintf(&b, "LINE %d short\n", line)
		line++
	}
	for _, long := range longs {
		for b.Len() < long.seam*(seamSize/seamChunks)-seamLeadIn {
			short()
		}
		fmt.Fprintf(&b, "LINE %d HEAD %s TAIL\n", line, strings.Repeat("x", long.length))
		line++
	}
	for b.Len() < seamSize {
		short()
	}
	return b.Bytes()
}

// requireSeamsInsideLongLines fails the test unless each of the given
// tentative chunk boundaries has no newline within the lookahead after
// it: the layout a chunker that cuts at the end of its lookahead gets
// wrong.
func requireSeamsInsideLongLines(t *testing.T, text []byte, seams []int) {
	t.Helper()
	chunkSize := len(text) / seamChunks
	for _, k := range seams {
		raw := k * chunkSize
		if bytes.IndexByte(text[raw:raw+seamLookahead], '\n') >= 0 {
			t.Fatalf("tentative boundary %d at byte %d has a newline within %d bytes; the fixture tests nothing", k, raw, seamLookahead)
		}
	}
}

// seamWindow is a context window a seam search asks for.
type seamWindow struct {
	name          string
	before, after int
}

// noWindow asks for no context; contextWindows add one line on one side.
// A window reads lines before and after its chunk, so each side is
// tested alone.
var (
	noWindow       = []seamWindow{{name: "no context"}}
	contextWindows = []seamWindow{{name: "no context"}, {name: "-B 1", before: 1}, {name: "-A 1", after: 1}}
)

// seamSearches are the searches each seam layout runs. The anchored and
// word searches match nothing in the file but would match at a cut in
// the middle of a long line.
var seamSearches = []struct {
	name     string
	patterns []string
	extra    []string
	windows  []seamWindow
}{
	{name: "start and end of the line", patterns: []string{"HEAD", "TAIL"}, windows: contextWindows},
	{name: "whole line", patterns: []string{"HEAD x+ TAIL"}, windows: noWindow},
	{name: "line start anchor", patterns: []string{"^x"}, windows: noWindow},
	{name: "line end anchor", patterns: []string{"x$"}, windows: noWindow},
	{name: "word", patterns: []string{"x+"}, extra: []string{"-w"}, windows: noWindow},
}

// rgMatchedLines runs single-process ripgrep over the whole file and
// returns the line number of each matched line by the offset of its
// first byte. --max-columns=1 replaces every line's text with a short
// note, so a long line costs nothing to print.
func rgMatchedLines(t *testing.T, path string, patterns, extra []string) map[int64]int {
	t.Helper()
	args := append([]string{"--no-config", "--text", "--encoding=none", "--line-number", "--byte-offset", "--max-columns=1"}, extra...)
	for _, p := range patterns {
		args = append(args, "-e", p)
	}
	out, err := exec.Command("rg", append(args, path)...).Output() //nolint:gosec // test fixture path and fixed patterns
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return map[int64]int{} // rg exits 1 when nothing matches
	}
	if err != nil {
		t.Fatalf("rg %v: %v", args, err)
	}
	lines := map[int64]int{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), ":", 3)
		line, lerr := strconv.Atoi(fields[0])
		offset, oerr := strconv.ParseInt(fields[1], 10, 64)
		if len(fields) < 3 || lerr != nil || oerr != nil {
			t.Fatalf("unexpected rg output line %q", scanner.Text())
		}
		lines[offset] = line
	}
	return lines
}

// requireRipgrepLines fails the test unless resp matched exactly the
// lines single-process ripgrep matches, at their first bytes and with
// their numbers.
func requireRipgrepLines(t *testing.T, resp *rxtypes.TraceResponse, want map[int64]int) {
	t.Helper()
	got := map[int64]int{}
	for _, m := range resp.Matches {
		got[m.Offset] = m.AbsoluteLineNumber
	}
	if len(got) != len(want) {
		t.Errorf("matched %d lines, ripgrep matches %d", len(got), len(want))
	}
	offsets := make([]int64, 0, len(got))
	for offset := range got {
		offsets = append(offsets, offset)
	}
	slices.Sort(offsets)
	for _, offset := range offsets {
		line, ok := want[offset]
		if !ok {
			t.Errorf("match at offset %d (line %d) is not a line ripgrep matches", offset, got[offset])
			continue
		}
		if got[offset] != line {
			t.Errorf("match at offset %d: line %d, ripgrep says %d", offset, got[offset], line)
		}
	}
}

// Scenarios: long lines across chunk boundaries, one per boundary and
// one longer than a whole chunk; matches at the start, the end and over
// the whole line, anchors and -w that would match at a cut, and a
// window of one line before or after the start and end matches. The chunk boundaries are line starts;
// the chunked scan, its cache hit and an unchunked scan give the same
// answer, which holds the lines ripgrep matches.
func TestLongLineAcrossAChunkBoundaryStaysOneLine(t *testing.T) {
	for _, layout := range seamLayouts {
		t.Run(layout.name, func(t *testing.T) {
			chunkedTraceEnv(t)
			t.Setenv("RX_LARGE_FILE_MB", "1")
			text := seamText(layout.longs)
			requireSeamsInsideLongLines(t, text, layout.seams)
			path := writeTextFile(t, "seam.log", text)

			tasks, err := CreateFileTasks(path)
			if err != nil {
				t.Fatalf("CreateFileTasks: %v", err)
			}
			if len(tasks) < 2 {
				t.Fatalf("fixture scans in %d chunk(s); the test needs several", len(tasks))
			}
			for _, task := range tasks[1:] {
				if text[task.Offset-1] != '\n' {
					t.Fatalf("chunk %d starts at byte %d, inside a line", task.TaskID, task.Offset)
				}
			}
			table := newTextLineTable(text)

			for _, search := range seamSearches {
				for _, window := range search.windows {
					t.Run(search.name+", "+window.name, func(t *testing.T) {
						t.Setenv("RX_CACHE_DIR", t.TempDir())
						opts := Options{RgExtraArgs: search.extra, ContextBefore: window.before, ContextAfter: window.after}
						scanned := false
						opts.afterScan = func(string) { scanned = true }
						cold := traceOnce(t, path, search.patterns, opts)
						if !scanned {
							t.Fatal("the first trace did not scan the file")
						}
						requireRipgrepLines(t, cold, rgMatchedLines(t, path, search.patterns, search.extra))
						if layout.cacheable && len(cold.Matches) > 0 {
							checkWindowsByOffset(t, cold, table, window.before, window.after)
						}

						if layout.cacheable {
							scanned = false
							warm := traceOnce(t, path, search.patterns, opts)
							if scanned {
								t.Fatal("the second trace scanned the file instead of reading the cache")
							}
							traceanswer.RequireSame(t, "cache hit", warm, cold)
						}

						t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1000")
						whole := opts
						whole.NoCache = true
						unchunked := traceOnce(t, path, search.patterns, whole)
						traceanswer.RequireSame(t, "one chunk", cold, unchunked)
						t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
					})
				}
			}
		})
	}
}
