package samples

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// timeIndexStep is the checkpoint step of the indexes these tests
// build: small, so a fixture of a few kilobytes has many checkpoints
// and an indexed search starts somewhere other than the first line.
const timeIndexStep = 256

// timeBase is the first timestamp of the generated logs:
// 2025-12-10 07:30:00 UTC.
var timeBase = time.Date(2025, 12, 10, 7, 30, 0, 0, time.UTC).UnixMilli()

// timedLine is one line of a generated log and the timestamp written on
// it, in ms, or -1 for a line without one.
type timedLine struct {
	text string
	ms   int64
}

// isoLines writes lines whose timestamps are ms values, in the
// app.log shape (`2025-12-10 07:30:00.000 INFO LINE n`); a value
// of -1 gives a continuation line without a timestamp. Every line says
// its number, so a wrong line is visible in a failure.
func isoLines(values []int64) []timedLine {
	lines := make([]timedLine, len(values))
	for i, ms := range values {
		if ms < 0 {
			lines[i] = timedLine{text: fmt.Sprintf("    at frame LINE %d", i+1), ms: -1}
			continue
		}
		stamp := time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05.000")
		lines[i] = timedLine{text: fmt.Sprintf("%s INFO LINE %d", stamp, i+1), ms: ms}
	}
	return lines
}

// orderedValues gives n lines one second apart from timeBase, with the
// 4th and 5th of every ten lines continuation lines without a
// timestamp, as a traceback leaves them.
func orderedValues(n int) []int64 {
	values := make([]int64, n)
	for i := range values {
		values[i] = timeBase + int64(i)*1000
		if i%10 == 3 || i%10 == 4 {
			values[i] = -1
		}
	}
	return values
}

// textOf joins lines with a line break after each.
func textOf(lines []timedLine) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// lineAtOracle is the rule written the plainest way: the first line in
// file order whose own timestamp is at least t, or -1.
func lineAtOracle(lines []timedLine, t int64) int64 {
	for i, l := range lines {
		if l.ms >= 0 && l.ms >= t {
			return int64(i + 1)
		}
	}
	return -1
}

// iso writes ms as a zone-less ISO query.
func iso(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000")
}

// timeCopies writes text as a plain file, a gzip copy and a seekable
// zstd copy whose frames end in the middle of lines, each in its own
// directory, and returns their paths by name.
func timeCopies(t *testing.T, text []byte) map[string]string {
	t.Helper()
	copies := map[string]string{
		"plain":    filepath.Join(t.TempDir(), "app.log"),
		"gzip":     filepath.Join(t.TempDir(), "app.log.gz"),
		"seekable": filepath.Join(t.TempDir(), "app.log.zst"),
	}
	writeFile(t, copies["plain"], text)
	writeFile(t, copies["gzip"], compressedcopy.Encode(t, compressedcopy.Gzip, text))
	seekablefile.Write(t, copies["seekable"], seekablefile.SplitEvery(text, 97))
	return copies
}

// writeFile writes data to path or fails the test.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// timeRequest is the samples request of a time-query test.
type timeRequest struct {
	values        []string
	before, after int
}

// askByTime resolves req against path with the stored index, as `rx
// samples` does, and returns the answer or the error.
func askByTime(path string, req timeRequest) (*rxtypes.SamplesResponse, error) {
	return Resolve(Request{
		Path: path, Timestamps: req.values,
		BeforeContext: req.before, AfterContext: req.after,
		IndexLoader: StoredIndex,
	})
}

// answerOf is what samplesanswer compares: the answer of a copy, or
// the text of its error, without what names the copy.
func answerOf(resp *rxtypes.SamplesResponse, err error) any {
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{
		"timestamps": resp.Timestamps, "samples": resp.Samples, "time_format": resp.TimeFormat,
		"lines": resp.Lines, "offsets": resp.Offsets,
	}
}

// answerByTime asks every copy of text, cold and after an index build
// (samplesanswer.ColdAndIndexed), requires all of them to answer as the
// plain file does, and returns the plain file's answer. The error of a
// refused request is compared the same way, with the file name taken
// out of it.
func answerByTime(t *testing.T, text []byte, req timeRequest) (*rxtypes.SamplesResponse, error) {
	t.Helper()
	var want any
	var plain *rxtypes.SamplesResponse
	var plainErr error
	for _, name := range []string{"plain", "gzip", "seekable"} {
		path := timeCopies(t, text)[name]
		got := samplesanswer.ColdAndIndexed(t, path, timeIndexStep, func(t testing.TB) any {
			resp, err := askByTime(path, req)
			if err != nil {
				err = errors.New(strings.ReplaceAll(err.Error(), path, "PATH"))
			}
			if name == "plain" {
				plain, plainErr = resp, err
			}
			return answerOf(resp, err)
		})
		if name == "plain" {
			want = got
			continue
		}
		samplesanswer.RequireAgree(t, name+" copy against the plain file", got, want)
	}
	return plain, plainErr
}

// requireLines checks the line each value resolved to, and that the
// first sample line of a found value is that line.
func requireLines(t *testing.T, resp *rxtypes.SamplesResponse, want map[string]int64) {
	t.Helper()
	for value, line := range want {
		got, ok := resp.Timestamps[value]
		if !ok || got != line {
			t.Errorf("%q: line %d (present %v), want %d", value, got, ok, line)
			continue
		}
		sample := resp.Samples[value]
		if line < 0 {
			if sample != nil {
				t.Errorf("%q: sample %q, want null", value, sample)
			}
			continue
		}
		if !strings.Contains(strings.Join(sample, "\n"), fmt.Sprintf("LINE %d\n", line)) &&
			!strings.HasSuffix(strings.Join(sample, "\n"), fmt.Sprintf("LINE %d", line)) {
			t.Errorf("%q: sample %q does not hold LINE %d", value, sample, line)
		}
	}
	if len(resp.Lines) != 0 || len(resp.Offsets) != 0 {
		t.Errorf("lines %v, offsets %v; want both empty in timestamps mode", resp.Lines, resp.Offsets)
	}
}

// An ordered file: a time on a line, between two lines, before the
// first, after the last, and just after a traceback's lines without
// timestamps. A single time returns its line with context, as a single
// --lines value does.
func TestTimestamps_OrderedFile(t *testing.T) {
	lines := isoLines(orderedValues(60))
	values := []string{
		iso(timeBase + 2000),        // on line 3
		iso(timeBase + 1500),        // between lines 2 and 3
		iso(timeBase - 3_600_000),   // an hour before the first line
		iso(timeBase + 59_001),      // after the last line
		iso(timeBase + 2500),        // after line 3, across the traceback of lines 4 and 5
		"2025-12-10 07:30:10.000",   // the file's own format, line 11
		"[2025-12-10 07:30:10.000]", // in brackets
		"1765351813000",             // epoch milliseconds: 07:30:13, line 14 is a continuation, so 16
		"2025-12-10T07:30:13Z",      // an instant: the zone-less file is read in RX_LOG_TZ, UTC
	}
	resp, err := answerByTime(t, textOf(lines), timeRequest{values: values, before: 2, after: 2})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireLines(t, resp, map[string]int64{
		values[0]: 3, values[1]: 3, values[2]: 1, values[3]: -1, values[4]: 6,
		values[5]: 11, values[6]: 11, values[7]: 16, values[8]: 16,
	})
	if got := resp.Samples[values[4]]; len(got) != 5 || !strings.HasSuffix(got[0], "LINE 4") {
		t.Errorf("context of line 6: %q, want lines 4 to 8", got)
	}
	if got := resp.Samples[values[2]]; len(got) != 3 {
		t.Errorf("context of line 1: %q, want lines 1 to 3", got)
	}
	want := rxtypes.SamplesTimeFormat{Format: "iso", HasZone: false, AssumedZone: "UTC"}
	if resp.TimeFormat == nil || *resp.TimeFormat != want {
		t.Errorf("time_format %+v, want %+v", resp.TimeFormat, want)
	}
}

// Two writers 2 s apart, interleaved: the line at T is the first line
// that reached T, at every random T, compared with the rule written as
// a plain loop.
func TestTimestamps_JitteredFileMatchesTheRule(t *testing.T) {
	values := make([]int64, 400)
	for i := range values {
		values[i] = timeBase + int64(i/2)*1000 + int64(i%2)*(-2000) + int64(i%7)*13
	}
	lines := isoLines(values)
	rng := rand.New(rand.NewPCG(7, 11))
	var queries []string
	want := map[string]int64{}
	for range 40 {
		ms := timeBase - 5000 + rng.Int64N(210_000)
		q := iso(ms)
		queries = append(queries, q)
		want[q] = lineAtOracle(lines, ms)
	}
	resp, err := answerByTime(t, textOf(lines), timeRequest{values: queries})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireLines(t, resp, want)
}

// Ranges: closed, open on either side, an end before the start, lines
// without timestamps inside, and an inclusive end at millisecond
// precision. A range returns its lines without context.
func TestTimestamps_Ranges(t *testing.T) {
	lines := isoLines(orderedValues(30))
	closed := iso(timeBase+2000) + ".." + iso(timeBase+6000)
	openStart := ".." + iso(timeBase+1000)
	openEnd := iso(timeBase+27_000) + ".."
	backwards := iso(timeBase+6000) + ".." + iso(timeBase+2000)
	beforeFirst := ".." + iso(timeBase-1)
	resp, err := answerByTime(t, textOf(lines), timeRequest{
		values: []string{closed, openStart, openEnd, backwards, beforeFirst}, before: 3, after: 3,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cases := []struct {
		value       string
		first, last int
	}{
		{closed, 3, 7},
		{openStart, 1, 2},
		{openEnd, 28, 30},
		{backwards, -1, -1},
		{beforeFirst, -1, -1},
	}
	for _, tc := range cases {
		got := resp.Samples[tc.value]
		if tc.first < 0 {
			if resp.Timestamps[tc.value] != -1 || got != nil {
				t.Errorf("%q: line %d, sample %q; want -1 and null", tc.value, resp.Timestamps[tc.value], got)
			}
			continue
		}
		if resp.Timestamps[tc.value] != int64(tc.first) || len(got) != tc.last-tc.first+1 ||
			!strings.HasSuffix(got[0], fmt.Sprintf("LINE %d", tc.first)) ||
			!strings.HasSuffix(got[len(got)-1], fmt.Sprintf("LINE %d", tc.last)) {
			t.Errorf("%q: line %d, sample %q; want lines %d to %d", tc.value, resp.Timestamps[tc.value], got, tc.first, tc.last)
		}
	}
}

// `..14:35:15` ends at 14:35:15.000: a line at 14:35:15.001 is after it.
func TestTimestamps_RangeEndIsInclusiveToTheMillisecond(t *testing.T) {
	at := time.Date(2025, 12, 10, 14, 35, 15, 0, time.UTC).UnixMilli()
	lines := isoLines([]int64{at - 1, at, at + 1, at + 2})
	value := "..2025-12-10 14:35:15"
	resp, err := answerByTime(t, textOf(lines), timeRequest{values: []string{value}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := resp.Samples[value]; len(got) != 2 || !strings.HasSuffix(got[1], "LINE 2") {
		t.Errorf("sample %q, want lines 1 and 2", got)
	}
}

// A time with no date takes it from a one-day file and is refused, with
// both dates, on a file that spans two.
func TestTimestamps_TimeOfDay(t *testing.T) {
	oneDay := isoLines(orderedValues(30))
	resp, err := answerByTime(t, textOf(oneDay), timeRequest{values: []string{"07:30:05", "07:30:05..07:30:07"}})
	if err != nil {
		t.Fatalf("one day: %v", err)
	}
	requireLines(t, resp, map[string]int64{"07:30:05": 6, "07:30:05..07:30:07": 6})

	midnight := time.Date(2025, 12, 10, 23, 59, 58, 0, time.UTC).UnixMilli()
	twoDays := isoLines([]int64{midnight, midnight + 1000, midnight + 2000, midnight + 3000})
	_, err = answerByTime(t, textOf(twoDays), timeRequest{values: []string{"00:00:01"}})
	if err == nil || !strings.Contains(err.Error(), "2025-12-10") || !strings.Contains(err.Error(), "2025-12-11") {
		t.Fatalf("two days: %v; want a refusal naming 2025-12-10 and 2025-12-11", err)
	}
}

// A refused time-of-day query is a usage error.
func TestTimestamps_TimeOfDayOnTwoDaysIsAUsageError(t *testing.T) {
	midnight := time.Date(2025, 12, 10, 23, 59, 58, 0, time.UTC).UnixMilli()
	path := filepath.Join(t.TempDir(), "app.log")
	writeFile(t, path, textOf(isoLines([]int64{midnight, midnight + 1000, midnight + 2000, midnight + 3000})))
	_, err := askByTime(path, timeRequest{values: []string{"00:00:01"}})
	if !IsUsageError(err) || !errors.Is(err, timestamps.ErrInvalidQuery) {
		t.Fatalf("err = %v; want a usage error wrapping ErrInvalidQuery", err)
	}
}

// zonedLines writes lines one second apart from wall-clock wall, each
// with the zone written after it.
func zonedLines(n int, wall int64, zone string) []timedLine {
	lines := make([]timedLine, n)
	for i := range lines {
		stamp := time.UnixMilli(wall + int64(i)*1000).UTC().Format("2006-01-02T15:04:05.000")
		lines[i] = timedLine{text: fmt.Sprintf("%s%s INFO LINE %d", stamp, zone, i+1)}
	}
	return lines
}

// Zones: a zoned file read with zoned and zone-less queries; a
// zone-less file written in RX_LOG_TZ, winter and summer alike; a query
// read in the process's zone under RX_QUERY_TZ=local.
func TestTimestamps_Zones(t *testing.T) {
	t.Run("zoned file", func(t *testing.T) {
		lines := zonedLines(20, timeBase, "+02:00")
		resp, err := answerByTime(t, textOf(lines), timeRequest{values: []string{
			"2025-12-10T05:30:03Z", "2025-12-10T07:30:03", "2025-12-10T07:30:03+02:00",
		}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		requireLines(t, resp, map[string]int64{
			"2025-12-10T05:30:03Z": 4, "2025-12-10T07:30:03": 4, "2025-12-10T07:30:03+02:00": 4,
		})
		want := rxtypes.SamplesTimeFormat{Format: "iso", HasZone: true, AssumedZone: "UTC"}
		if resp.TimeFormat == nil || *resp.TimeFormat != want {
			t.Errorf("time_format %+v, want %+v", resp.TimeFormat, want)
		}
	})
	for _, season := range []struct {
		name string
		wall int64
		z    string
	}{
		{"zone-less file in winter", timeBase, "2025-12-10T06:30:03Z"},
		{"zone-less file in summer", time.Date(2025, 7, 1, 7, 30, 0, 0, time.UTC).UnixMilli(), "2025-07-01T05:30:03Z"},
	} {
		t.Run(season.name, func(t *testing.T) {
			t.Setenv("RX_LOG_TZ", "Europe/Berlin")
			lines := zonedLines(20, season.wall, "")
			resp, err := answerByTime(t, textOf(lines), timeRequest{values: []string{season.z}})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			requireLines(t, resp, map[string]int64{season.z: 4})
			if resp.TimeFormat == nil || resp.TimeFormat.AssumedZone != "Europe/Berlin" {
				t.Errorf("time_format %+v, want assumed_zone Europe/Berlin", resp.TimeFormat)
			}
		})
	}
	t.Run("query in the process's zone", func(t *testing.T) {
		previous := time.Local
		time.Local = time.FixedZone("TEST+3", 3*3600)
		t.Cleanup(func() { time.Local = previous })
		t.Setenv("RX_QUERY_TZ", "local")
		lines := zonedLines(20, timeBase, "")
		resp, err := answerByTime(t, textOf(lines), timeRequest{values: []string{"2025-12-10T10:30:03"}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		requireLines(t, resp, map[string]int64{"2025-12-10T10:30:03": 4})
	})
}

// logShape is one shape of the playground's logs: the text of line n
// (n from 1) and the timestamp text a person copies from it.
type logShape struct {
	name  string
	line  func(n int) string
	stamp func(n int) string
}

// logShapes are the line shapes of the playground's real logs.
var logShapes = []logShape{
	{"middleware", func(n int) string { return fmt.Sprintf("2025-12-10 07:49:%02d.%03d INFO LINE %d", n, n*7, n) },
		func(n int) string { return fmt.Sprintf("2025-12-10 07:49:%02d.%03d", n, n*7) }},
	{"APPLOG", func(n int) string { return fmt.Sprintf("2025-2-15 12:34:%d:%d [main] LINE %d", n, n*7, n) },
		func(n int) string { return fmt.Sprintf("2025-2-15 12:34:%d:%d", n, n*7) }},
	{"core syslog", func(n int) string { return fmt.Sprintf("Dec 10 07:49:%02d.%03d host app: LINE %d", n, n*7, n) },
		func(n int) string { return fmt.Sprintf("Dec 10 07:49:%02d.%03d", n, n*7) }},
	{"postgresql", func(n int) string {
		return fmt.Sprintf("2025-12-10 07:49:%02d UTC [42]: LINE %d\n\tplan of LINE %d", n, 2*n-1, 2*n-1)
	}, func(n int) string { return fmt.Sprintf("2025-12-10 07:49:%02d UTC", n) }},
	{"python logging", func(n int) string { return fmt.Sprintf("2025-12-10 07:49:%02d,%03d - app - LINE %d", n, n*7, n) },
		func(n int) string { return fmt.Sprintf("2025-12-10 07:49:%02d,%03d", n, n*7) }},
}

// A timestamp copied from a line of each playground shape finds that
// line. The postgresql shape has a continuation line after each
// record, so its record n is line 2n-1.
func TestTimestamps_CopiedTimestampFindsItsLine(t *testing.T) {
	for _, shape := range logShapes {
		t.Run(shape.name, func(t *testing.T) {
			var b strings.Builder
			for n := 1; n <= 40; n++ {
				b.WriteString(shape.line(n))
				b.WriteByte('\n')
			}
			copied := shape.stamp(17)
			resp, err := answerByTime(t, []byte(b.String()), timeRequest{values: []string{copied}})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			want := int64(17)
			if shape.name == "postgresql" {
				want = 33
			}
			requireLines(t, resp, map[string]int64{copied: want})
		})
	}
}

// A file with no timestamp format refuses a time query as a usage
// error, and answers time_format null in every mode.
func TestTimestamps_FileWithoutTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.txt")
	writeFile(t, path, []byte(strings.Repeat("no time here\n", 50)))
	_, err := askByTime(path, timeRequest{values: []string{"2025-12-10T07:30:00"}})
	if !errors.Is(err, ErrNoTimeFormat) || !IsUsageError(err) {
		t.Fatalf("err = %v; want ErrNoTimeFormat as a usage error", err)
	}
	resp, err := Resolve(Request{Path: path, Lines: []OffsetOrRange{{Start: 3}}, IndexLoader: StoredIndex})
	if err != nil {
		t.Fatalf("lines: %v", err)
	}
	if resp.TimeFormat != nil || resp.Timestamps == nil || len(resp.Timestamps) != 0 {
		t.Errorf("time_format %+v, timestamps %v; want null and {}", resp.TimeFormat, resp.Timestamps)
	}
}

// An invalid value is a usage error that names the value.
func TestTimestamps_InvalidValueIsAUsageError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	writeFile(t, path, textOf(isoLines(orderedValues(10))))
	_, err := askByTime(path, timeRequest{values: []string{"yesterday at noon"}})
	if !IsUsageError(err) || !strings.Contains(err.Error(), "yesterday at noon") {
		t.Fatalf("err = %v; want a usage error naming the value", err)
	}
	many := make([]string, MaxTimestampValues+1)
	for i := range many {
		many[i] = iso(timeBase)
	}
	if _, err := askByTime(path, timeRequest{values: many}); !IsUsageError(err) {
		t.Fatalf("%d values: err = %v; want a usage error", len(many), err)
	}
}

// time_format is in the answer of every mode, the same with and
// without an index.
func TestTimestamps_TimeFormatInLinesAndOffsetsModes(t *testing.T) {
	text := textOf(isoLines(orderedValues(30)))
	for _, name := range []string{"plain", "gzip", "seekable"} {
		path := timeCopies(t, text)[name]
		samplesanswer.ColdAndIndexed(t, path, timeIndexStep, func(t testing.TB) any {
			byLine, err := Resolve(Request{Path: path, Lines: []OffsetOrRange{{Start: 5}}, IndexLoader: StoredIndex})
			if err != nil {
				t.Fatalf("lines: %v", err)
			}
			byOffset, err := Resolve(Request{Path: path, Offsets: []OffsetOrRange{{Start: 100}}, IndexLoader: StoredIndex})
			if err != nil {
				t.Fatalf("offsets: %v", err)
			}
			for _, resp := range []*rxtypes.SamplesResponse{byLine, byOffset} {
				if resp.TimeFormat == nil || resp.TimeFormat.Format != "iso" || len(resp.Timestamps) != 0 {
					t.Fatalf("%s: time_format %+v, timestamps %v", name, resp.TimeFormat, resp.Timestamps)
				}
			}
			return []any{byLine, byOffset}
		})
	}
}
