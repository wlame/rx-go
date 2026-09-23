package samples

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// timeRangeIn answers the time range of path read in a file zone.
func timeRangeIn(t *testing.T, path string, loader IndexLoader, zone config.Zone) *rxtypes.TimeRangeResponse {
	t.Helper()
	resp, err := TimeRange(t.Context(), Request{Path: path, IndexLoader: loader, FileZone: zone})
	if err != nil {
		t.Fatalf("TimeRange(%s): %v", path, err)
	}
	return resp
}

// wantRange is the members of a range answer a file-zone test expects.
type wantRange struct {
	hasZone         bool
	displayZone     string
	example         string
	firstMs, lastMs int64
}

// requireRange checks resp against want and source.
func requireRange(t *testing.T, name string, resp *rxtypes.TimeRangeResponse, want wantRange, source string) {
	t.Helper()
	ok := resp.Source == source && resp.HasZone != nil && *resp.HasZone == want.hasZone &&
		resp.DisplayZone != nil && *resp.DisplayZone == want.displayZone &&
		resp.Example != nil && *resp.Example == want.example &&
		resp.FirstMs != nil && *resp.FirstMs == want.firstMs && resp.LastMs != nil && *resp.LastMs == want.lastMs
	if !ok {
		t.Errorf("%s: %s; want %+v from %s", name, rangeText(resp), want, source)
	}
}

// A file without zones read in Asia/Tokyo: its first and last instants
// are nine hours earlier than its wall clock read as UTC, display_zone
// is the file zone, and every copy answers the same with an index as
// without one (a gzip copy without one has no range).
func TestTimeRange_FileZoneOnAZonelessFile(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "Europe/Berlin") // the file zone takes its place
	lines := timeRangeLines(300)
	_, _, firstMs, lastMs := firstAndLastStamped(lines)
	const nine = 9 * 3600 * 1000
	want := wantRange{hasZone: false, displayZone: "Asia/Tokyo", example: "2025-12-10 07:30:00.000",
		firstMs: firstMs - nine, lastMs: lastMs - nine}
	tokyo := zoneNamed(t, "Asia/Tokyo")
	for name, path := range timeCopies(t, textOf(lines)) {
		t.Run(name, func(t *testing.T) {
			cold := timeRangeIn(t, path, NoIndex, tokyo)
			if name == "gzip" {
				if cold.Source != TimeRangeNone || cold.FirstMs != nil || cold.LastMs != nil {
					t.Errorf("gzip without an index: %s", rangeText(cold))
				}
			} else {
				requireRange(t, "cold", cold, want, TimeRangeFromScan)
			}
			requireRange(t, "indexed", timeRangeIn(t, path, indexedLoader(t, path), tokyo), want, TimeRangeFromIndex)
		})
	}
}

// berlinDSTRangeText is berlinDSTLines with a banner before the first
// record and a traceback after the last, so neither end of the file is
// a timestamped line; the first record writes +02:00 and the last
// +01:00.
func berlinDSTRangeText(t *testing.T) ([]byte, []timedLine) {
	lines := berlinDSTLines(t)
	all := append([]timedLine{{text: "banner LINE 0", ms: -1}}, lines...)
	all = append(all, timedLine{text: "    at frame", ms: -1}, timedLine{text: "    at frame", ms: -1})
	return textOf(all), all
}

// A file whose lines write zones, the offset changing mid-file, read in
// UTC: first_ms and last_ms are the first and last lines' written wall
// clocks read as UTC, each with the offset its own line writes ignored.
// With an index the two lines are read again at the offsets it stores,
// since its instants cannot give the wall clocks; the answer equals the
// scan's. A gzip copy cannot be read at an offset, so with an index too
// its range is unknown.
func TestTimeRange_FileZoneOnAZonedFile(t *testing.T) {
	text, lines := berlinDSTRangeText(t)
	_, _, firstMs, lastMs := firstAndLastStamped(lines)
	first := lines[1].text
	want := wantRange{hasZone: true, displayZone: "UTC", example: first[:strings.Index(first, " ")],
		firstMs: firstMs, lastMs: lastMs}
	if !strings.HasSuffix(want.example, "+02:00") || !strings.Contains(lines[len(lines)-3].text, "+01:00") {
		t.Fatalf("the fixture's offsets do not change: %q … %q", first, lines[len(lines)-3].text)
	}
	utc := zoneNamed(t, "UTC")
	for name, path := range timeCopies(t, text) {
		t.Run(name, func(t *testing.T) {
			indexed := timeRangeIn(t, path, indexedLoader(t, path), utc)
			if name == "gzip" {
				if indexed.Source != TimeRangeNone || indexed.FirstMs != nil || indexed.LastMs != nil ||
					indexed.DisplayZone == nil || *indexed.DisplayZone != "UTC" {
					t.Errorf("gzip with an index: %s; want no range in UTC", rangeText(indexed))
				}
				return
			}
			requireRange(t, "cold", timeRangeIn(t, path, NoIndex, utc), want, TimeRangeFromScan)
			requireRange(t, "indexed", indexed, want, TimeRangeFromIndex)
		})
	}
}

// With an index, the range of a zoned file under a file zone reads the
// stored last timestamped line, a window of it, and nothing else; the
// first comes from the index's first offset with no read.
func TestBudget_IndexedTimeRangeUnderAFileZoneReadsOneLineWindow(t *testing.T) {
	path, lines, size := zonedLargeLog(t, 20_000)
	loader := indexedLoader(t, path)
	counter := withCountingOpen(t)
	resp := timeRangeIn(t, path, loader, zoneNamed(t, "UTC"))
	if resp.Source != TimeRangeFromIndex || resp.FirstMs == nil || *resp.FirstMs != lines[0].ms ||
		resp.LastMs == nil || *resp.LastMs != lines[len(lines)-1].ms {
		t.Fatalf("%s", rangeText(resp))
	}
	if read := counter.Load(); read > timestamps.WindowBytes {
		t.Errorf("read %d bytes of %d; budget %d", read, size, timestamps.WindowBytes)
	}
}

// The line read again at a stored offset is read as the index walk reads
// it, also when its first WindowBytes bytes end in \r bytes that run on
// past the window: to the line break (dropped) or to more text (kept).
// A run of \r bytes longer than the bound leaves the line unknown.
func TestStampOfLineAt_ReadsTheLineAsTheIndexDoes(t *testing.T) {
	stamp := "2025-12-10T07:30:00.000+02:00"
	cases := map[string]string{
		"short":                            stamp + " a\n",
		"no line break at the end":         stamp + " a",
		"crlf":                             stamp + " a\r\n",
		"long":                             stamp + " " + strings.Repeat("x", 500) + "\n",
		"window of cr ending the line":     stamp + strings.Repeat("\r", 400) + "\n",
		"window of cr then more text":      stamp + strings.Repeat("\r", 400) + "tail\n",
		"cr past the bound":                stamp + strings.Repeat("\r", maxLineReadBytes+timestamps.WindowBytes+10) + "tail\n",
		"cr past the bound to the break":   stamp + strings.Repeat("\r", maxLineReadBytes+timestamps.WindowBytes+10) + "\n",
		"no timestamp":                     "LINE without a time\n",
		"a timestamp after a short prefix": "[" + stamp + "] a\n",
	}
	parser := parserFor(t, []byte(stamp+" a\n"+stamp+" b\n"))
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			text := []byte("previous LINE\n" + line + "next LINE\n")
			start := int64(len("previous LINE\n"))
			got, ok, err := stampOfLineAt(bytes.NewReader(text), int64(len(text)), start, parser)
			if err != nil {
				t.Fatalf("stampOfLineAt: %v", err)
			}
			want, wantOK := parser.Own(trimLineEnd([]byte(line)))
			if strings.Contains(name, "past the bound") {
				wantOK = false
			}
			if ok != wantOK || (ok && got != want) {
				t.Errorf("got %+v %v, want %+v %v", got, ok, want, wantOK)
			}
		})
	}
}
