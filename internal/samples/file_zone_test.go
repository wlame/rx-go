package samples

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// zoneNamed parses a file zone or fails the test.
func zoneNamed(t testing.TB, name string) config.Zone {
	t.Helper()
	zone, err := config.ParseZone(name)
	if err != nil {
		t.Fatalf("ParseZone(%q): %v", name, err)
	}
	return zone
}

// msOf is the instant of an RFC 3339 text, for expected values.
func msOf(t testing.TB, text string) int64 {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return at.UnixMilli()
}

// requireTimeFormat checks the time_format of an answer.
func requireTimeFormat(t *testing.T, resp *rxtypes.SamplesResponse, want rxtypes.SamplesTimeFormat) {
	t.Helper()
	if resp.TimeFormat == nil || *resp.TimeFormat != want {
		t.Errorf("time_format %+v, want %+v", resp.TimeFormat, want)
	}
}

// requireFirstLineStamp checks the effective timestamp of the first line
// of a value's sample.
func requireFirstLineStamp(t *testing.T, resp *rxtypes.SamplesResponse, value string, want int64) {
	t.Helper()
	got := resp.LineTimestamps[value]
	if len(got) == 0 || got[0] == nil || *got[0] != want {
		t.Errorf("line_timestamps[%q] = %v, want first %d", value, got, want)
	}
}

// A file without zones read in a file zone: its wall clock is read in
// that zone, not in RX_LOG_TZ, so an instant lands nine hours of wall
// clock away from where it lands in UTC. A query without a zone is read
// in the file zone too. Every copy answers the same, cold and indexed.
func TestFileZone_ZonelessFileReadsItsWallClockInTheZone(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "Europe/Berlin") // the file zone takes its place
	lines := zonedLines(20, timeBase, "")  // 2025-12-10 07:30:00 … 07:30:19
	values := []string{"2025-12-09T22:30:03Z", "2025-12-10T07:30:03", "2025-12-10T07:30:03Z"}
	resp, err := answerByTime(t, textOf(lines), timeRequest{values: values, zone: zoneNamed(t, "Asia/Tokyo")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// 07:30:03 in Tokyo is 22:30:03 UTC the day before; 07:30:03 UTC is
	// 16:30:03 in Tokyo, after the last line.
	requireLines(t, resp, map[string]int64{values[0]: 4, values[1]: 4, values[2]: -1})
	requireFirstLineStamp(t, resp, values[0], msOf(t, "2025-12-09T22:30:03Z"))
	requireTimeFormat(t, resp, rxtypes.SamplesTimeFormat{Format: "iso", HasZone: false, AssumedZone: "Asia/Tokyo"})
}

// RX_QUERY_TZ still reads a query without a zone under a file zone.
func TestFileZone_QueryZoneStillReadsAZonelessQuery(t *testing.T) {
	t.Setenv("RX_QUERY_TZ", "UTC")
	lines := zonedLines(20, timeBase, "")
	values := []string{"2025-12-09T22:30:03", "2025-12-10T07:30:03"}
	resp, err := answerByTime(t, textOf(lines), timeRequest{values: values, zone: zoneNamed(t, "Asia/Tokyo")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireLines(t, resp, map[string]int64{values[0]: 4, values[1]: -1})
}

// A file whose lines write +02:00 read in UTC: the written wall clock is
// read as UTC and the +02:00 ignored. time_format keeps the file's
// has_zone. Every copy answers the same, cold and indexed.
func TestFileZone_ZonedFileIgnoresTheZoneItsLinesWrite(t *testing.T) {
	lines := zonedLines(20, timeBase, "+02:00") // 2025-12-10T07:30:00+02:00 …
	values := []string{"2025-12-10T07:30:03Z", "2025-12-10T07:30:03", "2025-12-10T05:30:03Z"}
	resp, err := answerByTime(t, textOf(lines), timeRequest{values: values, zone: zoneNamed(t, "UTC")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireLines(t, resp, map[string]int64{values[0]: 4, values[1]: 4, values[2]: 1})
	requireFirstLineStamp(t, resp, values[0], timeBase+3000)
	requireTimeFormat(t, resp, rxtypes.SamplesTimeFormat{Format: "iso", HasZone: true, AssumedZone: "UTC"})

	// Without the file zone the same instant is two hours later in the
	// file's own frame, past its last line.
	plain, err := answerByTime(t, textOf(lines), timeRequest{values: values[:1]})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireLines(t, plain, map[string]int64{values[0]: -1})
}

// berlinDSTLines writes a line every seven minutes across the end of
// summer time in Berlin (2025-10-26 01:00 UTC), each with the Berlin
// wall clock and the offset in force, and records on each line its wall
// clock read as UTC: the value a brute-force reader that ignores the
// written offset gives.
func berlinDSTLines(t testing.TB) []timedLine {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	start := time.Date(2025, 10, 25, 23, 0, 0, 0, time.UTC)
	var lines []timedLine
	for i := 0; i < 40; i++ {
		at := start.Add(time.Duration(i) * 7 * time.Minute).In(berlin)
		written := at.Format("2006-01-02T15:04:05.000")
		wall, err := time.Parse("2006-01-02T15:04:05.000", written)
		if err != nil {
			t.Fatalf("parse %q: %v", written, err)
		}
		lines = append(lines, timedLine{
			text: fmt.Sprintf("%s%s INFO LINE %d", written, at.Format("-07:00"), i+1),
			ms:   wall.UnixMilli(),
		})
	}
	return lines
}

// A file whose offset changes mid-file (summer time ends) read in UTC
// answers what a brute-force reader of the written wall clocks answers:
// for every query, the first line in file order at or after it, and for
// every line, its wall clock read as UTC. The wall clocks step back an
// hour where the clocks went back, which the rule handles as any file
// whose times step back.
func TestFileZone_OffsetChangingMidFileMatchesAWallClockOracle(t *testing.T) {
	lines := berlinDSTLines(t)
	text := textOf(lines)
	var values []string
	want := map[string]int64{}
	for at := msOf(t, "2025-10-26T00:50:00Z"); at <= msOf(t, "2025-10-26T04:10:00Z"); at += 5 * 60 * 1000 {
		value := rfc3339(at)
		values = append(values, value)
		want[value] = lineAtOracle(lines, at)
	}
	resp, err := answerByTime(t, text, timeRequest{values: values, zone: zoneNamed(t, "UTC")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireLines(t, resp, want)

	path := filepath.Join(t.TempDir(), "dst.log")
	writeFile(t, path, text)
	all := []OffsetOrRange{{Start: 1, End: ptr(int64(len(lines)))}}
	got := samplesanswer.ColdAndIndexed(t, path, timeIndexStep, func(t testing.TB) any {
		resp, err := Resolve(t.Context(), Request{Path: path, Lines: all, FileZone: zoneNamed(t, "UTC"), IndexLoader: StoredIndex})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return resp
	}).(*rxtypes.SamplesResponse)
	stamps := got.LineTimestamps[all[0].Key()]
	if len(stamps) != len(lines) {
		t.Fatalf("%d line timestamps for %d lines", len(stamps), len(lines))
	}
	for i, l := range lines {
		if stamps[i] == nil || *stamps[i] != l.ms {
			t.Errorf("line %d (%q): %v, want %d", i+1, l.text, stamps[i], l.ms)
		}
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// zonedLargeLog writes a plain log of n ordered lines that write +02:00,
// about 150 bytes each, and returns its path, its lines (ms is the wall
// clock read as UTC) and its size.
func zonedLargeLog(t *testing.T, n int) (string, []timedLine, int64) {
	t.Helper()
	lines := make([]timedLine, n)
	for i := range lines {
		ms := timeBase + int64(i)*100
		stamp := time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000")
		lines[i] = timedLine{text: fmt.Sprintf("%s+02:00 INFO LINE %d %s", stamp, i+1, strings.Repeat("x", 110)), ms: ms}
	}
	path := filepath.Join(t.TempDir(), "zoned.log")
	text := textOf(lines)
	writeFile(t, path, text)
	return path, lines, int64(len(text))
}

// An index of a file whose lines write zones holds instants, which
// cannot give back the wall clocks a file zone reads: under one, the
// time search does not start at a checkpoint (max_before) but at the
// first line, and finds the line a cold search finds. Without the file
// zone the same index keeps the search to one step. For a file without
// zones, whose index holds wall clocks, the file zone keeps it to one
// step too.
func TestBudget_FileZoneOnAZonedFileSearchesFromTheStart(t *testing.T) {
	const step = 64 * 1024
	path, lines, size := zonedLargeLog(t, 20_000)
	idx, err := index.Build(path, index.BuildOptions{StepBytes: step})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	loader := func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }
	target := lines[17_345].ms + 50
	ask := func(zone config.Zone, query string, loader IndexLoader) (int64, int64) {
		t.Helper()
		counter := withCountingOpen(t)
		resp, err := Resolve(t.Context(), Request{Path: path, Timestamps: []string{query}, FileZone: zone, IndexLoader: loader})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return resp.Timestamps[query], counter.Load()
	}

	utc := zoneNamed(t, "UTC")
	query := rfc3339(target)
	line, read := ask(utc, query, loader)
	if line != 17_347 {
		t.Fatalf("under a file zone, line %d, want 17347", line)
	}
	if cold, _ := ask(utc, query, NoIndex); cold != line {
		t.Fatalf("cold line %d, indexed %d", cold, line)
	}
	if read < size*8/10 {
		t.Errorf("under a file zone read %d bytes of %d; a search from the first line reads past line 17347", read, size)
	}

	// The same moment in the file's own frame is two hours earlier.
	ownQuery := rfc3339(target - 2*3600*1000)
	ownLine, ownRead := ask(config.Zone{}, ownQuery, loader)
	if ownLine != 17_347 || ownRead > 2*step+int64(searchBufferBytes(true))+4096 {
		t.Errorf("without a file zone: line %d, read %d bytes; want 17347 within about one step", ownLine, ownRead)
	}

	plainPath, plainLines, _ := largeTimedLog(t, 20_000)
	plainIdx, err := index.Build(plainPath, index.BuildOptions{StepBytes: step})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	counter := withCountingOpen(t)
	tokyoQuery := rfc3339(plainLines[17_345].ms + 50 - 9*3600*1000)
	resp, err := Resolve(t.Context(), Request{
		Path: plainPath, Timestamps: []string{tokyoQuery}, FileZone: zoneNamed(t, "Asia/Tokyo"),
		IndexLoader: func(string) (*rxtypes.UnifiedFileIndex, error) { return plainIdx, nil },
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, read := resp.Timestamps[tokyoQuery], counter.Load(); got != 17_347 || read > 2*step+int64(searchBufferBytes(true))+4096 {
		t.Errorf("zone-less file under a file zone: line %d, read %d bytes; want 17347 within about one step", got, read)
	}
}
