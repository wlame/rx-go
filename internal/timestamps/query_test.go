package timestamps

import (
	"errors"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zone database in the binary, so the daylight saving test runs anywhere
)

func instant(ms int64) Endpoint   { return Endpoint{Kind: EndpointInstant, Ms: ms} }
func wallAt(ms int64) Endpoint    { return Endpoint{Kind: EndpointWall, Ms: ms} }
func timeOfDay(ms int64) Endpoint { return Endpoint{Kind: EndpointTimeOfDay, Ms: ms} }

func todMs(h, m, s, ms int) int64 {
	return int64(h)*msPerHour + int64(m)*msPerMinute + int64(s)*msPerSecond + int64(ms)
}

func TestParseQuery_Forms(t *testing.T) {
	cases := []struct {
		value string
		want  Endpoint
	}{
		{"2026-10-06T12:34:56.123+02:00", instant(utcMs(2026, 10, 6, 10, 34, 56, 123))},
		{"2026-10-06 12:34:56.5Z", instant(utcMs(2026, 10, 6, 12, 34, 56, 500))},
		{"2026-10-06T12:34:56-0530", instant(utcMs(2026, 10, 6, 18, 4, 56, 0))},
		{"2026-10-06 12:34:56 UTC", instant(utcMs(2026, 10, 6, 12, 34, 56, 0))},
		{"2026-10-06T12:34", wallAt(utcMs(2026, 10, 6, 12, 34, 0, 0))},
		{"2026-10-06 12:34:56", wallAt(utcMs(2026, 10, 6, 12, 34, 56, 0))},
		{"2026/10/06 12:34:56", wallAt(utcMs(2026, 10, 6, 12, 34, 56, 0))},
		{"2025-12-10 12:34:56,123", wallAt(utcMs(2025, 12, 10, 12, 34, 56, 123))},
		{"2026-10-06 12:34:56 CEST", wallAt(utcMs(2026, 10, 6, 12, 34, 56, 0))},
		{"2026-10-06", wallAt(utcMs(2026, 10, 6, 0, 0, 0, 0))},
		{"2024-2-29", wallAt(utcMs(2024, 2, 29, 0, 0, 0, 0))},
		{"14:33:12", timeOfDay(todMs(14, 33, 12, 0))},
		{"14:33", timeOfDay(todMs(14, 33, 0, 0))},
		{"14:33:12.25", timeOfDay(todMs(14, 33, 12, 250))},
		{"2:33:12 PM", timeOfDay(todMs(14, 33, 12, 0))},
		{"2:33 pm", timeOfDay(todMs(14, 33, 0, 0))},
		{"12:05 AM", timeOfDay(todMs(0, 5, 0, 0))},
		{"1759754096", instant(1759754096000)},
		{"1759754096.5", instant(1759754096500)},
		{"1759754096123", instant(1759754096123)},
		{"  2026-10-06  ", wallAt(utcMs(2026, 10, 6, 0, 0, 0, 0))},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			q, err := ParseQuery(tc.value, nil)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.value, err)
			}
			if q.Range || q.Start != tc.want || q.End.Kind != EndpointOpen {
				t.Fatalf("ParseQuery(%q) = %+v; want single %+v", tc.value, q, tc.want)
			}
		})
	}
}

// TestParseQuery_CopiedFromTheFile: a timestamp copied from a line of
// each playground shape parses with that file's parser to the value the
// line has.
func TestParseQuery_CopiedFromTheFile(t *testing.T) {
	cases := []struct {
		format Format
		line   string
		value  string
	}{
		{isoAnchored, "2025-2-15 18:16:22:397 (field_trial.cc:164): x", "2025-2-15 18:16:22:397"},
		{syslogAnchored, "Dec 10 07:49:50.123 F747D4634A6C I x", "Dec 10 07:49:50.123"},
		{isoAnchored, "2025-12-10 07:00:04.574 [1765375204574] [PatchStream] INFO", "2025-12-10 07:00:04.574"},
		{isoAnchored, "2025-12-10 07:00:30 MST [216895]: [155-1] user=sa", "2025-12-10 07:00:30 MST"},
		{isoAnchored, "2025-12-10 07:49:50 UTC [123]: LOG", "2025-12-10 07:49:50 UTC"},
		{isoAnchored, "2025-12-10 16:18:53,741 [main] [] [WARN] x", "2025-12-10 16:18:53,741"},
		{clfWindowed, `h - - [06/Oct/2026:12:34:56 +0000] "GET /"`, "[06/Oct/2026:12:34:56 +0000]"},
		{ctimeAnchored, "[Tue Oct 06 12:34:56.123456 2026] [core:error]", "[Tue Oct 06 12:34:56.123456 2026]"},
		{slashDayFirst, "06/10/2026 14:34:56 x", " 06/10/2026 14:34:56 "},
		{slashMonthFirst, "10/06/2026 2:34:56 PM x", "10/06/2026 2:34:56 PM"},
		{dottedAnchored, "06.10.2026 12:34:56,789 x", "06.10.2026 12:34:56,789"},
		{epochAnchored, "[1696600000.123] x", "[1696600000.123]"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			p := mustParser(t, tc.format, mtime2025)
			line, ok := p.Own([]byte(tc.line))
			if !ok {
				t.Fatalf("the fixture line has no timestamp: %q", tc.line)
			}
			q, err := ParseQuery(tc.value, p)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.value, err)
			}
			if want := stampEndpoint(line); q.Start != want {
				t.Fatalf("ParseQuery(%q).Start = %+v; want %+v (the line's value)", tc.value, q.Start, want)
			}
		})
	}
}

func TestParseQuery_Ranges(t *testing.T) {
	a := wallAt(utcMs(2026, 10, 6, 12, 0, 0, 0))
	b := wallAt(utcMs(2026, 10, 6, 13, 0, 0, 0))
	open := Endpoint{}
	cases := []struct {
		value      string
		start, end Endpoint
	}{
		{"2026-10-06 12:00..2026-10-06 13:00", a, b},
		{" 2026-10-06 12:00 .. 2026-10-06 13:00 ", a, b},
		{"..2026-10-06 13:00", open, b},
		{"2026-10-06 12:00..", a, open},
		{"14:33:12..14:35:15", timeOfDay(todMs(14, 33, 12, 0)), timeOfDay(todMs(14, 35, 15, 0))},
		{"1759754096.5..1759754097", instant(1759754096500), instant(1759754097000)},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			q, err := ParseQuery(tc.value, nil)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.value, err)
			}
			if !q.Range || q.Start != tc.start || q.End != tc.end {
				t.Fatalf("ParseQuery(%q) = %+v; want range %+v..%+v", tc.value, q, tc.start, tc.end)
			}
		})
	}
}

func TestParseQuery_Invalid(t *testing.T) {
	for _, value := range []string{
		"", "   ", "..", " .. ", "a..b", "2026-13-01", "2026-02-30", "2026-10-06T25:00",
		"2026-10-06 12:34 foo", "2026-10-06Z", "2026-10-06 2:34 PM", "14:60", "13:00 PM", "1..2..3",
		"946684799", "4102444800", "2026", "Dec 10 07:49:50", "06/10/2026 14:34:56",
		strings.Repeat("1", MaxQueryBytes+1),
	} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseQuery(value, nil)
			if !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("ParseQuery(%q) error = %v; want ErrInvalidQuery", value, err)
			}
			if !strings.Contains(err.Error(), "accepted:") {
				t.Fatalf("ParseQuery(%q) error does not list the accepted forms: %v", value, err)
			}
		})
	}
}

// TestParseQuery_CommaIsNotASeparator: a comma stays inside its value.
func TestParseQuery_CommaIsNotASeparator(t *testing.T) {
	q, err := ParseQuery("2025-12-10 12:34:56,123..2025-12-10 12:34:57,456", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Query{
		Value: "2025-12-10 12:34:56,123..2025-12-10 12:34:57,456",
		Range: true,
		Start: wallAt(utcMs(2025, 12, 10, 12, 34, 56, 123)),
		End:   wallAt(utcMs(2025, 12, 10, 12, 34, 57, 456)),
	}
	if q != want {
		t.Fatalf("got %+v; want %+v", q, want)
	}
	if _, err := ParseQuery("2026-10-06,2026-10-07", nil); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("a comma-separated pair parsed: %v", err)
	}
}

func TestQuery_NeedsSpan(t *testing.T) {
	for value, want := range map[string]bool{
		"14:33":               true,
		"2026-10-06..14:33":   true,
		"2026-10-06 14:33":    false,
		"..1759754096":        false,
		"2026-10-06T14:33Z..": false,
	} {
		q, err := ParseQuery(value, nil)
		if err != nil {
			t.Fatal(err)
		}
		if q.NeedsSpan() != want {
			t.Errorf("ParseQuery(%q).NeedsSpan() = %t; want %t", value, q.NeedsSpan(), want)
		}
	}
}

func TestResolve(t *testing.T) {
	plus1 := time.FixedZone("+01:00", 3600)
	plus2 := time.FixedZone("+02:00", 7200)
	w := utcMs(2025, 12, 10, 12, 0, 0, 0) // a wall-clock reading
	oneDay := ResolveContext{FirstMs: utcMs(2025, 12, 10, 7, 0, 0, 0), LastMs: utcMs(2025, 12, 10, 23, 30, 0, 0), HasSpan: true}
	withZones := func(c ResolveContext) ResolveContext { c.HasZone, c.FirstOffsetMinutes = true, 120; return c }
	withLogZone := func(c ResolveContext, z *time.Location) ResolveContext { c.LogZone = z; return c }
	withQueryZone := func(c ResolveContext, z *time.Location) ResolveContext { c.QueryZone = z; return c }

	cases := []struct {
		name  string
		value string
		ctx   ResolveContext
		want  int64
	}{
		{"zone-less file, zone-less query: as written", "2025-12-10 12:00", oneDay, w},
		{"zone-less file, instant query, UTC logs", "2025-12-10T12:00Z", oneDay, w},
		{"zone-less file, instant query, logs at +01:00", "2025-12-10T12:00Z", withLogZone(oneDay, plus1), w + msPerHour},
		{"zone-less file, query zone +02:00, logs at +01:00", "2025-12-10 12:00", withQueryZone(withLogZone(oneDay, plus1), plus2), w - msPerHour},
		{"zoned file, instant query", "2025-12-10T12:00+02:00", withZones(oneDay), w - 2*msPerHour},
		{"zoned file, zone-less query in the first offset", "2025-12-10 12:00", withZones(oneDay), w - 2*msPerHour},
		{"zoned file, zone-less query in the query zone", "2025-12-10 12:00", withQueryZone(withZones(oneDay), plus1), w - msPerHour},
		{"time of day in a one-day file", "12:00", oneDay, w},
		{"time of day with a fraction", "12:00:00.5", oneDay, w + 500},
		{"epoch query, logs at +01:00", "1765368000", withLogZone(oneDay, plus1), w + msPerHour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := ParseQuery(tc.value, nil)
			if err != nil {
				t.Fatal(err)
			}
			r, err := Resolve(q, tc.ctx)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.value, err)
			}
			want := Resolved{Start: Bound{Ms: tc.want}, End: Bound{Open: true}}
			if r != want {
				t.Fatalf("Resolve(%q) = %+v; want %+v", tc.value, r, want)
			}
		})
	}
}

func TestResolve_Range(t *testing.T) {
	ctx := ResolveContext{FirstMs: utcMs(2025, 12, 10, 7, 0, 0, 0), LastMs: utcMs(2025, 12, 10, 23, 0, 0, 0), HasSpan: true}
	q, err := ParseQuery("14:33:12..14:35:15", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(q, ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Resolved{Range: true, Start: Bound{Ms: utcMs(2025, 12, 10, 14, 33, 12, 0)}, End: Bound{Ms: utcMs(2025, 12, 10, 14, 35, 15, 0)}}
	if r != want {
		t.Fatalf("got %+v; want %+v", r, want)
	}
	q, err = ParseQuery("..2025-12-10 14:35", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = Resolve(q, ResolveContext{}); err != nil || !r.Start.Open || r.End.Open {
		t.Fatalf("open start: got %+v, %v", r, err)
	}
}

// TestResolve_TimeOfDayDate: the date of a time-only query is the one
// date the file covers where the query is read.
func TestResolve_TimeOfDayDate(t *testing.T) {
	twoDays := ResolveContext{FirstMs: utcMs(2025, 12, 10, 22, 0, 0, 0), LastMs: utcMs(2025, 12, 11, 1, 0, 0, 0), HasSpan: true}
	q, err := ParseQuery("00:30", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(q, twoDays)
	if !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "2025-12-10") || !strings.Contains(err.Error(), "2025-12-11") {
		t.Fatalf("a two-day file: error = %v; want both dates", err)
	}
	// The same file read at -03:00 covers one day, 2025-12-10.
	minus3 := time.FixedZone("-03:00", -3*3600)
	twoDays.QueryZone = minus3
	r, err := Resolve(q, twoDays)
	if err != nil {
		t.Fatal(err)
	}
	if want := utcMs(2025, 12, 10, 3, 30, 0, 0); r.Start.Ms != want {
		t.Fatalf("got %d; want %d", r.Start.Ms, want)
	}
	if _, err = Resolve(q, ResolveContext{}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("no span: error = %v; want ErrInvalidQuery", err)
	}
}

// TestResolve_DaylightSaving: a zone-less file's wall clock is read in
// LogZone with the offset in force at that moment.
func TestResolve_DaylightSaving(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	ctx := ResolveContext{LogZone: berlin}
	for value, wantWall := range map[string]int64{
		"2025-07-01T10:00Z": utcMs(2025, 7, 1, 12, 0, 0, 0),  // CEST, +02:00
		"2025-12-01T10:00Z": utcMs(2025, 12, 1, 11, 0, 0, 0), // CET, +01:00
	} {
		q, err := ParseQuery(value, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Resolve(q, ctx)
		if err != nil || r.Start.Ms != wantWall {
			t.Errorf("Resolve(%q) = %+v, %v; want %d", value, r, err, wantWall)
		}
	}
}
