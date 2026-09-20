package timestamps

// Calendar arithmetic on plain integers. The per-line path cannot use
// time.Date: it is correct but slower than these few integer operations,
// and the package needs the same answer from both anyway (the tests
// compare them).

const (
	msPerSecond = int64(1000)
	msPerMinute = 60 * msPerSecond
	msPerHour   = 60 * msPerMinute
	msPerDay    = 24 * msPerHour
)

// maxYearsBack bounds the search for the year of a year-less timestamp.
// Eight years covers the longest gap between two February 29ths (1896 to
// 1904, 2096 to 2104).
const maxYearsBack = 8

// fields is one timestamp as written, before it is checked or
// converted. Every matcher fills one; finish turns it into a Stamp.
type fields struct {
	year, month, day            int
	hour, minute, second, milli int
	// offsetMinutes is the zone offset east of UTC; zoned says a zone
	// was written at all.
	offsetMinutes int
	zoned         bool
	// isEpoch marks an epoch value, which is already a UTC instant in
	// epochMs and skips every calendar check.
	isEpoch bool
	epochMs int64
}

// validClock reports whether the time of day exists. A leap second
// (second 60) is refused: no family below writes one in practice, and
// accepting it would make two lines a second apart read as equal.
func validClock(r fields) bool {
	return r.hour >= 0 && r.hour <= 23 &&
		r.minute >= 0 && r.minute <= 59 &&
		r.second >= 0 && r.second <= 59 &&
		r.milli >= 0 && r.milli <= 999
}

// clockMs is the time of day in milliseconds since midnight.
func clockMs(r fields) int64 {
	return int64(r.hour)*msPerHour + int64(r.minute)*msPerMinute +
		int64(r.second)*msPerSecond + int64(r.milli)
}

// isLeap reports whether year is a Gregorian leap year.
func isLeap(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

// daysPerMonth holds the length of each month in a common year.
var daysPerMonth = [13]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

// daysIn returns the number of days in a month (1–12) of a year.
func daysIn(year, month int) int {
	if month == 2 && isLeap(year) {
		return 29
	}
	return daysPerMonth[month]
}

// civilMs returns midnight UTC of a proleptic Gregorian date as
// milliseconds since the Unix epoch. It is Howard Hinnant's
// days_from_civil: shift the year to start in March so the leap day is
// the last day of the year, then count whole 400-year eras.
func civilMs(year, month, day int) int64 {
	y := int64(year)
	if month <= 2 {
		y--
	}
	era := floorDiv(y, 400)
	yoe := y - era*400 // year of era, 0..399
	m := int64(month)
	var mp int64 // month counted from March, 0..11
	if m > 2 {
		mp = m - 3
	} else {
		mp = m + 9
	}
	doy := (153*mp+2)/5 + int64(day) - 1 // day of the March-based year
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	days := era*146097 + doe - 719468 // 719468 days from 0000-03-01 to 1970-01-01
	return days * msPerDay
}

// inferYear gives a year-less timestamp its year: the latest year, not
// later than the year of the file's mtime, in which the date exists and
// the moment is at most one day after the mtime. A line can never be
// written after its file was last modified; the one day of slack allows
// for the zone the line was written in, which is unknown. So a December
// line in a file last written in January reads as the previous year, and
// February 29 reads as the latest leap year.
//
// The comparison uses the wall-clock value as if it were UTC, the same
// frame the line's value is stored in.
func inferYear(r fields, mtimeMs int64, mtimeYear int) (int64, bool) {
	limit := mtimeMs + msPerDay
	tod := clockMs(r)
	for y := mtimeYear; y >= mtimeYear-maxYearsBack; y-- {
		if r.day > daysIn(y, r.month) {
			continue
		}
		ms := civilMs(y, r.month, r.day) + tod
		if ms <= limit {
			return ms, true
		}
	}
	return 0, false
}

// floorDiv divides rounding toward negative infinity, so a negative
// value (a moment before 1970) lands in the right unit.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
