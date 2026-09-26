package output

import (
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// chainPart is a part with lines whose timestamps are written as
// example in family.
func chainPart(family, example string, lines int64) rxtypes.ChainPart {
	p := rxtypes.ChainPart{LineCount: &lines, Size: 100}
	if family != "" {
		p.TimeFormat = &rxtypes.SamplesTimeFormat{Format: family}
		p.Example = &example
	}
	return p
}

// The times of a chain are written in the layout its parts write
// theirs when every part with lines writes them one way, and to the
// millisecond otherwise: parts in two layouts, or a part whose layout
// is not known. Empty parts do not count.
func TestChainTimes_FollowTheParts(t *testing.T) {
	at := time.Date(2025, 12, 10, 7, 0, 30, 0, time.UTC).UnixMilli()
	seconds, millis := "2025-12-10 07:00:30", "2025-12-10 07:00:30.000"
	cases := []struct {
		name  string
		parts []rxtypes.ChainPart
		want  string
	}{
		{"one layout, seconds", []rxtypes.ChainPart{chainPart("iso", "2025-12-10 07:00:12", 3), chainPart("iso", "2025-12-10 08:00:00", 2)}, seconds},
		{"an empty part beside", []rxtypes.ChainPart{chainPart("", "", 0), chainPart("iso", "2025-12-10 07:00:12", 3)}, seconds},
		{"one layout, a T and a comma", []rxtypes.ChainPart{chainPart("iso", "2025-12-10T07:00:12,123", 3)}, "2025-12-10T07:00:30,000"},
		{"one layout, syslog", []rxtypes.ChainPart{chainPart("syslog", "Dec 10 07:00:12", 3), chainPart("syslog", "Dec  9 23:59:59", 1)}, "Dec 10 07:00:30"},
		{"two layouts", []rxtypes.ChainPart{chainPart("iso", "2025-12-10 07:00:12", 3), chainPart("iso", "2025-12-10 07:00:12.345", 3)}, millis},
		{"a layout not known", []rxtypes.ChainPart{chainPart("iso", "2025-12-10 07:00:12", 3), chainPart("", "", 3)}, millis},
		{"no part with lines", []rxtypes.ChainPart{chainPart("", "", 0)}, millis},
	}
	for _, tc := range cases {
		if got := NewChainTimes(tc.parts, time.UTC).Format(at); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
