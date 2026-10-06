package output

import (
	"testing"
	"time"
)

// An instant is written the way the file's example writes its first
// timestamp, in the zone given.
func TestFileTime_FollowsTheExample(t *testing.T) {
	instant := time.Date(2025, 12, 10, 7, 0, 4, 574e6, time.UTC).UnixMilli()
	plus2 := time.FixedZone("+02:00", 2*3600)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	cases := []struct {
		name  string
		style FileTimeStyle
		loc   *time.Location
		want  string
	}{
		{"iso with dot millis", FileTimeStyle{Family: "iso", Example: "2025-12-10 07:00:04.574"}, time.UTC, "2025-12-10 07:00:04.574"},
		{"iso read in a zone", FileTimeStyle{Family: "iso", Example: "2025-12-10 07:00:04.574"}, tokyo, "2025-12-10 16:00:04.574"},
		{"iso with comma millis", FileTimeStyle{Family: "iso", Example: "2025-12-10 16:18:53,741"}, time.UTC, "2025-12-10 07:00:04,574"},
		{"iso with colon millis", FileTimeStyle{Family: "iso", Example: "2025-2-15 18:16:22:5"}, time.UTC, "2025-12-10 07:00:04:574"},
		{"iso without a fraction", FileTimeStyle{Family: "iso", Example: "2025-12-10 07:49:50 UTC"}, time.UTC, "2025-12-10 07:00:04 UTC"},
		{"iso with T and a numeric zone", FileTimeStyle{Family: "iso", Example: "2026-10-06T12:34:56.123+02:00"}, plus2, "2025-12-10T09:00:04.574+02:00"},
		{"iso with Z and microseconds", FileTimeStyle{Family: "iso", Example: "2026-10-06T12:34:56.123456Z"}, time.UTC, "2025-12-10T07:00:04.574000Z"},
		{"iso with slashes", FileTimeStyle{Family: "iso", Example: "2026/10/06 12:34:56"}, time.UTC, "2025/12/10 07:00:04"},
		{"syslog", FileTimeStyle{Family: "syslog", Example: "Dec 10 07:00:12.156"}, time.UTC, "Dec 10 07:00:04.574"},
		{"clf", FileTimeStyle{Family: "clf", Example: "[06/Oct/2026:12:34:56 +0000]"}, time.UTC, "[10/Dec/2025:07:00:04 +0000]"},
		{"ctime", FileTimeStyle{Family: "ctime", Example: "Tue Oct  6 12:34:56 2026"}, time.UTC, "Wed Dec 10 07:00:04 2025"},
		{"slash month first, 12-hour", FileTimeStyle{Family: "slash", Example: "10/06/2026 12:34:56 PM"}, time.UTC, "12/10/2025 07:00:04 AM"},
		{"slash day first", FileTimeStyle{Family: "slash", DayFirst: true, Example: "13/10/2026 14:34:56"}, time.UTC, "10/12/2025 07:00:04"},
		{"dotted", FileTimeStyle{Family: "dotted", Example: "06.10.2026 12:34:56,789"}, time.UTC, "10.12.2025 07:00:04,574"},
		{"epoch", FileTimeStyle{Family: "epoch", Example: "1696600000.123"}, time.UTC, "2025-12-10T07:00:04.574Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FileTime(instant, tc.style, tc.loc); got != tc.want {
				t.Fatalf("FileTime = %q; want %q", got, tc.want)
			}
		})
	}
}

// FixedOffsetZone reads ±HH:MM and refuses anything else.
func TestFixedOffsetZone(t *testing.T) {
	for text, wantSeconds := range map[string]int{"+02:00": 7200, "-07:00": -25200, "+00:00": 0, "+05:30": 19800} {
		loc, ok := FixedOffsetZone(text)
		if !ok {
			t.Fatalf("%q refused", text)
		}
		if _, offset := time.Unix(0, 0).In(loc).Zone(); offset != wantSeconds {
			t.Errorf("%q: offset %d, want %d", text, offset, wantSeconds)
		}
	}
	for _, text := range []string{"UTC", "+2:00", "+02:60", "02:00", "Asia/Tokyo"} {
		if _, ok := FixedOffsetZone(text); ok {
			t.Errorf("%q accepted", text)
		}
	}
}
