package samples

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/timestamps"
)

// TimeBound is one bound of a time query, resolved once for several
// files that are read as one text: the parts of a log chain
// (ResolveQueries). The line a bound asks for is the first line, in the
// text's order, whose own timestamp is at or after it.
//
// A bound keeps its value in the frame of the file it was resolved for,
// and that frame, rather than a bare instant. A file read in the same
// frame compares its lines with the value as it is, exactly as one file
// holding the whole text would; only a file read in another frame (its
// timestamps carry zones where the first file's do not, or the other
// way round) gets the value through its instant (in). Going through the
// instant every time would move a wall clock that a change to summer
// time skips, and the line found with it.
type TimeBound struct {
	ms    int64
	frame timeFrame
}

// in returns b's value in frame f, the frame a file's lines are read in.
func (b TimeBound) in(f timeFrame) int64 {
	if sameFrame(b.frame, f) {
		return b.ms
	}
	// InstantOf returns the instant even when it falls outside the years
	// 1 to 9999; the bound only orders lines, so that value serves.
	instant, _ := timestamps.InstantOf(b.ms, b.frame.hasZone, b.frame.zone.Location)
	return valueInFrame(instant, f)
}

// ReachedBy reports whether a line whose own timestamp is the UTC
// instant `instant` is at or after b, compared in b's frame: whether a
// file whose highest timestamp is `instant` can hold b's line.
func (b TimeBound) ReachedBy(instant int64) bool {
	return valueInFrame(instant, b.frame) >= b.ms
}

// sameFrame reports whether a and b read values the same way: both as
// instants, or both as wall clocks in the same zone.
func sameFrame(a, b timeFrame) bool {
	return a.hasZone == b.hasZone && (a.hasZone || a.zone.Name == b.zone.Name)
}

// valueInFrame is the value the UTC instant `instant` has in frame f:
// the instant itself in a frame of instants, the wall clock it reads in
// f's zone in a frame of wall clocks (no zone is UTC).
func valueInFrame(instant int64, f timeFrame) int64 {
	if f.hasZone || f.zone.Location == nil {
		return instant
	}
	// Zone returns the offset east of UTC in seconds at that instant,
	// summer time included.
	_, offset := time.UnixMilli(instant).In(f.zone.Location).Zone()
	return instant + int64(offset)*msPerSecond
}

// msPerSecond converts a zone offset in seconds to milliseconds.
const msPerSecond = 1000

// ResolvedQuery is one time query resolved by ResolveQueries.
type ResolvedQuery struct {
	// Value is the query as the caller gave it.
	Value string
	// Range is true for a range query, even one with an open end.
	Range bool
	// Start is the bound of a single query's line, or of a range's first
	// line; nil for a range open at the start, whose first line is the
	// text's first.
	Start *TimeBound
	// After is, for a range closed at the end, the bound of the first
	// line later than the end: the range's last line is the line before
	// the one After finds, or the text's last when none does. Nil for a
	// single query and for a range open at the end.
	After *TimeBound
}

// QueryScope is what ResolveQueries takes from the whole text rather
// than from its first file: the span a time of day without a date takes
// its date from.
type QueryScope struct {
	// FirstMs and LastMs are the text's first and last own timestamps,
	// as UTC instants (a log chain's first_ms and last_ms). When FirstMs
	// is nil the text has no span; a nil LastMs is read from Last when a
	// query needs it.
	FirstMs, LastMs *int64
	// Last is the file that holds the text's last timestamped line: the
	// last part of a log chain that holds lines.
	Last Request
}

// ResolveQueries parses and resolves time queries once for several
// files read as one text, the parts of a log chain, the way a timestamps
// request resolves them for one file (resolveTimestamps).
//
// req is the text's first file with timestamps. It gives what one file
// holding the whole text takes from its head: the format a timestamp
// copied from a line is parsed in, the frame of the values, and the zone
// offset of the first timestamp, at which a query without a zone is
// read in a text whose timestamps carry zones (unless RX_QUERY_TZ names
// a zone). A time of day without a date takes its date from scope, the
// whole text's first and last timestamps, by timestamps.Resolve's rule:
// the one day both fall on, read where the query is read; a text of two
// days refuses it.
//
// It returns one ResolvedQuery per value, in order. The errors are
// those of a time request to Resolve: ErrTooManyTimestamps,
// ErrNoTimeFormat, an invalid query (timestamps.ErrInvalidQuery), and a
// failure to read the file.
func ResolveQueries(ctx context.Context, req Request, values []string, scope QueryScope) ([]ResolvedQuery, error) {
	if len(values) > MaxTimestampValues {
		return nil, fmt.Errorf("%w: %d given, at most %d per request", ErrTooManyTimestamps, len(values), MaxTimestampValues)
	}
	req, kind, err := prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	times, err := timesOf(req, kind)
	if err != nil {
		return nil, err
	}
	if times == nil {
		return nil, fmt.Errorf("%w of %s", ErrNoTimeFormat, req.Path)
	}
	queries := make([]timestamps.Query, len(values))
	for i, value := range values {
		if queries[i], err = timestamps.ParseQuery(value, times.parser); err != nil {
			return nil, err
		}
	}
	c, err := times.textContext(req, kind, queries, scope)
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedQuery, len(queries))
	for i, q := range queries {
		r, err := timestamps.Resolve(q, c)
		if err != nil {
			return nil, err
		}
		out[i] = times.resolvedQuery(values[i], r)
	}
	return out, nil
}

// resolvedQuery turns r, a query resolved in t's frame, into the bounds
// a reader of several files searches for.
func (t *fileTimes) resolvedQuery(value string, r timestamps.Resolved) ResolvedQuery {
	out := ResolvedQuery{Value: value, Range: r.Range}
	if !r.Start.Open {
		out.Start = &TimeBound{ms: r.Start.Ms, frame: t.frame}
	}
	// The first line later than T2 is at T2+1 or later, at millisecond
	// precision (searchBoundsOf). Resolve refuses a T2 at the end of the
	// value range, so the sum does not overflow.
	if r.Range && !r.End.Open {
		out.After = &TimeBound{ms: r.End.Ms + 1, frame: t.frame}
	}
	return out
}

// textContext gathers what timestamps.Resolve needs for queries on a
// text of several files whose first file is t's: the zones, the first
// file's first zone offset when a query needs it, and the span of the
// whole text from scope when a query needs it.
func (t *fileTimes) textContext(req Request, kind filekind.Kind, queries []timestamps.Query, scope QueryScope) (timestamps.ResolveContext, error) {
	c, needsSpan, needsFirst := t.baseContext(queries)
	if needsFirst {
		offset, err := t.firstOffset(req, kind)
		if err != nil {
			return c, err
		}
		c.FirstOffsetMinutes = offset
	}
	if !needsSpan || scope.FirstMs == nil {
		return c, nil
	}
	last := scope.LastMs
	if last == nil {
		var err error
		if last, err = lastInstantOf(req.context(), scope.Last); err != nil {
			return c, err
		}
	}
	if last != nil {
		c.FirstMs, c.LastMs = valueInFrame(*scope.FirstMs, t.frame), valueInFrame(*last, t.frame)
		c.HasSpan = true
	}
	return c, nil
}

// baseContext is the part of a ResolveContext that the zones give, and
// whether resolving queries needs the text's span (a time of day) and
// the first timestamp's zone offset (a query without a zone, in a text
// whose timestamps carry zones, with RX_QUERY_TZ unset).
func (t *fileTimes) baseContext(queries []timestamps.Query) (c timestamps.ResolveContext, needsSpan, needsFirst bool) {
	queryZone := config.QueryTZ()
	c = timestamps.ResolveContext{HasZone: t.frame.hasZone, LogZone: t.frame.zone.Location, QueryZone: queryZone.Location}
	for _, q := range queries {
		needsSpan = needsSpan || q.NeedsSpan()
		// A zoned file reads a query without a zone at its first
		// timestamp's offset, unless RX_QUERY_TZ names a zone.
		needsFirst = needsFirst || (c.HasZone && c.QueryZone == nil && hasWallEndpoint(q))
	}
	return c, needsSpan, needsFirst
}

// firstOffset is the zone offset, in minutes, written with the first
// timestamp of the file: from its index when it has one, else from a
// search for its first timestamped line. 0 when it has none.
func (t *fileTimes) firstOffset(req Request, kind filekind.Kind) (int, error) {
	if t.section != nil {
		if t.section.FirstZoneOffsetMinutes != nil {
			return *t.section.FirstZoneOffsetMinutes, nil
		}
		return 0, nil
	}
	found, err := t.searchBounds(req.context(), textSourceFor(req, kind), []int64{math.MinInt64})
	if err != nil {
		return 0, err
	}
	first := found[math.MinInt64]
	if first.line < 0 {
		return 0, nil
	}
	return first.stamp.OffsetMinutes, nil
}

// lastInstantOf returns the own timestamp of the last timestamped line
// of req's file as a UTC instant, read as a samples request reads the
// file: from its index when one can give it, else read back from the end
// of its text (lastStamp). Nil when the file has no timestamped line, or
// when the value falls outside the years 1 to 9999.
func lastInstantOf(ctx context.Context, req Request) (*int64, error) {
	req, kind, err := prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	times, err := timesOf(req, kind)
	if err != nil || times == nil {
		return nil, err
	}
	if times.section != nil {
		last := times.section.Last
		if last == nil {
			return nil, nil
		}
		return instantOf(frameValueAt(times.segments, last.Line, last.Ms), times.frame), nil
	}
	found, err := times.searchBounds(req.context(), textSourceFor(req, kind), []int64{math.MinInt64})
	if err != nil || found[math.MinInt64].line < 0 {
		return nil, err
	}
	stamp, err := times.lastStamp(req, kind)
	if err != nil {
		return nil, err
	}
	return instantOf(stamp.Ms, times.frame), nil
}

// boundsOf resolves req.TimeBounds for the values of req.Timestamps into
// the frame t reads the file in: each value a single query whose line is
// the first at or after its bound.
func (t *fileTimes) boundsOf(req Request) ([]timestamps.Resolved, error) {
	out := make([]timestamps.Resolved, len(req.Timestamps))
	for i, value := range req.Timestamps {
		bound, ok := req.TimeBounds[value]
		if !ok {
			return nil, fmt.Errorf("samples of %s: no resolved bound for the time query %q", req.Path, value)
		}
		out[i] = timestamps.Resolved{Start: timestamps.Bound{Ms: bound.in(t.frame)}}
	}
	return out, nil
}
