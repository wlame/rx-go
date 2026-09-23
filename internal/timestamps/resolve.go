package timestamps

import (
	"fmt"
	"time"
)

// ResolveContext is what [Resolve] needs to know about a file and the
// caller's settings to turn query endpoints into the file's frame. The
// package reads no environment: the caller fills this in.
type ResolveContext struct {
	// HasZone is the file's Format.HasZone. A file with zones has its
	// values as UTC instants; a file without has them as wall-clock
	// readings (the file frame).
	HasZone bool
	// FirstOffsetMinutes is the zone offset of the file's first
	// timestamp, for a file with zones. A query without a zone is read
	// at this offset when QueryZone is nil, so a time copied from the
	// file finds its line.
	FirstOffsetMinutes int
	// LogZone is the zone a file without zones was written in: its
	// wall-clock values are read in it to compare them with an instant.
	// nil means UTC.
	LogZone *time.Location
	// QueryZone is the zone a query without a zone is read in. nil
	// means the file's own frame: a zone-less file's wall clock, or a
	// zoned file's first offset.
	QueryZone *time.Location
	// FirstMs and LastMs are the file's first and last own timestamps,
	// in the file's frame. Only a time-of-day endpoint needs them (see
	// Query.NeedsSpan); HasSpan says they are filled in.
	FirstMs, LastMs int64
	HasSpan         bool
}

// Bound is one resolved end of a query, in the file's frame: what
// Stamp.Ms holds for the file's lines, so the two compare directly.
type Bound struct {
	Ms int64
	// Open is true for the missing side of a range: a range open at the
	// start begins at the file's first line, one open at the end runs
	// to its last.
	Open bool
}

// Resolved is a query turned into the file's frame.
type Resolved struct {
	// Range is true for a range query.
	Range bool
	// Start is the moment of a single query, or the start of a range.
	Start Bound
	// End is the inclusive end of a range (at millisecond precision);
	// open for a single query.
	End Bound
}

// Resolve turns a parsed query into bounds in the file's frame:
//
//   - an instant becomes itself for a file with zones, or its wall-clock
//     reading in LogZone for a file without;
//   - a date and time without a zone is read in QueryZone when it is
//     set, and otherwise in the file's own frame;
//   - a time of day takes its date from the file: the date of FirstMs
//     and LastMs, read where the query is read, when the two are the
//     same; otherwise the query is refused with both dates, since the
//     time would name a moment on each.
//
// A bound that lands outside the years 1 to 9999 in the file's frame is
// refused, as a line there would have no timestamp.
//
// Errors wrap [ErrInvalidQuery].
func Resolve(q Query, c ResolveContext) (Resolved, error) {
	out := Resolved{Range: q.Range}
	var err error
	if out.Start, err = c.resolveEndpoint(q, q.Start); err != nil {
		return out, err
	}
	if out.End, err = c.resolveEndpoint(q, q.End); err != nil {
		return out, err
	}
	for _, b := range [...]Bound{out.Start, out.End} {
		if !b.Open && !InValueRange(b.Ms) {
			return out, fmt.Errorf("%w %q: names a moment outside the years 1 to 9999", ErrInvalidQuery, q.Value)
		}
	}
	return out, nil
}

// resolveEndpoint resolves one endpoint of q.
func (c ResolveContext) resolveEndpoint(q Query, e Endpoint) (Bound, error) {
	switch e.Kind {
	case EndpointInstant:
		return Bound{Ms: c.instantToFile(e.Ms)}, nil
	case EndpointWall:
		return Bound{Ms: c.wallToFile(e.Ms)}, nil
	case EndpointTimeOfDay:
		day, err := c.spanDay(q)
		if err != nil {
			return Bound{}, err
		}
		return Bound{Ms: c.wallToFile(day*msPerDay + e.Ms)}, nil
	default:
		return Bound{Open: true}, nil
	}
}

// spanDay returns the one day, counted from 1970-01-01 in the frame a
// query is read in, that the file's first and last timestamps fall on.
func (c ResolveContext) spanDay(q Query) (int64, error) {
	if !c.HasSpan {
		return 0, fmt.Errorf("%w %q: a time with no date takes its date from the file, "+
			"and the file has no timestamped line", ErrInvalidQuery, q.Value)
	}
	first := floorDiv(c.fileToWall(c.FirstMs), msPerDay)
	last := floorDiv(c.fileToWall(c.LastMs), msPerDay)
	if first != last {
		return 0, fmt.Errorf("%w %q: a time with no date takes its date from the file, "+
			"and the file runs from %s to %s; give the date too (%sT…)",
			ErrInvalidQuery, q.Value, dayString(first), dayString(last), dayString(first))
	}
	return first, nil
}

// instantToFile turns a UTC instant into the file's frame.
func (c ResolveContext) instantToFile(instant int64) int64 {
	if c.HasZone {
		return instant
	}
	return instant + offsetMsAt(instant, c.LogZone)
}

// fileToInstant is the inverse of instantToFile.
func (c ResolveContext) fileToInstant(fileMs int64) int64 {
	return fileValueToInstant(fileMs, c.HasZone, c.LogZone)
}

// fileValueToInstant turns fileMs, a value in the frame of a file whose
// Format has HasZone = hasZone, into a UTC instant: as it is for a file
// with zones, read in logZone (nil = UTC) for a file without.
func fileValueToInstant(fileMs int64, hasZone bool, logZone *time.Location) int64 {
	if hasZone {
		return fileMs
	}
	return wallToInstant(fileMs, logZone)
}

// InstantOf returns the UTC instant, in milliseconds since the Unix
// epoch, of fileMs: a Stamp.Ms of a file whose Format has HasZone =
// hasZone. A file with zones holds instants already; a file without
// holds wall-clock readings, which are read in logZone, the zone the
// file was written in (nil means UTC).
//
// It returns false when the instant lies outside the years 1 to 9999.
// Every value the parser returns is inside them, but reading a wall
// clock near either end in a zone can step outside, and such a value
// could break an answer that renders it as a date.
func InstantOf(fileMs int64, hasZone bool, logZone *time.Location) (int64, bool) {
	instant := fileValueToInstant(fileMs, hasZone, logZone)
	return instant, InValueRange(instant)
}

// wallToFile turns a wall-clock reading without a zone into the file's
// frame, reading it in QueryZone or, when that is nil, in the file's
// own frame.
func (c ResolveContext) wallToFile(wall int64) int64 {
	if c.QueryZone != nil {
		return c.instantToFile(wallToInstant(wall, c.QueryZone))
	}
	if c.HasZone {
		return wall - int64(c.FirstOffsetMinutes)*msPerMinute
	}
	return wall
}

// fileToWall is the inverse of wallToFile: a file value as the wall
// clock reads it where the query is read.
func (c ResolveContext) fileToWall(fileMs int64) int64 {
	if c.QueryZone != nil {
		instant := c.fileToInstant(fileMs)
		return instant + offsetMsAt(instant, c.QueryZone)
	}
	if c.HasZone {
		return fileMs + int64(c.FirstOffsetMinutes)*msPerMinute
	}
	return fileMs
}

// offsetMsAt is the offset of zone loc from UTC at an instant, in ms.
// nil means UTC.
func offsetMsAt(instant int64, loc *time.Location) int64 {
	if loc == nil {
		return 0
	}
	_, seconds := time.UnixMilli(instant).In(loc).Zone()
	return int64(seconds) * msPerSecond
}

// wallToInstant reads a wall-clock value in zone loc and returns the
// instant. For a wall time that a daylight-saving change skips or
// repeats, time.Date picks one of the candidates, as it documents.
func wallToInstant(wall int64, loc *time.Location) int64 {
	if loc == nil {
		return wall
	}
	t := time.UnixMilli(wall).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(),
		t.Nanosecond(), loc).UnixMilli()
}

// dayString formats a day count since 1970-01-01 as YYYY-MM-DD.
func dayString(day int64) string {
	return time.UnixMilli(day * msPerDay).UTC().Format(time.DateOnly)
}
