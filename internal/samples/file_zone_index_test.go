package samples

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// writtenRun is a run of log lines that write one zone: count lines
// stepMs apart, the first showing the wall clock wall (milliseconds,
// read as if it were UTC), each written with zone after its timestamp
// ("" writes none).
type writtenRun struct {
	count  int
	wall   int64
	stepMs int64
	zone   string
}

// writtenLines lays runs end to end, numbering the lines `LINE n` across
// all of them. Each line's ms is the wall clock it writes, read as UTC:
// the value a file zone reads it as, whatever zone it writes.
func writtenLines(runs ...writtenRun) []timedLine {
	var lines []timedLine
	for _, run := range runs {
		for i := 0; i < run.count; i++ {
			wall := run.wall + int64(i)*run.stepMs
			stamp := time.UnixMilli(wall).UTC().Format("2006-01-02T15:04:05.000")
			lines = append(lines, timedLine{text: fmt.Sprintf("%s%s INFO LINE %d", stamp, run.zone, len(lines)+1), ms: wall})
		}
	}
	return lines
}

// wallMsAt is the wall clock zone shows at the instant ms, read as UTC.
func wallMsAt(ms int64, zone *time.Location) int64 {
	_, offset := time.UnixMilli(ms).In(zone).Zone()
	return ms + int64(offset)*1000
}

// lineAtWall is the brute-force answer under a file zone: the first line
// in file order whose written wall clock is at or after wall.
func lineAtWall(lines []timedLine, wall int64) int64 { return lineAtOracle(lines, wall) }

// zoneQueries returns queries, written with a Z, every five minutes
// and seventeen seconds over a span wide enough for any zone between
// UTC-5 and UTC+3 to cover every wall clock of lines and a little more on
// each side, and the line each must find under zone by the brute force.
func zoneQueries(lines []timedLine, zone *time.Location) ([]string, map[string]int64) {
	minWall, maxWall := int64(math.MaxInt64), int64(math.MinInt64)
	for _, l := range lines {
		minWall, maxWall = min(minWall, l.ms), max(maxWall, l.ms)
	}
	var values []string
	want := map[string]int64{}
	for at := minWall - 4*3600*1000; at <= maxWall+6*3600*1000; at += 5*60*1000 + 17*1000 {
		value := rfc3339(at)
		values = append(values, value)
		want[value] = lineAtWall(lines, wallMsAt(at, zone))
	}
	return values, want
}

// requireRangeLines checks that a range query found the lines from
// first to last (last -1: to the end of the file).
func requireRangeLines(t *testing.T, resp *rxtypes.SamplesResponse, value string, lines []timedLine, first, last int64) {
	t.Helper()
	if last < 0 {
		last = int64(len(lines))
	}
	if got := resp.Timestamps[value]; got != first {
		t.Errorf("%q starts at line %d, want %d", value, got, first)
		return
	}
	if got, want := int64(len(resp.Samples[value])), last-first+1; got != want {
		t.Errorf("%q holds %d lines, want %d (lines %d to %d)", value, got, want, first, last)
	}
}

// rangeOracle is the brute-force range under a file zone: from the line
// at wall clock fromWall to the line before the first one later than
// toWall (-1 for the end of the file).
func rangeOracle(lines []timedLine, fromWall, toWall int64) (int64, int64) {
	first := lineAtWall(lines, fromWall)
	after := lineAtWall(lines, toWall+1)
	if after < 0 {
		return first, -1
	}
	return first, after - 1
}

// Under a file zone, the index of a file whose lines write zones serves
// the time search: the query is shifted by each segment's written offset
// (zone_offsets), and the answer equals a brute-force reader of the
// written wall clocks, for every copy, cold and indexed. The fixtures
// are the shapes a file zone meets: one offset throughout, summer time
// ending (+02:00 then +01:00), an offset that grows while the wall clock
// goes back an hour (+01:00 then +02:00, so the stored instants step
// back two hours), and lines that write no zone between lines that
// write a negative one.
func TestFileZone_IndexShiftsTheQueryByEachSegmentsOffset(t *testing.T) {
	day := time.Date(2025, 10, 26, 0, 0, 0, 0, time.UTC).UnixMilli()
	hour := int64(3600 * 1000)
	fixtures := []struct {
		name  string
		lines []timedLine
	}{
		{"one offset throughout", writtenLines(writtenRun{120, day + 7*hour, 60_000, "+02:00"})},
		{"summer time ends", berlinDSTLines(t)},
		{"the wall clock goes back as the offset grows", writtenLines(
			writtenRun{60, day + 7*hour, 60_000, "+01:00"},
			writtenRun{90, day + 7*hour, 60_000, "+02:00"})},
		{"lines without a zone between negative offsets", writtenLines(
			writtenRun{40, day + 7*hour, 60_000, "-05:00"},
			writtenRun{3, day + 7*hour + 40*60_000, 60_000, ""},
			writtenRun{40, day + 7*hour + 43*60_000, 60_000, "-05:00"})},
	}
	for _, fx := range fixtures {
		for _, zoneName := range []string{"UTC", "Europe/Berlin", "+03:00", "America/New_York"} {
			t.Run(fx.name+" in "+zoneName, func(t *testing.T) {
				zone := zoneNamed(t, zoneName)
				values, want := zoneQueries(fx.lines, zone.Location)
				mid := fx.lines[len(fx.lines)/2].ms - 3600*1000
				rangeValue := rfc3339(mid-20*60_000) + ".." + rfc3339(mid+20*60_000)
				values = append(values, rangeValue)
				resp, err := answerByTime(t, textOf(fx.lines), timeRequest{values: values, zone: zone})
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				requireLines(t, resp, want)
				first, last := rangeOracle(fx.lines, wallMsAt(mid-20*60_000, zone.Location), wallMsAt(mid+20*60_000, zone.Location))
				if first > 0 && (last < 0 || last >= first) {
					requireRangeLines(t, resp, rangeValue, fx.lines, first, last)
				} else if got := resp.Timestamps[rangeValue]; got != -1 {
					t.Errorf("%q: line %d, want an empty range", rangeValue, got)
				}
			})
		}
	}
}

// A file written at +02:00 read at +03:00: an instant of 10:00Z is
// 13:00 at +03:00, so its line is the first written at 13:00 or later,
// whose stored instant is 11:00Z or later. A time of day without a date
// is read in the file zone too, with the file's first and last wall
// clocks giving the date.
func TestFileZone_PlusTwoFileReadAtPlusThree(t *testing.T) {
	day := time.Date(2025, 12, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	lines := writtenLines(writtenRun{300, day + 12*3600*1000, 30_000, "+02:00"}) // 12:00 to 14:29:30 written
	values := []string{"2025-12-10T10:00:00Z", "2025-12-10T10:00:10Z", "13:30:15", "2025-12-10T15:00:00"}
	resp, err := answerByTime(t, textOf(lines), timeRequest{values: values, zone: zoneNamed(t, "+03:00")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireLines(t, resp, map[string]int64{
		values[0]: 121, // 13:00:00 written, the 121st line
		values[1]: 122, // 13:00:30 written
		values[2]: 182, // 13:30:30 written, the first at or after 13:30:15
		values[3]: -1,  // 15:00 at +03:00 is after the last line, 14:29:30
	})
}

// A file whose written offset changes more often than the index records
// keeps zone_offsets null, and a file zone then searches it from its
// first line: still the brute-force answer, cold and indexed.
func TestFileZone_TooManyOffsetChangesSearchesFromTheStart(t *testing.T) {
	day := time.Date(2025, 10, 26, 0, 0, 0, 0, time.UTC).UnixMilli()
	var runs []writtenRun
	for i := 0; i < index.MaxZoneOffsets+1; i++ {
		zone := "+01:00"
		if i%2 == 1 {
			zone = "+02:00"
		}
		runs = append(runs, writtenRun{1, day + 7*3600*1000 + int64(i)*1000, 1000, zone})
	}
	lines := writtenLines(runs...)
	path := filepath.Join(t.TempDir(), "flip.log")
	writeFile(t, path, textOf(lines))
	if idx, err := index.Build(path, index.BuildOptions{StepBytes: timeIndexStep}); err != nil || idx.TimeIndex == nil || idx.TimeIndex.ZoneOffsets != nil {
		t.Fatalf("index.Build: %v; want a time section with zone_offsets null", err)
	}
	zone := zoneNamed(t, "Europe/Berlin")
	values, want := zoneQueries(lines, zone.Location)
	resp, err := answerByTime(t, textOf(lines), timeRequest{values: values, zone: zone})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireLines(t, resp, want)
}

// writtenLog writes the lines of runs, each padded to about 150 bytes,
// as a plain file, and returns its path, its lines and its size.
func writtenLog(t *testing.T, runs ...writtenRun) (string, []timedLine, int64) {
	t.Helper()
	lines := writtenLines(runs...)
	for i := range lines {
		lines[i].text += " " + strings.Repeat("x", 110)
	}
	path := filepath.Join(t.TempDir(), "written.log")
	text := textOf(lines)
	writeFile(t, path, text)
	return path, lines, int64(len(text))
}

// timeQueryReads resolves query on path under zone with loader, and
// returns the line it found and the bytes of the file it read.
func timeQueryReads(t *testing.T, path, query string, zone config.Zone, loader IndexLoader) (int64, int64) {
	t.Helper()
	counter := withCountingOpen(t)
	resp, err := Resolve(t.Context(), Request{Path: path, Timestamps: []string{query}, FileZone: zone, IndexLoader: loader})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return resp.Timestamps[query], counter.Load()
}

// loaderOf builds the index of path with checkpoints every step bytes
// and returns a loader that hands it out.
func loaderOf(t *testing.T, path string, step int64) IndexLoader {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{StepBytes: step})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	return func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }
}

// oneStepBudget is what an indexed time search may read for passes
// passes: a step and a read buffer each, plus the lines machinery's
// read of the answer from its checkpoint.
func oneStepBudget(step int64, passes int) int64 {
	return int64(passes+1)*(step+int64(searchBufferBytes(true))) + 4096
}

// A file where summer time ends: its wall clock goes back an hour where
// the written offset drops from +02:00 to +01:00. A query just past the
// last wall clock written at +02:00 has its line an hour of log later,
// at +01:00. Each segment is searched from its own checkpoint and only
// up to its end, so the search reads about one step per segment, not
// the hour of log between them.
func TestBudget_FileZoneSearchesEachSegmentFromItsOwnCheckpoint(t *testing.T) {
	const step = 64 * 1024
	day := time.Date(2025, 10, 26, 0, 0, 0, 0, time.UTC).UnixMilli()
	hour := int64(3600 * 1000)
	path, lines, size := writtenLog(t,
		writtenRun{4000, day + 3*hour - 4000*1000, 1000, "+02:00"}, // written 01:53:20 to 02:59:59
		writtenRun{8000, day + 2*hour, 1000, "+01:00"})             // written 02:00:00 on, an hour later in UTC
	loader := loaderOf(t, path, step)
	utc := zoneNamed(t, "UTC")
	query := rfc3339(lines[3999].ms + 500) // just past 02:59:59 written at +02:00
	line, read := timeQueryReads(t, path, query, utc, loader)
	if want := lineAtWall(lines, lines[3999].ms+500); line != want || want != 4000+3601 {
		t.Fatalf("line %d, want %d (4000 + 3601)", line, want)
	}
	if budget := oneStepBudget(step, 2); read > budget {
		t.Errorf("read %d bytes of %d; budget %d for two segments", read, size, budget)
	}
}

// With zone_offsets null (an offset that changes too often), a file
// zone searches from the first line: an index cannot shift the query.
func TestBudget_FileZoneWithoutZoneOffsetsSearchesFromTheStart(t *testing.T) {
	const step = 64 * 1024
	var runs []writtenRun
	for i := 0; i < 2*index.MaxZoneOffsets; i++ {
		zone := "+01:00"
		if i%2 == 1 {
			zone = "+02:00"
		}
		runs = append(runs, writtenRun{10, timeBase + int64(i)*10_000, 1000, zone})
	}
	path, lines, size := writtenLog(t, runs...)
	loader := loaderOf(t, path, step)
	target := lines[len(lines)-100].ms
	line, read := timeQueryReads(t, path, rfc3339(target), zoneNamed(t, "UTC"), loader)
	if want := lineAtWall(lines, target); line != want {
		t.Fatalf("line %d, want %d", line, want)
	}
	if read < size*8/10 {
		t.Errorf("read %d bytes of %d; a search from the first line reads most of the file", read, size)
	}
}
