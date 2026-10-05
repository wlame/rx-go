package samples

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// timeRangeOf answers the time range of path with the given loader.
func timeRangeOf(t *testing.T, path string, loader IndexLoader) *rxtypes.TimeRangeResponse {
	t.Helper()
	resp, err := TimeRange(t.Context(), Request{Path: path, IndexLoader: loader})
	if err != nil {
		t.Fatalf("TimeRange(%s): %v", path, err)
	}
	return resp
}

// indexedLoader builds path's index with the tests' small step and
// returns a loader that hands it out.
func indexedLoader(t *testing.T, path string) IndexLoader {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{StepBytes: timeIndexStep})
	if err != nil {
		t.Fatalf("index.Build(%s): %v", path, err)
	}
	return func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }
}

// rangeText renders an answer for a failure message.
func rangeText(r *rxtypes.TimeRangeResponse) string {
	str := func(s *string) string {
		if s == nil {
			return "null"
		}
		return fmt.Sprintf("%q", *s)
	}
	num := func(v *int64) string {
		if v == nil {
			return "null"
		}
		return fmt.Sprint(*v)
	}
	flag := func(v *bool) string {
		if v == nil {
			return "null"
		}
		return fmt.Sprint(*v)
	}
	return fmt.Sprintf("{format %s has_zone %s day_first %s display_zone %s example %s first %s last %s source %s}",
		str(r.Format), flag(r.HasZone), flag(r.DayFirst), str(r.DisplayZone), str(r.Example),
		num(r.FirstMs), num(r.LastMs), r.Source)
}

// sameRange compares two answers in every member but source and
// cli_command, the members that say how an answer was produced.
func sameRange(a, b *rxtypes.TimeRangeResponse) bool {
	x, y := *a, *b
	x.Source, y.Source = "", ""
	return rangeText(&x) == rangeText(&y)
}

// rfc3339 writes ms as the instant a client sends: RFC 3339 with Z.
func rfc3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// timeRangeLines is an ordered log whose first and last lines carry no
// timestamp: a banner before the first record, a traceback after the
// last.
func timeRangeLines(n int) []timedLine {
	values := append([]int64{-1}, orderedValues(n)...)
	values = append(values, -1, -1)
	return isoLines(values)
}

// firstAndLastStamped returns the 1-based numbers and values of the
// first and the last line of lines with a timestamp.
func firstAndLastStamped(lines []timedLine) (firstLine, lastLine int64, firstMs, lastMs int64) {
	for i, l := range lines {
		if l.ms < 0 {
			continue
		}
		if firstLine == 0 {
			firstLine, firstMs = int64(i+1), l.ms
		}
		lastLine, lastMs = int64(i+1), l.ms
	}
	return firstLine, lastLine, firstMs, lastMs
}

// The range of a plain, a seekable and a gzip copy of one log, without
// an index and with one: the index answers what a scan reads, and a
// gzip file without an index leaves the range unknown.
func TestTimeRange_EveryStorageWithAndWithoutAnIndex(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "") // the expected instants are the wall clock read as UTC
	lines := timeRangeLines(300)
	_, _, firstMs, lastMs := firstAndLastStamped(lines)
	copies := timeCopies(t, textOf(lines))

	plain := timeRangeOf(t, copies["plain"], NoIndex)
	if plain.Source != TimeRangeFromScan || plain.FirstMs == nil || *plain.FirstMs != firstMs ||
		plain.LastMs == nil || *plain.LastMs != lastMs {
		t.Fatalf("plain without an index: %s; want first %d, last %d from a scan", rangeText(plain), firstMs, lastMs)
	}
	if plain.Example == nil || *plain.Example != "2025-12-10 07:30:00.000" {
		t.Fatalf("example %s", rangeText(plain))
	}

	wantSource := map[string]string{"plain": TimeRangeFromScan, "seekable": TimeRangeFromScan, "gzip": TimeRangeNone}
	for name, path := range copies {
		t.Run(name, func(t *testing.T) {
			cold := timeRangeOf(t, path, NoIndex)
			if cold.Source != wantSource[name] {
				t.Fatalf("source %q, want %q", cold.Source, wantSource[name])
			}
			if name == "gzip" {
				// The head gives the format and its text; the range waits
				// for an index.
				if cold.Format == nil || cold.Example == nil || cold.FirstMs != nil || cold.LastMs != nil {
					t.Fatalf("gzip without an index: %s", rangeText(cold))
				}
			} else if !sameRange(cold, plain) {
				t.Fatalf("cold %s\nplain %s", rangeText(cold), rangeText(plain))
			}
			indexed := timeRangeOf(t, path, indexedLoader(t, path))
			if indexed.Source != TimeRangeFromIndex || !sameRange(indexed, plain) {
				t.Fatalf("indexed %s\nplain   %s", rangeText(indexed), rangeText(plain))
			}
		})
	}
}

// A file in which no format is recognized answers its path and source
// with every other member null, with an index and without one.
func TestTimeRange_NoFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	writeFile(t, path, []byte("LINE 1 alpha\nLINE 2 beta\nLINE 3 gamma\n"))
	for name, loader := range map[string]IndexLoader{"cold": NoIndex, "indexed": indexedLoader(t, path)} {
		resp := timeRangeOf(t, path, loader)
		if resp.Format != nil || resp.HasZone != nil || resp.DayFirst != nil || resp.DisplayZone != nil ||
			resp.Example != nil || resp.FirstMs != nil || resp.LastMs != nil || resp.Path != path {
			t.Errorf("%s: %s; want only path and source", name, rangeText(resp))
		}
	}
}

// display_zone and example for each shape of the playground's logs,
// and the instants for a file without zones read in RX_LOG_TZ.
func TestTimeRange_DisplayZoneAndExampleOfEachShape(t *testing.T) {
	cases := []struct {
		name        string
		logTZ       string
		lines       string
		example     string
		displayZone string
		firstMs     int64
	}{
		{"middleware", "", "2025-12-10 07:00:04.574 [1765375204574] INFO a\n2025-12-10 07:00:05.000 INFO b\n2025-12-10 07:00:06.000 INFO c\n",
			"2025-12-10 07:00:04.574", "UTC", time.Date(2025, 12, 10, 7, 0, 4, 574e6, time.UTC).UnixMilli()},
		{"middleware read in Tokyo", "Asia/Tokyo", "2025-12-10 07:00:04.574 INFO a\n2025-12-10 07:00:05.000 INFO b\n2025-12-10 07:00:06.000 INFO c\n",
			"2025-12-10 07:00:04.574", "Asia/Tokyo", time.Date(2025, 12, 9, 22, 0, 4, 574e6, time.UTC).UnixMilli()},
		{"a fixed RX_LOG_TZ", "-07:00", "2025-12-10 07:00:04.574 INFO a\n2025-12-10 07:00:05.000 INFO b\n2025-12-10 07:00:06.000 INFO c\n",
			"2025-12-10 07:00:04.574", "-07:00", time.Date(2025, 12, 10, 14, 0, 4, 574e6, time.UTC).UnixMilli()},
		{"APPLOG", "", "2025-2-15 18:16:22:397 (scheduler.cc:120): a\n2025-2-15 18:16:22:5 b\n2025-2-15 18:16:23:12 c\n",
			"2025-2-15 18:16:22:397", "UTC", time.Date(2025, 2, 15, 18, 16, 22, 397e6, time.UTC).UnixMilli()},
		{"postgresql", "", "2025-12-10 07:49:50 UTC [123]: LOG a\n\tplan line\n2025-12-10 07:49:51 UTC [123]: LOG b\n2025-12-10 07:49:52 UTC [123]: LOG c\n",
			"2025-12-10 07:49:50 UTC", "+00:00", time.Date(2025, 12, 10, 7, 49, 50, 0, time.UTC).UnixMilli()},
		{"python logging", "", "2025-12-10 16:18:53,741 INFO a\n2025-12-10 16:18:54,000 INFO b\n2025-12-10 16:18:55,000 INFO c\n",
			"2025-12-10 16:18:53,741", "UTC", time.Date(2025, 12, 10, 16, 18, 53, 741e6, time.UTC).UnixMilli()},
		{"a numeric zone", "Asia/Tokyo", "2026-10-06T12:34:56.123+02:00 a\n2026-10-06T12:34:57.000+02:00 b\n2026-10-06T12:34:58.000+02:00 c\n",
			"2026-10-06T12:34:56.123+02:00", "+02:00", time.Date(2026, 10, 6, 10, 34, 56, 123e6, time.UTC).UnixMilli()},
		{"syslog", "", "Dec 10 07:00:12.156 host a\nDec 10 07:00:13.000 host b\nDec 10 07:00:14.000 host c\n",
			"Dec 10 07:00:12.156", "UTC", time.Date(2025, 12, 10, 7, 0, 12, 156e6, time.UTC).UnixMilli()},
	}
	mtime := time.Date(2025, 12, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RX_LOG_TZ", tc.logTZ)
			path := filepath.Join(t.TempDir(), "app.log")
			writeFile(t, path, []byte(tc.lines))
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatalf("chtimes: %v", err)
			}
			for name, loader := range map[string]IndexLoader{"cold": NoIndex, "indexed": indexedLoader(t, path)} {
				resp := timeRangeOf(t, path, loader)
				if resp.Example == nil || *resp.Example != tc.example || resp.DisplayZone == nil ||
					*resp.DisplayZone != tc.displayZone || resp.FirstMs == nil || *resp.FirstMs != tc.firstMs {
					t.Errorf("%s: %s; want example %q, display_zone %q, first %d",
						name, rangeText(resp), tc.example, tc.displayZone, tc.firstMs)
				}
			}
		})
	}
}

// A time query for first_ms, sent as a client sends an instant, finds
// the first timestamped line, and one for last_ms the last: for a file
// without zones under two RX_LOG_TZ values and for a file with zones,
// with an index and without.
func TestTimeRange_SamplesAtTheEndsLandOnTheFirstAndLastTimestampedLines(t *testing.T) {
	zoneless := timeRangeLines(40)
	var zoned []timedLine
	for i, l := range zoneless {
		if l.ms < 0 {
			zoned = append(zoned, l)
			continue
		}
		at := time.UnixMilli(l.ms).In(time.FixedZone("", 2*3600)).Format("2006-01-02T15:04:05.000-07:00")
		zoned = append(zoned, timedLine{text: fmt.Sprintf("%s LINE %d", at, i+1), ms: l.ms})
	}
	cases := []struct {
		name  string
		logTZ string
		lines []timedLine
	}{
		{"no zones, read in UTC", "UTC", zoneless},
		{"no zones, read in Tokyo", "Asia/Tokyo", zoneless},
		{"zones", "Asia/Tokyo", zoned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RX_LOG_TZ", tc.logTZ)
			path := filepath.Join(t.TempDir(), "app.log")
			writeFile(t, path, textOf(tc.lines))
			firstLine, lastLine, _, _ := firstAndLastStamped(tc.lines)
			for name, loader := range map[string]IndexLoader{"cold": NoIndex, "indexed": indexedLoader(t, path)} {
				r := timeRangeOf(t, path, loader)
				if r.FirstMs == nil || r.LastMs == nil {
					t.Fatalf("%s: %s", name, rangeText(r))
				}
				first, last := rfc3339(*r.FirstMs), rfc3339(*r.LastMs)
				resp, err := Resolve(t.Context(), Request{Path: path, Timestamps: []string{first, last}, IndexLoader: loader})
				if err != nil {
					t.Fatalf("%s: Resolve: %v", name, err)
				}
				if resp.Timestamps[first] != firstLine || resp.Timestamps[last] != lastLine {
					t.Errorf("%s: %s lands on line %d, %s on line %d; want %d and %d",
						name, first, resp.Timestamps[first], last, resp.Timestamps[last], firstLine, lastLine)
				}
			}
		})
	}
}

// With an index, the range is read from it: nothing of the file.
func TestBudget_IndexedTimeRangeReadsNothing(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "")
	path, lines, _ := largeTimedLog(t, 20_000)
	loader := indexedLoader(t, path)
	counter := withCountingOpen(t)
	resp := timeRangeOf(t, path, loader)
	if resp.Source != TimeRangeFromIndex || resp.LastMs == nil || *resp.LastMs != lines[len(lines)-1].ms {
		t.Fatalf("%s", rangeText(resp))
	}
	if read := counter.Load(); read != 0 {
		t.Errorf("read %d bytes of the file; want none", read)
	}
}

// Without an index, a plain file's range reads the head of the text
// and one step back from its end when that step holds a timestamp.
func TestBudget_ScannedTimeRangeReadsTheHeadAndOneTailStep(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "")
	path, lines, size := largeTimedLog(t, 20_000)
	counter := withCountingOpen(t)
	resp := timeRangeOf(t, path, NoIndex)
	if resp.Source != TimeRangeFromScan || resp.LastMs == nil || *resp.LastMs != lines[len(lines)-1].ms {
		t.Fatalf("%s", rangeText(resp))
	}
	budget := int64(timestamps.SampleBytes + tailStepBytes + timestamps.WindowBytes + 1)
	if read := counter.Load(); read > budget {
		t.Errorf("read %d bytes of %d; budget %d", read, size, budget)
	}
}

// A file whose last timestamped line starts further from the end than
// TimeRangeTailBytes answers last_ms null, having read the head and no
// more than that much of the tail.
func TestBudget_TimeRangeStopsReadingBackAtTheCap(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "")
	var text bytes.Buffer
	for i := range 100 {
		fmt.Fprintf(&text, "%s INFO LINE %d\n", time.UnixMilli(timeBase+int64(i)*1000).UTC().Format("2006-01-02 15:04:05.000"), i+1)
	}
	filler := strings.Repeat("x", 120) + "\n"
	for text.Len() < 20<<20+timestamps.SampleBytes {
		text.WriteString(filler)
	}
	path := filepath.Join(t.TempDir(), "app.log")
	writeFile(t, path, text.Bytes())

	counter := withCountingOpen(t)
	resp := timeRangeOf(t, path, NoIndex)
	if resp.Source != TimeRangeFromScan || resp.FirstMs == nil || *resp.FirstMs != timeBase || resp.LastMs != nil {
		t.Fatalf("%s; want the first timestamp and last_ms null", rangeText(resp))
	}
	// Each step reads one byte before it and a window past it.
	steps := int64(TimeRangeTailBytes / tailStepBytes)
	budget := int64(timestamps.SampleBytes+TimeRangeTailBytes) + steps*(timestamps.WindowBytes+1)
	if read := counter.Load(); read > budget {
		t.Errorf("read %d bytes of %d; budget %d", read, text.Len(), budget)
	}
}

// timeRangeHead is the head of the logs the tail tests write: 100
// timestamped lines, a second apart from timeBase.
func timeRangeHead() []byte {
	var b bytes.Buffer
	for i := range 100 {
		fmt.Fprintf(&b, "%s INFO LINE %d\n", time.UnixMilli(timeBase+int64(i)*1000).UTC().Format("2006-01-02 15:04:05.000"), i+1)
	}
	return b.Bytes()
}

// padWithFiller appends lines without a timestamp to b until it is n
// bytes long; it does nothing when b is that long already.
func padWithFiller(b *bytes.Buffer, n int) {
	filler := strings.Repeat("x", 120) + "\n"
	for n-b.Len() > len(filler) {
		b.WriteString(filler)
	}
	if rest := n - b.Len(); rest > 0 {
		b.WriteString(strings.Repeat("x", rest-1) + "\n")
	}
}

// logWithLastStampAt is a log size bytes long whose last timestamped
// line starts at byte lastAt, with lines without a timestamp around it,
// and the value of that line's timestamp.
func logWithLastStampAt(lastAt, size int) ([]byte, int64) {
	var b bytes.Buffer
	b.Write(timeRangeHead())
	padWithFiller(&b, lastAt)
	lastMs := timeBase + 3_600_000
	fmt.Fprintf(&b, "%s INFO LAST\n", time.UnixMilli(lastMs).UTC().Format("2006-01-02 15:04:05.000"))
	padWithFiller(&b, size)
	return b.Bytes(), lastMs
}

// A seekable file whose read back from the end would decode more than
// TimeRangeDecodeBytes of frames answers last_ms null, having decoded
// no more than that: a frame that would pass the limit is refused
// before it is decoded, whatever the seek table says of the file's
// compressed size.
func TestBudget_TimeRangeDecodesAtMostTheLimitOfASeekableFile(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "")
	const mib = 1 << 20
	const size = 35 * mib
	cases := []struct {
		name string
		// lastFromEnd is how far before the end of the text the last
		// timestamped line starts, well within TimeRangeTailBytes.
		lastFromEnd int
		cuts        []int
	}{
		{"a last frame larger than the limit", 1000, []int{size - 33*mib}},
		{"the last two frames larger than the limit together", 3 * mib, []int{size - 33*mib, size - 2*mib}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, _ := logWithLastStampAt(size-tc.lastFromEnd, size)
			path := filepath.Join(t.TempDir(), "app.log.zst")
			seekablefile.Write(t, path, seekablefile.SplitAt(text, tc.cuts...))

			decoded := countDecodedFrames(t)
			resp := timeRangeOf(t, path, NoIndex)
			if resp.Source != TimeRangeFromScan || resp.FirstMs == nil || *resp.FirstMs != timeBase || resp.LastMs != nil {
				t.Fatalf("%s; want the first timestamp and last_ms null from a scan", rangeText(resp))
			}
			if got := decoded.Load(); got > TimeRangeDecodeBytes {
				t.Errorf("decoded %d bytes of frames; the limit is %d", got, TimeRangeDecodeBytes)
			}
		})
	}
}

// A seekable file of 16 MiB frames, the largest the compression docs
// suggest for logs read by position, keeps its answer when the read back
// goes the whole TimeRangeTailBytes: the two frames that read covers fit
// TimeRangeDecodeBytes, and the answer is the plain copy's.
func TestTimeRange_SeekableFramesOfSixteenMiBAnswerAsPlain(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "")
	const frame = 16 << 20
	const size = 3 * frame
	// The last timestamped line starts just after the cap's floor, so
	// the read back reaches into the second frame for it.
	text, lastMs := logWithLastStampAt(size-TimeRangeTailBytes+64, size)
	plainPath := filepath.Join(t.TempDir(), "app.log")
	writeFile(t, plainPath, text)
	seekablePath := filepath.Join(t.TempDir(), "app.log.zst")
	seekablefile.Write(t, seekablePath, seekablefile.SplitEvery(text, frame))

	plain := timeRangeOf(t, plainPath, NoIndex)
	if plain.LastMs == nil || *plain.LastMs != lastMs {
		t.Fatalf("plain %s; want last %d", rangeText(plain), lastMs)
	}
	decoded := countDecodedFrames(t)
	seekableResp := timeRangeOf(t, seekablePath, NoIndex)
	if !sameRange(seekableResp, plain) {
		t.Fatalf("seekable %s\nplain    %s", rangeText(seekableResp), rangeText(plain))
	}
	if got := decoded.Load(); got > TimeRangeDecodeBytes {
		t.Errorf("decoded %d bytes of frames; the limit is %d", got, TimeRangeDecodeBytes)
	}
}

// A tail of lines that are a few bytes followed by a long run of \r
// bytes is read within the same budget as any other tail: the read back
// knows whether a run of \r bytes ends its line from the bytes the step
// after it read, and reads no further for it. Runs about a step long
// are the worst case for a read that looked forward for the line end.
func TestBudget_TimeRangeReadsACarriageReturnTailWithinTheCap(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "")
	for _, run := range []int{200 << 10, tailStepBytes - 200, tailStepBytes + 200, tailStepBytes + 4096, 3 << 19, 2*tailStepBytes + 100} {
		t.Run(fmt.Sprint(run), func(t *testing.T) {
			var text bytes.Buffer
			text.Write(timeRangeHead())
			line := "abc" + strings.Repeat("\r", run) + "\n"
			for text.Len() < 20<<20+timestamps.SampleBytes {
				text.WriteString(line)
			}
			path := filepath.Join(t.TempDir(), "app.log")
			writeFile(t, path, text.Bytes())

			counter := withCountingOpen(t)
			resp := timeRangeOf(t, path, NoIndex)
			if resp.Source != TimeRangeFromScan || resp.FirstMs == nil || *resp.FirstMs != timeBase || resp.LastMs != nil {
				t.Fatalf("%s; want the first timestamp and last_ms null", rangeText(resp))
			}
			steps := int64(TimeRangeTailBytes / tailStepBytes)
			budget := int64(timestamps.SampleBytes+TimeRangeTailBytes) + steps*(timestamps.WindowBytes+1)
			if read := counter.Load(); read > budget {
				t.Errorf("read %d bytes of %d; budget %d", read, text.Len(), budget)
			}
		})
	}
}
