package output

import (
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ChainTimes writes the times of a log chain's answers for a person: in
// the layout its parts write their timestamps (FileTime), the way
// `rx time-range` writes one file's, when every part with lines writes
// them one way; to the millisecond otherwise, so that the times of parts
// written in several layouts lose no digit.
type ChainTimes struct {
	style FileTimeStyle
	// own says that every part with lines writes the one layout style
	// gives.
	own bool
	loc *time.Location
}

// chainLayoutProbe is the instant two layouts are compared at: a day
// above 12 (day/month order), an hour past noon (12-hour clock), a
// fraction of three different digits and UTC (the form of a zone), so
// two styles that write one of them differently write it differently.
var chainLayoutProbe = time.Date(2026, time.January, 13, 15, 4, 5, 678_000_000, time.UTC).UnixMilli()

// chainMillisecondLayout is how ChainTimes writes a time when the parts
// write theirs in several layouts.
const chainMillisecondLayout = "2006-01-02 15:04:05.000"

// NewChainTimes returns the writer of the times of a chain whose parts
// are parts, in loc. The parts' layouts come from their time_format,
// day_first and example; a part with lines whose layout is not known
// keeps the times to the millisecond.
func NewChainTimes(parts []rxtypes.ChainPart, loc *time.Location) ChainTimes {
	c := ChainTimes{loc: loc}
	seen := false
	for _, p := range parts {
		if (p.LineCount != nil && *p.LineCount == 0) || (p.LineCount == nil && p.Size == 0) {
			continue
		}
		style, ok := partTimeStyle(p)
		if !ok {
			return c
		}
		if !seen {
			c.style, seen = style, true
			continue
		}
		if FileTime(chainLayoutProbe, style, time.UTC) != FileTime(chainLayoutProbe, c.style, time.UTC) {
			return c
		}
	}
	c.own = seen
	return c
}

// partTimeStyle is the layout a part writes its timestamps in, and
// false when it is not known.
func partTimeStyle(p rxtypes.ChainPart) (FileTimeStyle, bool) {
	if p.TimeFormat == nil || p.Example == nil {
		return FileTimeStyle{}, false
	}
	return FileTimeStyle{Family: p.TimeFormat.Format, DayFirst: p.DayFirst != nil && *p.DayFirst, Example: *p.Example}, true
}

// Format writes the UTC instant ms.
func (c ChainTimes) Format(ms int64) string {
	if c.own {
		return FileTime(ms, c.style, c.loc)
	}
	return time.UnixMilli(ms).In(c.loc).Format(chainMillisecondLayout)
}
