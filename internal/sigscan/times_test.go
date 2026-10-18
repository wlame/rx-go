package sigscan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
)

// msOf returns an RFC 3339 time as milliseconds since the Unix epoch.
func msOf(t testing.TB, text string) int64 {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatal(err)
	}
	return v.UnixMilli()
}

// timesOf returns the time of each finding of a signature, -1 for none.
func timesOf(res *Result, id string) []int64 {
	var out []int64
	for _, f := range res.BySignature[id].Findings {
		if f.TimeMS == nil {
			out = append(out, -1)
			continue
		}
		out = append(out, *f.TimeMS)
	}
	return out
}

// requireTimes fails when a signature's findings do not have the times
// given (-1 for none).
func requireTimes(t *testing.T, res *Result, id string, want ...int64) {
	t.Helper()
	got := timesOf(res, id)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: times %v, want %v", id, got, want)
	}
}

// stampedText is an ISO-stamped log with a traceback two lines after a
// stamped line, which carries that line's time.
var stampedText = lines(
	"2026-01-02T10:00:00.000Z INFO start",
	"2026-01-02T10:00:01.500Z ERROR request failed",
	"  detail one",
	"Traceback (most recent call last):",
	"  File \"app.py\", line 3",
	"ValueError: bad input",
	"2026-01-02T10:05:00.000Z ERROR again",
)

// isoLines is n ISO-stamped lines, one second apart.
func isoLines(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "2026-01-02T10:00:%02d.000Z INFO line %d\n", i, i+1)
	}
	return b.String()
}

// stampedSigs finds ERROR lines and Python tracebacks.
var stampedSigs = sigFile(
	regexSig("error", `ERROR`),
	kindSig("tb", "block", `start: '^Traceback \(most recent call last\):'`, `continue: '^\s'`, `end: '^[A-Za-z_][\w.]*(?::|$)'`),
)

// A finding takes the time of its first line when the line has one,
// and else the time of the nearest earlier stamped line: a traceback
// two lines after a stamped line carries that line's time.
func TestScan_TimeOfAFindingsFirstLine(t *testing.T) {
	sigs := loadSigs(t, stampedSigs, "error", "tb")
	res := scanOne(t, stampedText, sigs, Options{LookbackBytes: 64 << 10})
	requireTimes(t, res, "error", msOf(t, "2026-01-02T10:00:01.5Z"), msOf(t, "2026-01-02T10:05:00Z"))
	requireTimes(t, res, "tb", msOf(t, "2026-01-02T10:00:01.5Z"))
}

// A stamped line farther back than LookbackBytes gives no time: the
// distance is from the stamped line's start to the finding's first
// line's start. A lookback of 0 keeps only a line's own time.
func TestScan_TimeFartherThanTheLookbackIsNull(t *testing.T) {
	sigs := loadSigs(t, stampedSigs, "error", "tb")
	distance := strings.Index(stampedText, "Traceback") - strings.Index(stampedText, "2026-01-02T10:00:01")
	cases := []struct {
		lookback int
		tb       int64
	}{
		{lookback: distance, tb: msOf(t, "2026-01-02T10:00:01.5Z")},
		{lookback: distance - 1, tb: -1},
		{lookback: 0, tb: -1},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("lookback-%d", tc.lookback), func(t *testing.T) {
			res := scanOne(t, stampedText, sigs, Options{LookbackBytes: tc.lookback})
			requireTimes(t, res, "tb", tc.tb)
			requireTimes(t, res, "error", msOf(t, "2026-01-02T10:00:01.5Z"), msOf(t, "2026-01-02T10:05:00Z"))
		})
	}
}

// A file without timestamps gives no time to any finding.
func TestScan_TimeWithoutAFormatIsNullEverywhere(t *testing.T) {
	sigs := loadSigs(t, stampedSigs, "error", "tb")
	text := strings.ReplaceAll(stampedText, "2026-01-02T", "day ")
	res := scanOne(t, text, sigs, Options{LookbackBytes: 64 << 10})
	requireTimes(t, res, "error", -1, -1)
	requireTimes(t, res, "tb", -1)
}

// A finding's time is the value the index records for the same line,
// in the file's own frame: the first and the last stamped line of an
// ISO file, of a syslog file whose year comes from the file's mtime,
// and of a file whose first line starts with a byte-order mark (line 1
// is stamped behind the mark, for the index and the scan alike). And a
// carried time equals the line_timestamps value samples gives the same
// line.
func TestScan_TimeEqualsTheIndex(t *testing.T) {
	every := loadSigs(t, sigFile(regexSig("line", `.`)), "line")
	texts := map[string]string{
		"iso": stampedText,
		"syslog": lines(
			"Jan  2 10:00:00 host app[1]: start",
			"  continued",
			"Jan  2 10:00:07 host app[1]: stop",
		),
		"bom": "\xEF\xBB\xBF" + isoLines(10),
	}
	for name, text := range texts {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.log")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			idx, err := index.Build(path, index.BuildOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ti := idx.TimeIndex
			if ti == nil || ti.First == nil || ti.Last == nil {
				t.Fatalf("the index found no timestamps: %+v", ti)
			}
			if name == "bom" && ti.First.Line != 1 {
				t.Fatalf("the index does not read the marked line 1 as stamped (first stamped line %d); the case tests nothing", ti.First.Line)
			}
			res, err := Scan(context.Background(), classify(t, path), every, Options{Workers: 1, LookbackBytes: 64 << 10})
			if err != nil {
				t.Fatal(err)
			}
			got := timesOf(res, "line")
			for line, want := range map[int64]int64{ti.First.Line: ti.First.Ms, ti.Last.Line: ti.Last.Ms} {
				if got[line-1] != want {
					t.Errorf("line %d: time %d, the index records %d", line, got[line-1], want)
				}
			}
			for line := int64(1); line < ti.First.Line; line++ {
				if got[line-1] != -1 {
					t.Errorf("line %d before the first stamped line has time %d", line, got[line-1])
				}
			}
		})
	}
	t.Run("carried", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		if err := os.WriteFile(path, []byte(stampedText), 0o600); err != nil {
			t.Fatal(err)
		}
		pin, err := paths.Pin(path)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := samples.Resolve(context.Background(), samples.Request{Path: path, Source: pin, Lines: []samples.OffsetOrRange{{Start: 4}}})
		if err != nil {
			t.Fatal(err)
		}
		want := answer.LineTimestamps["4"][0]
		res := scanOne(t, stampedText, every, Options{LookbackBytes: 64 << 10})
		if got := res.BySignature["line"].Findings[3].TimeMS; want == nil || got == nil || *got != *want {
			t.Errorf("line 4: time %v, samples gives %v", got, want)
		}
	})
}

// isoStampAtStart is an ISO timestamp in UTC at the start of a line.
var isoStampAtStart = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z`)

// oracleTime is the time a finding whose first line starts at byte
// start of text must have: the timestamp of the nearest line starting
// at or before start that begins with an ISO timestamp in UTC, when that
// line starts at most lookback bytes before start; -1 otherwise. It
// knows that one format only and shares no code with the scan.
func oracleTime(t testing.TB, text string, start, lookback int64) int64 {
	t.Helper()
	best, bestStart := int64(-1), int64(-1)
	pos := int64(0)
	for _, line := range strings.SplitAfter(text, "\n") {
		if pos > start {
			break
		}
		if m := isoStampAtStart.FindString(line); m != "" {
			best, bestStart = msOf(t, m), pos
		}
		pos += int64(len(line))
	}
	if best < 0 || start-bestStart > lookback {
		return -1
	}
	return best
}

// derefTime shows a finding's time, -1 for none.
func derefTime(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// timeFixture: findings at stamped lines, a few lines after one (within
// the lookback and beyond it), a block and runs that cross chunk edges,
// a CRLF stamped line and a last line without a newline. Its times are
// checked against oracleTime on every chunking and storage.
var timeFixture = fixture{
	name: "time of every finding",
	def: regexSig("error", `ERROR`) +
		kindSig("tb", "block", `start: '^Traceback \(most recent call last\):'`, `continue: '^\s'`, `end: '^[A-Za-z_][\w.]*(?::|$)'`) +
		kindSig("blanks", "run", "blank: true", "min: 2") +
		kindSig("frames", "run", `match: '^\s+at '`, "min: 2"),
	ids: []string{"error", "tb", "blanks", "frames"},
	text: lines(
		"2026-01-02T10:00:00.000Z INFO start",
		"2026-01-02T10:00:01.250Z ERROR one",
		"  at frame 1",
		"  at frame 2",
		"Traceback (most recent call last):",
		"  File \"a.py\", line 1",
		"ValueError: v",
		"",
		"",
		"ERROR unstamped error line",
		"filler "+strings.Repeat("x", 70),
		"ERROR far from any stamp",
		"2026-01-02T10:00:09.000Z ERROR three\r",
		"Traceback (most recent call last):",
		"  File \"b.py\", line 2",
		"KeyError: k",
	) + "2026-01-02T10:00:10.000Z ERROR end",
	opts: Options{LookbackBytes: 80},
	want: map[string][]lineSpan{
		"error":  {{2, 2}, {10, 10}, {12, 12}, {13, 13}, {17, 17}},
		"tb":     {{5, 7}, {14, 16}},
		"blanks": {{8, 9}},
		"frames": {{3, 4}},
	},
	timed: true,
}

// Every finding of the time fixture has the time of its first line or
// of the nearest stamped line within the lookback, none past it.
func TestScan_TimeOfEveryKindOfFinding(t *testing.T) {
	timeFixture.run(t)
}

// replayTimeFixture: every line may start a two-line block, so at a
// chunk edge inside a block the merge's replay and the chunk's own walk
// stay out of step, and the replay itself starts the next block, on a
// stamped line. That block keeps its own line's time, not the time
// carried into the chunk.
var replayTimeFixture = fixture{
	name: "blocks the replay starts keep their own time",
	def:  kindSig("pair", "block", `start: 'S [a-z]$'`, "lines: 1"),
	ids:  []string{"pair"},
	text: lines(
		"2026-01-02T10:00:00.000Z S a",
		"S b",
		"2026-01-02T10:00:02.000Z S c",
		"S d",
		"2026-01-02T10:00:04.000Z S e",
		"S f",
	),
	opts:  Options{LookbackBytes: 1 << 10},
	want:  map[string][]lineSpan{"pair": {{1, 2}, {3, 4}, {5, 6}}},
	timed: true,
}
