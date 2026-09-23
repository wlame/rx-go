package index

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// zonedLine writes moment at with the offset of zone after it, in the
// shape `2025-10-26T02:30:00.000+02:00 INFO LINE n`; an empty layout
// writes no zone.
func zonedLine(at time.Time, zone *time.Location, layout string, n int) string {
	return fmt.Sprintf("%s%s INFO LINE %d", at.In(zone).Format("2006-01-02T15:04:05.000"), at.In(zone).Format(layout), n)
}

// offsetsText writes one line per entry of offsets, a minute apart from
// 2025-10-26 00:00 UTC, each written in a fixed zone of that many
// minutes east of UTC; nil writes the line without a zone, and a line
// starting "    at" is a continuation without a timestamp. A banner
// without a timestamp comes first.
func offsetsText(offsets []*int) []byte {
	start := time.Date(2025, 10, 26, 0, 0, 0, 0, time.UTC)
	lines := []string{"banner without a time"}
	for i, offset := range offsets {
		at := start.Add(time.Duration(i) * time.Minute)
		switch {
		case offset == nil:
			lines = append(lines, zonedLine(at, time.UTC, "", i+2))
		case *offset == continuation:
			lines = append(lines, "    at frame")
		default:
			lines = append(lines, zonedLine(at, time.FixedZone("", *offset*60), "-07:00", i+2))
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// continuation marks an offsetsText line without a timestamp.
const continuation = 1 << 20

// repeat returns n copies of the offset v (nil for none).
func repeat(v *int, n int) []*int {
	out := make([]*int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// concat joins lists of offsets.
func concat(parts ...[]*int) []*int {
	var out []*int
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// The build records where the zone offset the lines write changes: the
// first entry at the first timestamped line, then one entry at each
// line that writes another offset. A line without a timestamp changes
// nothing; a line without a zone in a file whose timestamps carry zones
// counts as offset 0, the offset its stored value is in. Plain, gzip and
// seekable copies record the same list.
func TestBuild_RecordsWhereTheWrittenZoneOffsetChanges(t *testing.T) {
	plus1, plus2, minus7, utc := intPtr(60), intPtr(120), intPtr(-420), intPtr(0)
	cont := intPtr(continuation)
	cases := []struct {
		name    string
		offsets []*int
		want    []rxtypes.ZoneOffset
	}{
		{"one offset throughout", repeat(plus2, 30), []rxtypes.ZoneOffset{{Line: 2, OffsetMinutes: 120}}},
		{"summer time ends", concat(repeat(plus2, 15), repeat(plus1, 15)),
			[]rxtypes.ZoneOffset{{Line: 2, OffsetMinutes: 120}, {Line: 17, OffsetMinutes: 60}}},
		{"a continuation between two offsets changes nothing",
			concat(repeat(plus1, 10), repeat(cont, 3), repeat(plus1, 5), repeat(plus2, 10)),
			[]rxtypes.ZoneOffset{{Line: 2, OffsetMinutes: 60}, {Line: 20, OffsetMinutes: 120}}},
		{"lines without a zone count as 0", concat(repeat(minus7, 10), repeat(nil, 2), repeat(minus7, 10)),
			[]rxtypes.ZoneOffset{{Line: 2, OffsetMinutes: -420}, {Line: 12, OffsetMinutes: 0}, {Line: 14, OffsetMinutes: -420}}},
		{"Z after an offset", concat(repeat(plus2, 10), repeat(utc, 10)),
			[]rxtypes.ZoneOffset{{Line: 2, OffsetMinutes: 120}, {Line: 12, OffsetMinutes: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := offsetsText(tc.offsets)
			dir := t.TempDir()
			for name, write := range map[string]func(*testing.T, string, []byte){
				"app.log": writePlain, "app.log.gz": writeGzip,
				"app.zst": func(t *testing.T, path string, text []byte) {
					seekablefile.Write(t, path, seekablefile.SplitEvery(text, 61))
				},
			} {
				idx := buildAt(t, dir, name, write, text, fileMtime, 256)
				if idx.TimeIndex == nil || !idx.TimeIndex.HasZone {
					t.Fatalf("%s: time_index %+v; want a zoned section", name, idx.TimeIndex)
				}
				if got := idx.TimeIndex.ZoneOffsets; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s: zone_offsets %v, want %v", name, got, tc.want)
				}
				if err := validTimeIndex(idx); err != nil {
					t.Errorf("%s: the built section does not validate: %v", name, err)
				}
			}
		})
	}
}

// A file whose timestamps carry no zone records one entry, offset 0, at
// its first timestamped line, even when some of its lines write a zone:
// those keep the wall clock they show. A file without a timestamped
// line records an empty list.
func TestBuild_ZonelessFileRecordsOneZeroOffset(t *testing.T) {
	var offsets []*int
	for i := 0; i < 40; i++ {
		if i%10 == 9 {
			offsets = append(offsets, intPtr(-420))
			continue
		}
		offsets = append(offsets, nil)
	}
	idx := buildAt(t, t.TempDir(), "app.log", writePlain, offsetsText(offsets), fileMtime, 256)
	if idx.TimeIndex == nil || idx.TimeIndex.HasZone {
		t.Fatalf("time_index %+v; want a zone-less section", idx.TimeIndex)
	}
	if got, want := idx.TimeIndex.ZoneOffsets, []rxtypes.ZoneOffset{{Line: 2, OffsetMinutes: 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("zone_offsets %v, want %v", got, want)
	}
}

// alternatingOffsets gives n timestamped lines whose offset alternates
// between +01:00 and +02:00 every line: n change points.
func alternatingOffsets(n int) []*int {
	offsets := make([]*int, n)
	for i := range offsets {
		offsets[i] = intPtr(60 + 60*(i%2))
	}
	return offsets
}

// The list holds at most MaxZoneOffsets change points. A file with
// exactly that many records them all; one with more records null, which
// a search reads as "too irregular to compensate".
func TestBuild_ZoneOffsetsPastTheLimitAreNull(t *testing.T) {
	at := buildAt(t, t.TempDir(), "app.log", writePlain, offsetsText(alternatingOffsets(MaxZoneOffsets)), fileMtime, 0)
	if got := len(at.TimeIndex.ZoneOffsets); got != MaxZoneOffsets {
		t.Errorf("%d change points recorded, want %d", got, MaxZoneOffsets)
	}
	past := buildAt(t, t.TempDir(), "app.log", writePlain, offsetsText(alternatingOffsets(MaxZoneOffsets+1)), fileMtime, 0)
	if past.TimeIndex.ZoneOffsets != nil {
		t.Errorf("%d change points recorded past the limit; want null", len(past.TimeIndex.ZoneOffsets))
	}
	if err := validTimeIndex(past); err != nil {
		t.Errorf("a null list does not validate: %v", err)
	}
}
