package samples

import (
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// timeSection is a time section with the given points and zone offsets,
// of a file whose timestamps carry zones when hasZone is set.
func timeSection(hasZone bool, first, last, highest rxtypes.TimePoint, firstOffset int, offsets []rxtypes.ZoneOffset) *rxtypes.TimeIndex {
	ti := &rxtypes.TimeIndex{
		Format: "iso", Anchored: true, HasZone: hasZone, TimestampedLines: last.Line,
		First: &first, Last: &last, Max: &highest, ZoneOffsets: offsets,
	}
	if hasZone {
		ti.FirstZoneOffsetMinutes = &firstOffset
	}
	return ti
}

// TimesOfIndex gives a section's first, last and highest time as UTC
// instants in the frame a request reads the file in; the highest is an
// upper bound exactly when, under a file zone, the file writes several
// offsets, or its offsets were not recorded.
func TestTimesOfIndex(t *testing.T) {
	ms := func(h, m int) int64 { return time.Date(2025, 10, 26, h, m, 0, 0, time.UTC).UnixMilli() }
	tokyo, err := config.ParseZone("+09:00")
	if err != nil {
		t.Fatal(err)
	}
	const nine = 9 * 3600_000
	// A zone-less file: wall clocks, read in UTC (RX_LOG_TZ unset) or in
	// the file zone.
	zoneless := timeSection(false, rxtypes.TimePoint{Ms: ms(1, 0), Line: 1}, rxtypes.TimePoint{Ms: ms(2, 0), Line: 9, Offset: 90},
		rxtypes.TimePoint{Ms: ms(3, 0), Line: 5, Offset: 50}, 0, []rxtypes.ZoneOffset{{Line: 1}})
	// A zoned file that writes +02:00, then +01:00 from line 4: the
	// instant 01:00Z of line 3 is the highest, written 03:00+02:00; line
	// 5 (00:40Z) was written 01:40+01:00.
	dst := timeSection(true, rxtypes.TimePoint{Ms: ms(0, 30), Line: 1}, rxtypes.TimePoint{Ms: ms(0, 40), Line: 5, Offset: 50},
		rxtypes.TimePoint{Ms: ms(1, 0), Line: 3, Offset: 30}, 120,
		[]rxtypes.ZoneOffset{{Line: 1, OffsetMinutes: 120}, {Line: 4, OffsetMinutes: 60}})
	oneOffset := timeSection(true, rxtypes.TimePoint{Ms: ms(0, 30), Line: 1}, rxtypes.TimePoint{Ms: ms(0, 40), Line: 5, Offset: 50},
		rxtypes.TimePoint{Ms: ms(1, 0), Line: 3, Offset: 30}, 120, []rxtypes.ZoneOffset{{Line: 1, OffsetMinutes: 120}})
	unrecorded := timeSection(true, rxtypes.TimePoint{Ms: ms(0, 30), Line: 1}, rxtypes.TimePoint{Ms: ms(0, 40), Line: 5, Offset: 50},
		rxtypes.TimePoint{Ms: ms(1, 0), Line: 3, Offset: 30}, 120, nil)

	cases := []struct {
		label             string
		ti                *rxtypes.TimeIndex
		zone              config.Zone
		first, last, high *int64
		bound             bool
		assumed           string
	}{
		{"zone-less", zoneless, config.Zone{}, ptr[int64](ms(1, 0)), ptr[int64](ms(2, 0)), ptr[int64](ms(3, 0)), false, "UTC"},
		{"zone-less in a file zone", zoneless, tokyo, ptr[int64](ms(1, 0) - nine), ptr[int64](ms(2, 0) - nine), ptr[int64](ms(3, 0) - nine), false, "+09:00"},
		{"zoned, its own frame", dst, config.Zone{}, ptr[int64](ms(0, 30)), ptr[int64](ms(0, 40)), ptr[int64](ms(1, 0)), false, "UTC"},
		// Wall clocks: first 02:30, last 01:40; the highest wall clock
		// any line can write is below 01:00Z + 2 h = 03:00.
		{"zoned with two offsets in a file zone", dst, tokyo, ptr[int64](ms(2, 30) - nine), ptr[int64](ms(1, 40) - nine), ptr[int64](ms(3, 0) - nine), true, "+09:00"},
		{"zoned with one offset in a file zone", oneOffset, tokyo, ptr[int64](ms(2, 30) - nine), ptr[int64](ms(2, 40) - nine), ptr[int64](ms(3, 0) - nine), false, "+09:00"},
		// Offsets not recorded: the first comes back with its own stored
		// offset, the last is not known, the highest is bounded by the
		// largest offset there is, 18 hours.
		{"zoned, offsets not recorded, in a file zone", unrecorded, tokyo, ptr[int64](ms(2, 30) - nine), nil, ptr[int64](ms(19, 0) - nine), true, "+09:00"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := TimesOfIndex(tc.ti, tc.zone)
			if !equalPtr(got.FirstMs, tc.first) || !equalPtr(got.LastMs, tc.last) || !equalPtr(got.MaxMs, tc.high) || got.MaxIsBound != tc.bound {
				t.Fatalf("first %v last %v max %v bound %v; want %v %v %v %v", show(got.FirstMs), show(got.LastMs),
					show(got.MaxMs), got.MaxIsBound, show(tc.first), show(tc.last), show(tc.high), tc.bound)
			}
			if got.Format == nil || got.Format.Format != "iso" || got.Format.HasZone != tc.ti.HasZone || got.Format.AssumedZone != tc.assumed {
				t.Fatalf("format %+v", got.Format)
			}
		})
	}
	if got := TimesOfIndex(nil, config.Zone{}); got.Format != nil || got.FirstMs != nil {
		t.Fatalf("a file without a section: %+v", got)
	}
	none := &rxtypes.TimeIndex{Format: "iso", Anchored: true, ZoneOffsets: []rxtypes.ZoneOffset{}}
	if got := TimesOfIndex(none, config.Zone{}); got.Format == nil || got.FirstMs != nil || got.MaxMs != nil {
		t.Fatalf("a section without timestamped lines: %+v", got)
	}
}

func equalPtr(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func show(v *int64) any {
	if v == nil {
		return nil
	}
	return time.UnixMilli(*v).UTC().Format(time.RFC3339)
}
