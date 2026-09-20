package timestamps

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// utcMs is the oracle for every expected value: the standard library's
// calendar, independent of the package's own arithmetic.
func utcMs(y int, mo time.Month, d, h, mi, s, ms int) int64 {
	return time.Date(y, mo, d, h, mi, s, ms*int(time.Millisecond), time.UTC).UnixMilli()
}

func boolPtr(b bool) *bool { return &b }

var (
	isoAnchored     = Format{Family: FamilyISO, Anchored: true}
	isoZoned        = Format{Family: FamilyISO, Anchored: true, HasZone: true}
	isoWindowed     = Format{Family: FamilyISO}
	clfWindowed     = Format{Family: FamilyCLF, HasZone: true}
	ctimeAnchored   = Format{Family: FamilyCtime, Anchored: true}
	ctimeWindowed   = Format{Family: FamilyCtime}
	syslogAnchored  = Format{Family: FamilySyslog, Anchored: true}
	slashMonthFirst = Format{Family: FamilySlash, Anchored: true, DayFirst: boolPtr(false)}
	slashDayFirst   = Format{Family: FamilySlash, Anchored: true, DayFirst: boolPtr(true)}
	dottedAnchored  = Format{Family: FamilyDotted, Anchored: true}
	epochAnchored   = Format{Family: FamilyEpoch, Anchored: true, HasZone: true}
	epochWindowed   = Format{Family: FamilyEpoch, HasZone: true}
)

// mtime2025 is the mtime of the playground's year-less core.log.
var mtime2025 = time.Date(2025, 12, 27, 10, 0, 0, 0, time.UTC)

func mustParser(t testing.TB, f Format, mtime time.Time) *Parser {
	t.Helper()
	p, err := NewParser(f, mtime.UnixNano())
	if err != nil {
		t.Fatalf("NewParser(%+v): %v", f, err)
	}
	return p
}

type ownCase struct {
	name   string
	format Format
	line   string
	want   Stamp
	ok     bool
}

func wall(ms int64) Stamp { return Stamp{Ms: ms} }

func zoned(ms int64, offsetMinutes int) Stamp {
	return Stamp{Ms: ms, Zoned: true, OffsetMinutes: offsetMinutes}
}

func runOwnCases(t *testing.T, cases []ownCase, mtime time.Time) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mustParser(t, tc.format, mtime)
			got, ok := p.Own([]byte(tc.line))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("Own(%q) = %+v, %t; want %+v, %t", tc.line, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestOwn_ISO(t *testing.T) {
	runOwnCases(t, []ownCase{
		{"one-digit fields and colon millis", isoAnchored, "2025-2-15 18:16:22:397 (scheduler.cc:120): Setting", wall(utcMs(2025, 2, 15, 18, 16, 22, 397)), true},
		{"one-digit second and colon millis", isoAnchored, "2025-2-15 12:34:5:123 x", wall(utcMs(2025, 2, 15, 12, 34, 5, 123)), true},
		{"dot millis", isoAnchored, "2025-12-10 12:34:56.123 [main] INFO", wall(utcMs(2025, 12, 10, 12, 34, 56, 123)), true},
		{"comma millis", isoAnchored, "2025-12-10 12:34:56,123 [main] [WARN]", wall(utcMs(2025, 12, 10, 12, 34, 56, 123)), true},
		{"UTC word converts", isoZoned, "2025-12-10 07:49:50 UTC [123]: LOG", zoned(utcMs(2025, 12, 10, 7, 49, 50, 0), 0), true},
		{"GMT word converts", isoZoned, "2025-12-10 07:49:50 GMT x", zoned(utcMs(2025, 12, 10, 7, 49, 50, 0), 0), true},
		{"MST word stays zone-less", isoAnchored, "2025-12-10 07:00:30 MST [4242]: [12-1]", wall(utcMs(2025, 12, 10, 7, 0, 30, 0)), true},
		{"CEST word stays zone-less", isoAnchored, "2025-12-10 07:00:30 CEST x", wall(utcMs(2025, 12, 10, 7, 0, 30, 0)), true},
		{"RFC 3339 with offset", isoZoned, "2026-10-06T12:34:56.123+02:00 msg", zoned(utcMs(2026, 10, 6, 10, 34, 56, 123), 120), true},
		{"Z", isoZoned, "2026-10-06T12:34:56Z msg", zoned(utcMs(2026, 10, 6, 12, 34, 56, 0), 0), true},
		{"offset without colon", isoZoned, "2026-10-06T12:34:56+0200 msg", zoned(utcMs(2026, 10, 6, 10, 34, 56, 0), 120), true},
		{"offset hours only", isoZoned, "2026-10-06T12:34:56+02 msg", zoned(utcMs(2026, 10, 6, 10, 34, 56, 0), 120), true},
		{"negative half-hour offset", isoZoned, "2026-10-06T12:34:56-05:30 msg", zoned(utcMs(2026, 10, 6, 18, 4, 56, 0), -330), true},
		{"space then numeric offset", isoZoned, "2026-10-06 12:34:56 +0200 msg", zoned(utcMs(2026, 10, 6, 10, 34, 56, 0), 120), true},
		{"slashes in the date", isoAnchored, "2026/10/06 12:34:56 x", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"one fraction digit", isoAnchored, "2026-10-06 12:34:56.5 x", wall(utcMs(2026, 10, 6, 12, 34, 56, 500)), true},
		{"nine fraction digits truncate", isoAnchored, "2026-10-06 12:34:56.123456789 x", wall(utcMs(2026, 10, 6, 12, 34, 56, 123)), true},
		{"timestamp fills the line", isoAnchored, "2026-10-06 12:34:56", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"ends with CRLF", isoAnchored, "2026-10-06 12:34:56\r\n", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"colon after the seconds then text", isoAnchored, "2026-10-06 12:34:56: msg", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"leap day in a leap year", isoAnchored, "2024-02-29 00:00:00 x", wall(utcMs(2024, 2, 29, 0, 0, 0, 0)), true},
		{"leap day in a common year", isoAnchored, "2023-02-29 00:00:00 x", Stamp{}, false},
		{"April 31", isoAnchored, "2026-04-31 00:00:00 x", Stamp{}, false},
		{"month 13", isoAnchored, "2026-13-01 00:00:00 x", Stamp{}, false},
		{"day 32", isoAnchored, "2026-10-32 00:00:00 x", Stamp{}, false},
		{"hour 24", isoAnchored, "2026-10-06 24:00:00 x", Stamp{}, false},
		{"minute 60", isoAnchored, "2026-10-06 12:60:00 x", Stamp{}, false},
		{"second 60", isoAnchored, "2026-10-06 12:34:60 x", Stamp{}, false},
		{"unpadded colon millis, two digits", isoAnchored, "2025-2-15 18:16:41:34 (webrtc_voice_engine.cc:1)", wall(utcMs(2025, 2, 15, 18, 16, 41, 34)), true},
		{"unpadded colon millis, one digit", isoAnchored, "2025-2-15 18:16:41:7 (x.cc:1)", wall(utcMs(2025, 2, 15, 18, 16, 41, 7)), true},
		{"colon millis with 4 digits", isoAnchored, "2026-10-06 12:34:56:1234 x", Stamp{}, false},
		{"ten fraction digits", isoAnchored, "2026-10-06 12:34:56.1234567890 x", Stamp{}, false},
		{"three-digit second", isoAnchored, "2026-10-06 12:34:567 x", Stamp{}, false},
		{"no seconds", isoAnchored, "2026-10-06 12:34 x", Stamp{}, false},
		{"mixed date separators", isoAnchored, "2026-10/06 12:34:56 x", Stamp{}, false},
		{"five-digit year", isoAnchored, "12026-10-06 12:34:56 x", Stamp{}, false},
		{"offset hour 24", isoAnchored, "2026-10-06T12:34:56+24:00 x", Stamp{}, false},
	}, mtime2025)
}

// A file's values are in one frame. In a file whose format has zones,
// every line is a UTC instant, and a line without a zone is read as
// UTC. In a file whose format has none, every line is a wall-clock
// reading, and a line that does carry a zone keeps the wall clock it
// shows: a host on -07:00 that writes zone-less lines and, from another
// component, `2025-12-10T07:00:07.953-0700` lines writes one clock, and
// reading the zoned ones as instants would put them seven hours after
// their neighbors. Zoned and OffsetMinutes still say what was written.
func TestOwn_OneFramePerFile(t *testing.T) {
	gcLine := "[2025-12-10T07:00:07.953-0700][512.004s][info][gc] GC(57) Pause Young"
	runOwnCases(t, []ownCase{
		{"zoned line, zone-less file", isoAnchored, gcLine,
			Stamp{Ms: utcMs(2025, 12, 10, 7, 0, 7, 953), Zoned: true, OffsetMinutes: -420}, true},
		{"zoned line, zoned file", isoZoned, gcLine,
			zoned(utcMs(2025, 12, 10, 14, 0, 7, 953), -420), true},
		{"zone-less line, zoned file", isoZoned, "2025-12-10 07:00:04.574 INFO x",
			wall(utcMs(2025, 12, 10, 7, 0, 4, 574)), true},
		{"UTC line, zone-less file", isoAnchored, "2025-12-10 07:49:50 UTC [123]: LOG",
			zoned(utcMs(2025, 12, 10, 7, 49, 50, 0), 0), true},
		{"numeric zone, windowed zone-less file", isoWindowed, "host app 2026-10-06T12:34:56+02:00 msg",
			Stamp{Ms: utcMs(2026, 10, 6, 12, 34, 56, 0), Zoned: true, OffsetMinutes: 120}, true},
	}, mtime2025)
}

func TestOwn_CLF(t *testing.T) {
	runOwnCases(t, []ownCase{
		{"access log line", clfWindowed, `127.0.0.1 - - [06/Oct/2026:12:34:56 +0000] "GET / HTTP/1.1" 200 512`, zoned(utcMs(2026, 10, 6, 12, 34, 56, 0), 0), true},
		{"lower-case month and negative offset", clfWindowed, `10.0.0.1 - bob [10/oct/2026:12:34:56 -0700] "GET /"`, zoned(utcMs(2026, 10, 10, 19, 34, 56, 0), -420), true},
		{"at column 0", Format{Family: FamilyCLF, Anchored: true, HasZone: true}, `[06/Oct/2026:12:34:56 +0000] x`, zoned(utcMs(2026, 10, 6, 12, 34, 56, 0), 0), true},
		{"unknown month", clfWindowed, `h - - [06/Foo/2026:12:34:56 +0000] x`, Stamp{}, false},
		{"missing bracket", clfWindowed, `h - - [06/Oct/2026:12:34:56 +0000 x`, Stamp{}, false},
	}, mtime2025)
}

func TestOwn_Ctime(t *testing.T) {
	runOwnCases(t, []ownCase{
		{"microseconds", ctimeAnchored, "Tue Oct 06 12:34:56.123456 2026 x", wall(utcMs(2026, 10, 6, 12, 34, 56, 123)), true},
		{"space-padded day", ctimeAnchored, "Tue Oct  6 12:34:56 2026 x", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"any case", ctimeAnchored, "tUE oCT 06 12:34:56 2026", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"Apache error log after a bracket", ctimeAnchored, "[Tue Oct 06 12:34:56.123456 2026] [core:error] [pid 1]", wall(utcMs(2026, 10, 6, 12, 34, 56, 123)), true},
		{"windowed after a prefix", ctimeWindowed, "pid=1 Tue Oct 06 12:34:56 2026 x", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"unknown weekday", ctimeAnchored, "Xyz Oct 06 12:34:56 2026", Stamp{}, false},
		{"no year", ctimeAnchored, "Tue Oct 06 12:34:56 x", Stamp{}, false},
	}, mtime2025)
}

func TestOwn_Syslog(t *testing.T) {
	runOwnCases(t, []ownCase{
		{"core.log shape", syslogAnchored, "Dec 10 07:49:50.123 7A3F09C21B5E I      job.scheduler.retry", wall(utcMs(2025, 12, 10, 7, 49, 50, 123)), true},
		{"space-padded day", syslogAnchored, "Oct  6 12:34:56 host sshd[1]: x", wall(utcMs(2025, 10, 6, 12, 34, 56, 0)), true},
		{"upper-case month", syslogAnchored, "DEC 10 07:49:50 x", wall(utcMs(2025, 12, 10, 7, 49, 50, 0)), true},
		{"full month name", syslogAnchored, "December 10 07:49:50 x", Stamp{}, false},
		{"February 30", syslogAnchored, "Feb 30 07:49:50 x", Stamp{}, false},
	}, mtime2025)
}

func TestOwn_SyslogYearInference(t *testing.T) {
	mtimeJan2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mtimeJune := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		line  string
		mtime time.Time
		want  int64
	}{
		{"December line in a January file is last year", "Dec 31 23:59:59 x", mtimeJan2, utcMs(2025, 12, 31, 23, 59, 59, 0)},
		{"New Year line in a January file is this year", "Jan  1 00:00:01 x", mtimeJan2, utcMs(2026, 1, 1, 0, 0, 1, 0)},
		{"within one day after the mtime is this year", "Jan  2 23:00:00 x", mtimeJan2, utcMs(2026, 1, 2, 23, 0, 0, 0)},
		{"more than one day after the mtime is last year", "Jan  3 00:00:01 x", mtimeJan2, utcMs(2025, 1, 3, 0, 0, 1, 0)},
		{"February 29 takes the latest leap year", "Feb 29 10:00:00 x", mtimeJune, utcMs(2024, 2, 29, 10, 0, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mustParser(t, syslogAnchored, tc.mtime)
			got, ok := p.Own([]byte(tc.line))
			if !ok || got != wall(tc.want) {
				t.Fatalf("Own(%q) = %+v, %t; want %d", tc.line, got, ok, tc.want)
			}
		})
	}
}

func TestOwn_Slash(t *testing.T) {
	runOwnCases(t, []ownCase{
		{"month first, 12 PM is noon", slashMonthFirst, "10/06/2026 12:34:56 PM x", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"12 AM is midnight", slashMonthFirst, "10/06/2026 12:00:00 AM x", wall(utcMs(2026, 10, 6, 0, 0, 0, 0)), true},
		{"1 PM is 13", slashMonthFirst, "10/06/2026 1:05:00 PM x", wall(utcMs(2026, 10, 6, 13, 5, 0, 0)), true},
		{"lower-case pm", slashMonthFirst, "10/06/2026 1:05:00 pm x", wall(utcMs(2026, 10, 6, 13, 5, 0, 0)), true},
		{"11 AM stays", slashMonthFirst, "10/06/2026 11:05:00 AM", wall(utcMs(2026, 10, 6, 11, 5, 0, 0)), true},
		{"hour 13 with PM", slashMonthFirst, "10/06/2026 13:05:00 PM x", Stamp{}, false},
		{"hour 0 with AM", slashMonthFirst, "10/06/2026 0:05:00 AM x", Stamp{}, false},
		{"day first", slashDayFirst, "06/10/2026 14:34:56 x", wall(utcMs(2026, 10, 6, 14, 34, 56, 0)), true},
		{"two-digit year 69 is 2069", slashDayFirst, "06/10/69 14:34:56 x", wall(utcMs(2069, 10, 6, 14, 34, 56, 0)), true},
		{"two-digit year 70 is 1970", slashDayFirst, "06/10/70 14:34:56 x", wall(utcMs(1970, 10, 6, 14, 34, 56, 0)), true},
		{"fraction", slashDayFirst, "06/10/2026 14:34:56.250 x", wall(utcMs(2026, 10, 6, 14, 34, 56, 250)), true},
		{"day-first lock refuses month 13", slashDayFirst, "10/13/2026 14:34:56 x", Stamp{}, false},
		{"month-first lock refuses month 13", slashMonthFirst, "13/10/2026 14:34:56 x", Stamp{}, false},
		{"three-digit year", slashDayFirst, "06/10/202 14:34:56 x", Stamp{}, false},
	}, mtime2025)
}

func TestOwn_Dotted(t *testing.T) {
	runOwnCases(t, []ownCase{
		{"day first", dottedAnchored, "06.10.2026 12:34:56 x", wall(utcMs(2026, 10, 6, 12, 34, 56, 0)), true},
		{"comma fraction", dottedAnchored, "06.10.2026 12:34:56,5 x", wall(utcMs(2026, 10, 6, 12, 34, 56, 500)), true},
		{"day 32", dottedAnchored, "32.10.2026 12:34:56 x", Stamp{}, false},
		{"month 13", dottedAnchored, "06.13.2026 12:34:56 x", Stamp{}, false},
	}, mtime2025)
}

func TestOwn_Epoch(t *testing.T) {
	runOwnCases(t, []ownCase{
		{"seconds", epochAnchored, "1696600000 msg", zoned(1696600000000, 0), true},
		{"seconds with fraction", epochAnchored, "1696600000.123 msg", zoned(1696600000123, 0), true},
		{"seconds with one fraction digit", epochAnchored, "1696600000.5 msg", zoned(1696600000500, 0), true},
		{"milliseconds", epochAnchored, "1696600000123 msg", zoned(1696600000123, 0), true},
		{"after a bracket at column 0", epochAnchored, "[1696600000] msg", zoned(1696600000000, 0), true},
		{"windowed after a bracket", epochWindowed, "2025-12-10 07:00:04.574 [1765375204574] [Worker-3] INFO", zoned(1765375204574, 0), true},
		{"windowed needs a bracket", epochWindowed, "id=1696600000 x", Stamp{}, false},
		{"first second of 2000", epochAnchored, "946684800 x", Stamp{}, false},
		{"before 2000", epochAnchored, "0946684799 x", Stamp{}, false},
		{"2000 exactly", epochAnchored, "0946684800 x", zoned(946684800000, 0), true},
		{"last second before 2100", epochAnchored, "4102444799 x", zoned(4102444799000, 0), true},
		{"2100 exactly", epochAnchored, "4102444800 x", Stamp{}, false},
		{"milliseconds after 2100", epochAnchored, "4102444800000 x", Stamp{}, false},
		{"eleven digits", epochAnchored, "16966000001 x", Stamp{}, false},
		{"twelve digits", epochAnchored, "169660000012 x", Stamp{}, false},
		{"fourteen digits", epochAnchored, "16966000001234 x", Stamp{}, false},
	}, mtime2025)
}

// TestOwn_AnchoredAndWindowed: an anchored file reads column 0, or
// column 1 after a `[`, and nothing else; a windowed file takes the first
// timestamp in the window.
func TestOwn_AnchoredAndWindowed(t *testing.T) {
	ts := wall(utcMs(2026, 10, 6, 12, 34, 56, 0))
	runOwnCases(t, []ownCase{
		{"anchored ignores column 5", isoAnchored, "abcd 2026-10-06 12:34:56 x", Stamp{}, false},
		{"windowed finds column 5", isoWindowed, "abcd 2026-10-06 12:34:56 x", ts, true},
		{"anchored reads column 1 after a bracket", isoAnchored, "[2026-10-06 12:34:56] INFO", ts, true},
		{"anchored ignores column 1 after a space", isoAnchored, " 2026-10-06 12:34:56 x", Stamp{}, false},
		{"anchored ignores column 2 after a bracket", isoAnchored, "[ 2026-10-06 12:34:56] x", Stamp{}, false},
		{"anchored ignores a tab-led continuation", isoAnchored, "\t2026-10-06 12:34:56 x", Stamp{}, false},
		{"windowed skips a match inside a word", isoWindowed, "x2026-10-06 12:34:56 y", Stamp{}, false},
		{"windowed skips an invalid date for a later one", isoWindowed, "a 2026-13-06 12:34:56 b 2026-10-06 12:34:56", ts, true},
		{"windowed takes the first match, even in a message", isoWindowed,
			`{"msg":"x 2099-01-01 00:00:00","ts":"2026-10-06T12:34:56Z"}`, wall(utcMs(2099, 1, 1, 0, 0, 0, 0)), true},
		{"windowed slash does not start inside a year", Format{Family: FamilySlash, DayFirst: boolPtr(true)}, "2026/10/06 12:34:56 x", Stamp{}, false},
	}, mtime2025)
}

// TestOwn_Window: a timestamp must end within the first maxEnd bytes, and
// one cut by the window never reads as a shorter timestamp.
func TestOwn_Window(t *testing.T) {
	stamp := "2026-10-06 12:34:56"
	p := mustParser(t, isoWindowed, mtime2025)
	for end := maxEnd - 2; end <= WindowBytes+2; end++ {
		line := strings.Repeat("x", end-len(stamp)-1) + " " + stamp + " tail"
		_, ok := p.Own([]byte(line))
		if want := end <= maxEnd; ok != want {
			t.Errorf("timestamp ending at byte %d: found = %t; want %t", end, ok, want)
		}
	}
}

// TestOwn_YearRange: a timestamp counts only when it lies in the years 1
// to 9999 both as the instant its zone names and as the value the file
// stores, so every value can be rendered as an RFC 3339 time.
func TestOwn_YearRange(t *testing.T) {
	runOwnCases(t, []ownCase{
		{"first millisecond of year 1", isoAnchored, "0001-01-01 00:00:00.000 x", wall(utcMs(1, 1, 1, 0, 0, 0, 0)), true},
		{"last millisecond of year 9999", isoAnchored, "9999-12-31 23:59:59.999 x", wall(utcMs(9999, 12, 31, 23, 59, 59, 999)), true},
		{"year 0", isoAnchored, "0000-01-01 00:00:00 x", Stamp{}, false},
		{"a zone pushes the instant into year 10000", isoZoned, "9999-12-31 23:59:59.999-23:59 x", Stamp{}, false},
		{"a zone pushes the instant into year 0", isoZoned, "0001-01-01 00:00:00+23:59 x", Stamp{}, false},
		{"year 0 as written, year 1 as an instant", isoAnchored, "0000-12-31T23:00:00-05:00 x", Stamp{}, false},
		{"year 9999 as written, year 10000 as an instant", isoAnchored, "9999-12-31T23:59:59.999-23:59 x", Stamp{}, false},
	}, mtime2025)
}

// TestValueRange_MatchesTimeDate pins the value range to the standard
// library's calendar.
func TestValueRange_MatchesTimeDate(t *testing.T) {
	if want := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(); minValueMs != want {
		t.Errorf("minValueMs = %d; want %d", int64(minValueMs), want)
	}
	if want := time.Date(9999, 12, 31, 23, 59, 59, 999e6, time.UTC).UnixMilli(); maxValueMs != want {
		t.Errorf("maxValueMs = %d; want %d", int64(maxValueMs), want)
	}
}

func TestNewParser_RejectsInvalidFormat(t *testing.T) {
	for _, f := range []Format{
		{Family: "rfc2822"},
		{Family: FamilySlash},
		{Family: FamilyISO, DayFirst: boolPtr(true)},
	} {
		if _, err := NewParser(f, 0); !errors.Is(err, ErrInvalidFormat) {
			t.Errorf("NewParser(%+v) error = %v; want ErrInvalidFormat", f, err)
		}
	}
}

func TestFormat_YearFromMtime(t *testing.T) {
	for _, fam := range families {
		f := Format{Family: fam.name}
		if got, want := f.YearFromMtime(), fam.name == FamilySyslog; got != want {
			t.Errorf("%s: YearFromMtime() = %t; want %t", fam.name, got, want)
		}
	}
}

// TestCivilMs_MatchesTimeDate compares the integer calendar with the
// standard library over random dates from 1600 to 2400.
func TestCivilMs_MatchesTimeDate(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		y := 1600 + rng.Intn(800)
		m := 1 + rng.Intn(12)
		d := 1 + rng.Intn(daysIn(y, m))
		want := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).UnixMilli()
		if got := civilMs(y, m, d); got != want {
			t.Fatalf("civilMs(%d, %d, %d) = %d; want %d", y, m, d, got, want)
		}
	}
}
