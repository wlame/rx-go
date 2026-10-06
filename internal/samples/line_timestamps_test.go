package samples

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// lineNumberPattern finds the number every generated line carries.
var lineNumberPattern = regexp.MustCompile(`LINE (\d+)`)

// lineNumberIn returns the number written on a generated line.
func lineNumberIn(t testing.TB, text string) int64 {
	t.Helper()
	m := lineNumberPattern.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("line %q carries no LINE number", text)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("line %q: %v", text, err)
	}
	return n
}

// effectiveOracle is the effective-timestamp rule written the plainest
// way, over the whole text: each line's own timestamp, or else the own
// timestamp of the nearest earlier line that has one when that line
// starts at most lookback bytes before it; nil otherwise. Values are in
// the file's frame, keyed by line number.
func effectiveOracle(t testing.TB, text []byte, lookback int64) map[int64]*int64 {
	t.Helper()
	parser := parserFor(t, text)
	out := map[int64]*int64{}
	var start int64
	var stampedStart, stampedMs int64
	hasStamped := false
	for i, line := range bytes.SplitAfter(text, []byte("\n")) {
		if len(line) == 0 {
			break
		}
		number := int64(i + 1)
		if stamp, ok := index.LineStamp(parser, line); ok {
			stampedStart, stampedMs, hasStamped = start, stamp.Ms, true
		}
		if hasStamped && start-stampedStart <= lookback {
			ms := stampedMs
			out[number] = &ms
		} else {
			out[number] = nil
		}
		start += int64(len(line))
	}
	return out
}

// shifted returns the oracle's values moved by delta milliseconds.
func shifted(values map[int64]*int64, delta int64) map[int64]*int64 {
	out := make(map[int64]*int64, len(values))
	for n, v := range values {
		if v != nil {
			ms := *v + delta
			out[n] = &ms
		} else {
			out[n] = nil
		}
	}
	return out
}

// requireLineTimestamps checks that line_timestamps has the keys of
// samples, null for a null sample, and for every sample line the value
// want gives the line it names.
func requireLineTimestamps(t testing.TB, resp *rxtypes.SamplesResponse, want map[int64]*int64) {
	t.Helper()
	if resp.LineTimestamps == nil {
		t.Fatalf("line_timestamps is null for a timestamped file")
	}
	if len(resp.LineTimestamps) != len(resp.Samples) {
		t.Errorf("line_timestamps has %d keys, samples %d", len(resp.LineTimestamps), len(resp.Samples))
	}
	for key, sample := range resp.Samples {
		got, ok := resp.LineTimestamps[key]
		if !ok {
			t.Errorf("line_timestamps lacks key %q", key)
			continue
		}
		if sample == nil {
			if got != nil {
				t.Errorf("%q: line_timestamps %v for a null sample, want null", key, got)
			}
			continue
		}
		if len(got) != len(sample) {
			t.Errorf("%q: %d timestamps for %d lines", key, len(got), len(sample))
			continue
		}
		for i, text := range sample {
			n := lineNumberIn(t, text)
			if describeMs(got[i]) != describeMs(want[n]) {
				t.Errorf("%q: line %d has %s, want %s", key, n, describeMs(got[i]), describeMs(want[n]))
			}
		}
	}
}

// describeMs prints a nullable timestamp for a failure message.
func describeMs(ms *int64) string {
	if ms == nil {
		return "null"
	}
	return time.UnixMilli(*ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// lineStartsOf returns the offset each line of text starts at.
func lineStartsOf(text []byte) []int64 {
	starts := []int64{0}
	for i, b := range text {
		if b == '\n' && i+1 < len(text) {
			starts = append(starts, int64(i+1))
		}
	}
	return starts
}

// askEveryCopy runs ask on the plain, gzip and seekable copies of text,
// each cold and after an index build, requires every answer to agree
// with the plain file's cold one, and returns the plain file's answers.
// ask returns the answers of one copy, in a fixed order.
func askEveryCopy(t *testing.T, text []byte, ask func(t testing.TB, path string) []*rxtypes.SamplesResponse) []*rxtypes.SamplesResponse {
	t.Helper()
	var want any
	var plain []*rxtypes.SamplesResponse
	for _, name := range []string{"plain", "gzip", "seekable"} {
		path := timeCopies(t, text)[name]
		got := samplesanswer.ColdAndIndexed(t, path, timeIndexStep, func(t testing.TB) any {
			answers := ask(t, path)
			if name == "plain" && plain == nil {
				plain = answers
			}
			docs := make([]any, len(answers))
			for i, resp := range answers {
				docs[i] = answerOf(resp, nil)
			}
			return docs
		})
		if name == "plain" {
			want = got
			continue
		}
		samplesanswer.RequireAgree(t, name+" copy against the plain file", got, want)
	}
	return plain
}

// resolveOrFail resolves req with the stored index, as `rx samples`
// does, or fails the test.
func resolveOrFail(t testing.TB, req Request) *rxtypes.SamplesResponse {
	t.Helper()
	req.IndexLoader = StoredIndex
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return resp
}

// Every mode annotates each sample line with its effective timestamp:
// lines with their own timestamp, continuation lines inside a sample
// and windows whose first line is a continuation line, which read back
// for the record's line before the window. Single positions, ranges,
// several keys, a line counted from the end, one past the end and line
// 0 (both null).
func TestLineTimestamps_EveryModeFollowsTheRule(t *testing.T) {
	text := textOf(isoLines(orderedValues(200)))
	starts := lineStartsOf(text)
	want := effectiveOracle(t, text, 64*1024)
	rangeEnd, offsetEnd := int64(31), starts[47]
	answers := askEveryCopy(t, text, func(t testing.TB, path string) []*rxtypes.SamplesResponse {
		byLines := resolveOrFail(t, Request{
			Path: path, BeforeContext: 1, AfterContext: 2,
			Lines: []OffsetOrRange{{Start: 5}, {Start: 15}, {Start: 25, End: &rangeEnd}, {Start: -1}, {Start: 500}, {Start: 0}},
		})
		byOffsets := resolveOrFail(t, Request{
			Path: path, BeforeContext: 1, AfterContext: 2,
			Offsets: []OffsetOrRange{{Start: starts[14] + 3}, {Start: starts[44], End: &offsetEnd}, {Start: int64(len(text)) + 10}},
		})
		byTime := resolveOrFail(t, Request{
			Path: path, BeforeContext: 2, AfterContext: 1,
			Timestamps: []string{iso(timeBase + 5000), iso(timeBase+3000) + ".." + iso(timeBase+9000)},
		})
		return []*rxtypes.SamplesResponse{byLines, byOffsets, byTime}
	})
	for _, resp := range answers {
		requireLineTimestamps(t, resp, want)
	}
	// Line 4 is the first line of the window of line 5 and has no
	// timestamp of its own: it carries line 3's.
	if got := answers[0].LineTimestamps["5"]; len(got) != 4 || got[0] == nil || *got[0] != timeBase+2000 {
		t.Errorf("line_timestamps[5] = %v, want line 3's timestamp first", got)
	}
}

// The stamp a sample line is annotated with is the one an index build
// reads from the same line: the sample text has lost its line break
// (and one \r before it), which the parser does not look at.
func TestLineTimestamps_SampleTextReadsAsTheRawLine(t *testing.T) {
	parser, err := timestamps.NewParser(timestamps.Format{Family: timestamps.FamilyISO}, 0)
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	for i, raw := range awkwardLines(rand.New(rand.NewPCG(13, 17))) {
		want, wantOK := index.LineStamp(parser, raw)
		for _, text := range []string{stripNewline(string(raw)), trimNewline(string(raw))} {
			if got, ok := index.LineStamp(parser, []byte(text)); got != want || ok != wantOK {
				t.Fatalf("line %d: sample text reads %+v %v, the raw line %+v %v", i+1, got, ok, want, wantOK)
			}
		}
	}
}

// tracebackLog is a log with CRLF line breaks: a record and a traceback
// of 10 lines, then a record and a traceback of 40 lines, then a last
// record. Every line is 52 bytes with its line break, so line k of the
// long traceback (lines 13 to 52) starts 52·(k−12) bytes after line 12.
func tracebackLog() []byte {
	var b strings.Builder
	line := func(text string) {
		b.WriteString(text + strings.Repeat(".", 50-len(text)) + "\r\n")
	}
	record := func(n int, ms int64) {
		line(fmt.Sprintf("%s INFO LINE %d", time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05.000"), n))
	}
	record(1, timeBase)
	for n := 2; n <= 11; n++ {
		line(fmt.Sprintf("    at frame LINE %d", n))
	}
	record(12, timeBase+1000)
	for n := 13; n <= 52; n++ {
		line(fmt.Sprintf("    at frame LINE %d", n))
	}
	record(53, timeBase+2000)
	return []byte(b.String())
}

// A traceback inherits its record's timestamp up to the lookback limit
// and no further, counted in bytes with each line's \r\n. With a
// lookback of 1 KiB, line 31 starts 988 bytes after its record and
// inherits; line 32 starts 1,040 bytes after it and does not. A window
// that starts on either line reads back for the record before it.
func TestLineTimestamps_LookbackLimit(t *testing.T) {
	t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", "1")
	text := tracebackLog()
	want := effectiveOracle(t, text, 1024)
	whole, middle := int64(53), int64(40)
	answers := askEveryCopy(t, text, func(t testing.TB, path string) []*rxtypes.SamplesResponse {
		return []*rxtypes.SamplesResponse{resolveOrFail(t, Request{
			Path:  path,
			Lines: []OffsetOrRange{{Start: 1, End: &whole}, {Start: 31}, {Start: 32}, {Start: 20, End: &middle}},
		})}
	})
	resp := answers[0]
	requireLineTimestamps(t, resp, want)
	all := resp.LineTimestamps["1-53"]
	for n := 1; n <= 11; n++ {
		if all[n-1] == nil || *all[n-1] != timeBase {
			t.Errorf("line %d: %s, want the first record's timestamp", n, describeMs(all[n-1]))
		}
	}
	if got := resp.LineTimestamps["31"]; len(got) != 1 || got[0] == nil || *got[0] != timeBase+1000 {
		t.Errorf("line 31: %v, want the second record's timestamp", got)
	}
	if got := resp.LineTimestamps["32"]; len(got) != 1 || got[0] != nil {
		t.Errorf("line 32: %v, want null", got)
	}
}

// With a lookback of 0, a line without its own timestamp has none.
func TestLineTimestamps_ZeroLookback(t *testing.T) {
	t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", "0")
	text := tracebackLog()
	want := effectiveOracle(t, text, 0)
	whole := int64(53)
	answers := askEveryCopy(t, text, func(t testing.TB, path string) []*rxtypes.SamplesResponse {
		return []*rxtypes.SamplesResponse{resolveOrFail(t, Request{
			Path: path, Lines: []OffsetOrRange{{Start: 1, End: &whole}, {Start: 2}},
		})}
	})
	requireLineTimestamps(t, answers[0], want)
	for i, ms := range answers[0].LineTimestamps["1-53"] {
		if stamped := i+1 == 1 || i+1 == 12 || i+1 == 53; stamped != (ms != nil) {
			t.Errorf("line %d: %s", i+1, describeMs(ms))
		}
	}
}

// A zone-less file is read in RX_LOG_TZ: in Tokyo, every value is 9
// hours before the wall clock written on the line.
func TestLineTimestamps_ZonelessFileInLogZone(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "Asia/Tokyo")
	text := textOf(isoLines(orderedValues(30)))
	want := shifted(effectiveOracle(t, text, 64*1024), -9*3600*1000)
	end := int64(30)
	answers := askEveryCopy(t, text, func(t testing.TB, path string) []*rxtypes.SamplesResponse {
		return []*rxtypes.SamplesResponse{resolveOrFail(t, Request{Path: path, Lines: []OffsetOrRange{{Start: 1, End: &end}}})}
	})
	requireLineTimestamps(t, answers[0], want)
}

// A zoned file's values are the instants its lines name, whatever
// RX_LOG_TZ says.
func TestLineTimestamps_ZonedFile(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "Asia/Tokyo")
	text := textOf(zonedLines(20, timeBase, "+02:00"))
	end := int64(20)
	answers := askEveryCopy(t, text, func(t testing.TB, path string) []*rxtypes.SamplesResponse {
		return []*rxtypes.SamplesResponse{resolveOrFail(t, Request{Path: path, Lines: []OffsetOrRange{{Start: 1, End: &end}}})}
	})
	got := answers[0].LineTimestamps["1-20"]
	if len(got) != 20 {
		t.Fatalf("line_timestamps[1-20] = %v, want 20 values", got)
	}
	for i, ms := range got {
		if want := timeBase - 2*3600*1000 + int64(i)*1000; ms == nil || *ms != want {
			t.Errorf("line %d: %s, want %s", i+1, describeMs(ms), describeMs(&want))
		}
	}
}

// A file without a timestamp format answers line_timestamps null, and
// the key is present in the JSON answer.
func TestLineTimestamps_FileWithoutTimestamps(t *testing.T) {
	path := timeCopies(t, []byte(strings.Repeat("no time here LINE 1\n", 50)))["plain"]
	resp := resolveOrFail(t, Request{Path: path, Lines: []OffsetOrRange{{Start: 3}}})
	if resp.LineTimestamps != nil {
		t.Fatalf("line_timestamps %v, want null", resp.LineTimestamps)
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"line_timestamps":null`)) {
		t.Errorf("answer %s lacks \"line_timestamps\":null", encoded)
	}
}

// A value that RX_LOG_TZ moves outside the years 1 to 9999 is null: a
// wall clock at the first moment of year 1, read east of UTC, is an
// instant before it.
func TestLineTimestamps_ValueOutsideTheYearRangeIsNull(t *testing.T) {
	// isoLines takes a negative value for a continuation line, and year
	// 1 is before 1970, so these lines are written out.
	text := []byte("0001-01-01 00:00:00.000 INFO LINE 1\n0001-01-01 00:00:01.000 INFO LINE 2\n" +
		"    at frame LINE 3\n0001-01-01 00:00:03.000 INFO LINE 4\n")
	path := timeCopies(t, text)["plain"]
	end := int64(4)
	for zone, wantNull := range map[string]bool{"+05:00": true, "-05:00": false} {
		t.Setenv("RX_LOG_TZ", zone)
		resp := resolveOrFail(t, Request{Path: path, Lines: []OffsetOrRange{{Start: 1, End: &end}}})
		if got := resp.LineTimestamps["1-4"]; len(got) != 4 {
			t.Fatalf("RX_LOG_TZ=%s: line_timestamps[1-4] = %v, want 4 values", zone, got)
		}
		for i, ms := range resp.LineTimestamps["1-4"] {
			if (ms == nil) != wantNull {
				t.Errorf("RX_LOG_TZ=%s: line %d is %s", zone, i+1, describeMs(ms))
			}
		}
	}
}
