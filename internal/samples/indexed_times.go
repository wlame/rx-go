package samples

import (
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// IndexedTimes is what a line index's time section says about a file's
// timestamps, read the way a request with a given file zone reads the
// file (Request.FileZone): the format as a samples answer's time_format
// gives it, and the file's first, last and highest timestamp as UTC
// instants in milliseconds.
//
// Nothing of the file is read: every member comes from the section.
// A reader of several files (the parts of a log chain) puts their times
// on one axis with it, the axis TimeRange and line_timestamps use.
type IndexedTimes struct {
	// Format is the file's format, or nil for a file whose section is
	// null (no format recognized in the first mebibyte of its text).
	Format *rxtypes.SamplesTimeFormat
	// FirstMs and LastMs are the timestamps of the first and the last
	// line, in file order, that has one; MaxMs is the highest timestamp
	// of the file. Each is nil when no line has a timestamp, or when the
	// value falls outside the years 1 to 9999 once read in the zone.
	// LastMs is also nil when it cannot be known without reading the
	// file: under a file zone, in a file whose timestamps carry zones
	// whose offset changes too often for the index to record
	// (zone_offsets null).
	FirstMs, LastMs, MaxMs *int64
	// MaxIsBound says that MaxMs is an upper bound of the file's highest
	// timestamp rather than the timestamp itself (see TimesOfIndex).
	MaxIsBound bool
}

// maxZoneOffsetMs is the largest zone offset a timestamp can carry, east
// or west of UTC: 18 hours, the bound the index's load check holds every
// stored offset to.
const maxZoneOffsetMs = 18 * 60 * msPerMinute

// TimesOfIndex returns what ti, the time section of a line index, says
// about the file's times read under fileZone (the zero Zone reads the
// file as its timestamps say).
//
// The section stores each line's value in the file's own frame: the
// instant for a file whose timestamps carry zones, the wall clock for
// one whose timestamps carry none. A request reads a file in the frame
// frameFor gives, and each stored value is turned into it with the
// segment of zone_offsets that holds its line (segmentsToFrame), then
// into an instant: the same values TimeRange and line_timestamps give.
//
// The highest timestamp needs care under a file zone. max is the line
// with the highest stored value, which in a file whose timestamps carry
// zones is the latest instant; under a file zone each line reads as the
// wall clock it writes, which is its instant plus the offset it writes.
// When the file writes one offset, every line moves by the same amount
// and max is still the highest, so MaxMs is exact. When it writes
// several (a change to daylight saving time inside the file), the line
// with the latest wall clock can be another one: a line written at
// 02:59:59+02:00 and a later one at 02:00:00+01:00. No line's wall clock
// is above max's stored value plus the largest offset any line writes,
// so that is MaxMs, and MaxIsBound is true. A caller that needs the
// line itself confirms it with a search of the file. When the offsets
// are not recorded (zone_offsets null), the largest is not known, and
// the bound uses the largest offset there is, 18 hours.
func TimesOfIndex(ti *rxtypes.TimeIndex, fileZone config.Zone) IndexedTimes {
	if ti == nil {
		return IndexedTimes{}
	}
	detected := formatOfSection(ti)
	frame := Request{FileZone: fileZone}.frameFor(detected)
	out := IndexedTimes{Format: timeFormatIn(detected, frame)}
	if ti.First == nil || ti.Last == nil || ti.Max == nil {
		return out
	}
	segments, ok := segmentsToFrame(frame, detected, ti)
	if !ok {
		// Under a file zone, the instants of a file whose offsets were
		// not recorded: the first comes back with the offset stored for
		// it, the last would need its line read again (TimeRange does
		// that), and the highest is bounded.
		firstOffset := int64(0)
		if ti.FirstZoneOffsetMinutes != nil {
			firstOffset = int64(*ti.FirstZoneOffsetMinutes) * msPerMinute
		}
		out.FirstMs = instantOf(ti.First.Ms+firstOffset, frame)
		out.MaxMs = instantOf(ti.Max.Ms+maxZoneOffsetMs, frame)
		out.MaxIsBound = true
		return out
	}
	out.FirstMs = instantOf(frameValueAt(segments, ti.First.Line, ti.First.Ms), frame)
	out.LastMs = instantOf(frameValueAt(segments, ti.Last.Line, ti.Last.Ms), frame)
	highest, exact := highestInFrame(segments, ti.Max)
	out.MaxMs = instantOf(highest, frame)
	out.MaxIsBound = !exact
	return out
}

// highestInFrame returns the highest value of a file's lines in the
// frame segments turn the stored values into, given max, the line with
// the highest stored value, and whether that value is exact.
//
// With one segment every stored value moves by the same offset, so max
// stays the highest and its value in the frame is exact. With several,
// each line's value in the frame is its stored value, at most max's,
// plus its segment's offset, at most the largest one: their sum bounds
// every line, and is returned as a bound.
func highestInFrame(segments []zoneSegment, highest *rxtypes.TimePoint) (int64, bool) {
	if len(segments) == 1 {
		return highest.Ms + segments[0].offsetMs, true
	}
	largest := segments[0].offsetMs
	for _, s := range segments[1:] {
		largest = max(largest, s.offsetMs)
	}
	return highest.Ms + largest, false
}

// TimeFormatOf returns the time_format a samples answer gives for a file
// of format detected read under fileZone.
func TimeFormatOf(detected timestamps.Format, fileZone config.Zone) *rxtypes.SamplesTimeFormat {
	return timeFormatIn(detected, Request{FileZone: fileZone}.frameFor(detected))
}

// timeFormatIn is the time_format member of an answer that reads a file
// of format detected in frame. A file whose timestamps carry zones
// reads a line without one as UTC; a file whose timestamps carry none
// was written in RX_LOG_TZ, and under a file zone every line is read in
// that zone. has_zone is the file's, whatever the frame.
func timeFormatIn(detected timestamps.Format, frame timeFrame) *rxtypes.SamplesTimeFormat {
	assumed := "UTC"
	if !frame.hasZone {
		assumed = frame.zone.Name
	}
	return &rxtypes.SamplesTimeFormat{
		Format: string(detected.Family), HasZone: detected.HasZone, AssumedZone: assumed,
	}
}
