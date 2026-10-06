package timestamps

import (
	"strconv"
	"testing"
	"time"
)

// FuzzParseQuery: no value panics ParseQuery or Resolve, with or without
// a file's parser.
func FuzzParseQuery(f *testing.F) {
	for _, seed := range []string{
		"2026-10-06T12:34:56.123+02:00", "2026-10-06 12:34:56.5Z", "2026-10-06T12:34", "2026-10-06",
		"14:33:12", "14:33", "2:33:12 PM", "1759754096", "1759754096.5", "1759754096123",
		"14:33:12..14:35:15", "..2026-10-06", "2026-10-06..", "..", "a..b", "2026-13-01", "",
		"2025-12-10 07:00:30 MST", "Dec 10 07:49:50.123", "[06/Oct/2026:12:34:56 +0000]",
		"2025-12-10 12:34:56,123..2025-12-10 12:34:57,456", "1..2..3",
	} {
		f.Add(seed)
	}
	parsers := []*Parser{nil}
	for _, format := range allFormats() {
		parsers = append(parsers, mustParser(f, format, mtime2025))
	}
	ctx := ResolveContext{
		LogZone: time.FixedZone("", 3600), QueryZone: time.FixedZone("", -7200),
		FirstMs: utcMs(2025, 12, 10, 7, 0, 0, 0), LastMs: utcMs(2025, 12, 10, 8, 0, 0, 0), HasSpan: true,
	}
	f.Fuzz(func(t *testing.T, value string) {
		for _, p := range parsers {
			q, err := ParseQuery(value, p)
			if err != nil {
				continue
			}
			if _, err := Resolve(q, ctx); err != nil {
				t.Fatalf("ParseQuery(%q) succeeded, Resolve failed: %v", value, err)
			}
		}
	})
}

// FuzzParseQueryRoundTrip: a value formatted from an instant resolves
// back to it.
func FuzzParseQueryRoundTrip(f *testing.F) {
	f.Add(int64(1765353000123), int16(0))
	f.Add(int64(1709208000000), int16(120))
	f.Add(int64(1767225599999), int16(-330))
	f.Add(int64(950000000000), int16(0))
	f.Fuzz(func(t *testing.T, ms int64, offsetMinutes int16) {
		const lo, hi = int64(-30610224000000), int64(253370764800000) // years 1000–9998
		if ms < lo || ms >= hi {
			ms = lo + floorMod(ms, hi-lo)
		}
		const maxOffset = 24*60 - 1
		offset := int(offsetMinutes)
		if offset < -maxOffset || offset > maxOffset {
			offset = int(floorMod(int64(offset), 2*maxOffset+1)) - maxOffset
		}
		at := time.UnixMilli(ms).In(time.FixedZone("", offset*60))

		zoned := ResolveContext{HasZone: true}
		resolveTo(t, at.Format("2006-01-02T15:04:05.000Z07:00"), zoned, ms)
		zoneless := ResolveContext{}
		resolveTo(t, at.Format("2006-01-02 15:04:05.000"), zoneless, wallMs(at))

		epochMs := fuzzEpochMs(ms)
		resolveTo(t, strconv.FormatInt(epochMs, 10), zoned, epochMs)
	})
}

func resolveTo(t *testing.T, value string, ctx ResolveContext, want int64) {
	t.Helper()
	q, err := ParseQuery(value, nil)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", value, err)
	}
	r, err := Resolve(q, ctx)
	if err != nil || r.Start.Ms != want || r.Range {
		t.Fatalf("Resolve(%q) = %+v, %v; want %d", value, r, err, want)
	}
}
