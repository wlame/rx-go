package timestamps

import (
	"fmt"
	"testing"
	"time"
)

// allFormats is every family in every mode, both slash orders included.
func allFormats() []Format {
	var out []Format
	for _, fam := range families {
		for _, anchored := range []bool{true, false} {
			if fam.hasOrder {
				out = append(out,
					Format{Family: fam.name, Anchored: anchored, DayFirst: boolPtr(false)},
					Format{Family: fam.name, Anchored: anchored, DayFirst: boolPtr(true)})
				continue
			}
			out = append(out, Format{Family: fam.name, Anchored: anchored})
		}
	}
	return out
}

// FuzzOwn: no input panics, nothing past the window changes the
// answer, and Locate finds Own's timestamp in a span of the window.
func FuzzOwn(f *testing.F) {
	for _, bl := range ownBenchLines {
		f.Add([]byte(bl.line))
	}
	for _, seed := range []string{
		"2025-2-15 12:34:5:123 x", "Dec 10 07:49:50.123 x", "2025-12-10 07:49:50 UTC [123]:",
		"2025-12-10 12:34:56,123", "2026-10-06T12:34:56.123+02:00", "[1765375204574]",
		"2026-10-06 12:34:56:12", "06/10/69 14:34:56", "10/06/2026 12:00:00 AM", "",
	} {
		f.Add([]byte(seed))
	}
	parsers := make([]*Parser, 0, 16)
	for _, format := range allFormats() {
		parsers = append(parsers, mustParser(f, format, mtime2025))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		for _, p := range parsers {
			got, ok := p.Own(line)
			located, span, locatedOK := p.Locate(line)
			if located != got || locatedOK != ok {
				t.Fatalf("%s: Locate %+v,%t differs from Own %+v,%t", p, located, locatedOK, got, ok)
			}
			if ok && (span.Start < 0 || span.End <= span.Start || span.End > min(len(line), WindowBytes)) {
				t.Fatalf("%s: span %+v outside the window of a %d-byte line", p, span, len(line))
			}
			if len(line) <= WindowBytes {
				continue
			}
			cut, cutOK := p.Own(line[:WindowBytes])
			if got != cut || ok != cutOK {
				t.Fatalf("%s: Own reads past the window: %+v,%t vs %+v,%t", p, got, ok, cut, cutOK)
			}
		}
	})
}

// roundTripLayouts formats an instant in each family's shape. zoned
// layouts carry the zone, so they parse back to the instant; the others
// parse back to the wall-clock reading. precision is the unit the layout
// keeps.
var roundTripLayouts = []struct {
	format    Format
	prefix    string
	layout    string
	suffix    string
	zoned     bool
	precision time.Duration
}{
	{isoZoned, "", "2006-01-02T15:04:05.000Z07:00", " msg", true, time.Millisecond},
	{isoZoned, "", "2006-01-02 15:04:05 -0700", " msg", true, time.Second},
	{isoAnchored, "", "2006-01-02 15:04:05.000", " CEST msg", false, time.Millisecond},
	// Go layouts write fractions only after "." or ",", so ":000" is literal.
	{isoAnchored, "", "2006-1-2 15:4:5:000", " (x.cc:1)", false, time.Second},
	{isoAnchored, "", "2006/01/02 15:04:05,000000", " msg", false, time.Millisecond},
	{clfWindowed, "host - - ", "[02/Jan/2006:15:04:05 -0700]", ` "GET /"`, true, time.Second},
	{ctimeAnchored, "[", "Mon Jan _2 15:04:05.000000 2006", "] [x]", false, time.Millisecond},
	{syslogAnchored, "", "Jan _2 15:04:05.000", " host x", false, time.Millisecond},
	{slashMonthFirst, "", "01/02/2006 03:04:05.000 PM", " msg", false, time.Millisecond},
	{slashDayFirst, "", "02/01/2006 15:04:05", " msg", false, time.Second},
	{dottedAnchored, "", "02.01.2006 15:04:05,000", " msg", false, time.Millisecond},
}

// FuzzOwnRoundTrip: a timestamp formatted from an instant parses back to
// that instant (zoned layouts in a zoned file) or to its wall-clock
// reading (the rest, and a zoned layout in a zone-less file).
func FuzzOwnRoundTrip(f *testing.F) {
	f.Add(int64(1765353000123), int16(0))
	f.Add(int64(1709208000000), int16(120))  // 2024-02-29
	f.Add(int64(1767225599999), int16(-330)) // last ms of 2025
	f.Add(int64(950000000000), int16(0))     // 2000-02-08: 9 digits of epoch seconds
	f.Fuzz(func(t *testing.T, ms int64, offsetMinutes int16) {
		// Years 1000–9998 keep every layout at four year digits.
		const lo, hi = int64(-30610224000000), int64(253370764800000)
		if ms < lo || ms >= hi {
			ms = lo + floorMod(ms, hi-lo)
		}
		// Offsets run from -23:59 to +23:59.
		const maxOffset = 24*60 - 1
		offset := int(offsetMinutes)
		if offset < -maxOffset || offset > maxOffset {
			offset = int(floorMod(int64(offset), 2*maxOffset+1)) - maxOffset
		}
		zone := time.FixedZone("", offset*60)
		instant := time.UnixMilli(ms).In(zone)
		for _, rt := range roundTripLayouts {
			line := rt.prefix + instant.Format(rt.layout) + rt.suffix
			// The mtime is an hour after the wall-clock reading, so a year-less
			// line takes the year it was formatted with.
			p := mustParser(t, rt.format, time.UnixMilli(wallMs(instant)).Add(time.Hour))
			got, ok := p.Own([]byte(line))
			want := Stamp{Ms: wallMs(instant.Truncate(rt.precision))}
			if rt.zoned {
				want = Stamp{Ms: instant.Truncate(rt.precision).UnixMilli(), Zoned: true, OffsetMinutes: offset}
			}
			if !ok || got != want {
				t.Fatalf("%s %q: got %+v,%t; want %+v", rt.format.Family, line, got, ok, want)
			}
		}
		zonedLine := instant.Format("2006-01-02T15:04:05.000Z07:00") + " msg"
		wantWall := Stamp{Ms: wallMs(instant), Zoned: true, OffsetMinutes: offset}
		if got, ok := mustParser(t, isoAnchored, mtime2025).Own([]byte(zonedLine)); !ok || got != wantWall {
			t.Fatalf("zone-less file %q: got %+v,%t; want %+v", zonedLine, got, ok, wantWall)
		}
		epochMs := fuzzEpochMs(ms)
		p := mustParser(t, epochAnchored, mtime2025)
		for _, line := range []string{
			fmt.Sprintf("%d.%03d x", epochMs/1000, epochMs%1000),
			fmt.Sprintf("[%d] x", epochMs),
		} {
			if got, ok := p.Own([]byte(line)); !ok || got != (Stamp{Ms: epochMs, Zoned: true}) {
				t.Fatalf("epoch %q: got %+v,%t; want %d", line, got, ok, epochMs)
			}
		}
	})
}

// wallMs is a time's wall-clock reading in its own zone, as if UTC.
func wallMs(t time.Time) int64 {
	_, offset := t.Zone()
	return t.UnixMilli() + int64(offset)*1000
}

func floorMod(a, b int64) int64 { return a - floorDiv(a, b)*b }

// firstTenDigitSecondMs is the first instant whose epoch seconds have ten
// digits (2001-09-09T01:46:40Z); earlier ones print with nine.
const firstTenDigitSecondMs = int64(1000000000000)

// fuzzEpochMs maps any int64 to an instant the epoch family reads in both
// of its forms.
func fuzzEpochMs(ms int64) int64 {
	if ms >= firstTenDigitSecondMs && ms < epochMaxMs {
		return ms
	}
	return firstTenDigitSecondMs + floorMod(ms, epochMaxMs-firstTenDigitSecondMs)
}
