package output

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FileTimeStyle is how a file writes its timestamps, as a time-range
// answer describes it: the family, the day/month order of a slash date,
// and the first timestamp as written, from which FileTime reads the
// options a family leaves open.
type FileTimeStyle struct {
	Family   string
	DayFirst bool
	Example  string
}

// familyLayout is how one family writes a time: Go time layouts before
// and after the place a fraction of a second goes. A time renders as
// t.Format(before) + fraction + t.Format(after) + zone.
type familyLayout struct {
	before, after string
	// fraction is false for a family that never writes one.
	fraction bool
	// zone is true for a family whose zone FileTime writes as the
	// example writes it; clf has its zone in its layout.
	zone bool
}

// familyLayouts is the layout of each family, keyed by the family name
// a time-range answer gives. The options the lines of a file choose are
// applied from the example: `T` or a space and `-` or `/` in an iso
// date, the fraction's separator and digits, a 12-hour clock, and the
// form of a zone.
var familyLayouts = map[string]familyLayout{
	"iso":    {before: "2006-01-02 15:04:05", fraction: true, zone: true},
	"clf":    {before: "[02/Jan/2006:15:04:05 -0700]"},
	"ctime":  {before: "Mon Jan _2 15:04:05", after: " 2006", fraction: true},
	"syslog": {before: "Jan _2 15:04:05", fraction: true},
	"slash":  {before: "01/02/2006 15:04:05", fraction: true},
	"dotted": {before: "02.01.2006 15:04:05", fraction: true},
	// An epoch value is shown as ISO 8601 with milliseconds.
	"epoch": {before: "2006-01-02T15:04:05.000Z07:00"},
}

// FileTime renders instantMs, milliseconds since the Unix epoch, in loc
// the way a file of the given style writes its timestamps, so a person
// can match it with the file's lines: `2025-12-10 07:00:04.574`,
// `Dec 10 07:00:12.156`, `2025-12-10 16:18:53,741`.
//
// It follows the example in the choices above, and not in every detail:
// numbers are always zero-padded, a two-digit year is written with four
// and milliseconds after a `:` with three digits. An unknown family is
// rendered as ISO 8601.
func FileTime(instantMs int64, style FileTimeStyle, loc *time.Location) string {
	layout, ok := familyLayouts[style.Family]
	if !ok {
		layout = familyLayouts["epoch"]
	}
	t := time.UnixMilli(instantMs).In(loc)
	before, after := layout.before, layout.after
	if style.Family == "iso" {
		before = isoDateTimeLayout(style.Example)
	}
	if style.Family == "slash" && style.DayFirst {
		before = "02/01/2006 15:04:05"
	}
	twelveHour := hasMeridiem(style.Example) && style.Family == "slash"
	if twelveHour {
		before = strings.Replace(before, "15:", "03:", 1)
		after += " PM"
	}
	var b strings.Builder
	b.WriteString(t.Format(before))
	if layout.fraction {
		b.WriteString(fractionOf(t, style.Example))
	}
	b.WriteString(t.Format(after))
	if layout.zone {
		b.WriteString(zoneOf(t, style.Example))
	}
	return b.String()
}

// isoDateTimeLayout is the date and time of an iso example: `/` or `-`
// between the date's numbers, `T` or a space before the time.
func isoDateTimeLayout(example string) string {
	date := "2006-01-02"
	if len(example) > 4 && example[4] == '/' {
		date = "2006/01/02"
	}
	separator := " "
	if hour := hourStart(example); hour > 0 && example[hour-1] == 'T' {
		separator = "T"
	}
	return date + separator + "15:04:05"
}

// hourStart is where the hour of an example starts: the digits before
// its first `:`. It is -1 for an example without a `:`.
func hourStart(example string) int {
	colon := strings.IndexByte(example, ':')
	if colon < 0 {
		return -1
	}
	start := colon
	for start > 0 && isDigit(example[start-1]) {
		start--
	}
	return start
}

// hasMeridiem reports whether the example ends with AM or PM.
func hasMeridiem(example string) bool {
	upper := strings.ToUpper(example)
	return strings.HasSuffix(upper, " AM") || strings.HasSuffix(upper, " PM")
}

// fractionOf renders the fraction of a second of t as the example
// writes it: its separator and its number of digits, or nothing when
// the example has none. Milliseconds after a `:` are a number, not a
// fraction, and are written with three digits.
func fractionOf(t time.Time, example string) string {
	separator, digits := exampleFraction(example)
	if digits == 0 {
		return ""
	}
	ms := t.Nanosecond() / int(time.Millisecond)
	if separator == ':' {
		return fmt.Sprintf(":%03d", ms)
	}
	value := strconv.Itoa(1000 + ms)[1:] // three digits, zero-padded
	if digits <= 3 {
		value = value[:digits]
	} else {
		value += strings.Repeat("0", digits-3)
	}
	return string(separator) + value
}

// exampleFraction finds the fraction of an example's seconds: the byte
// after the seconds (`.`, `,` or `:`) and the digits that follow it.
// The seconds are the third number of the first run of numbers joined
// by `:`, which is where every family with a fraction writes its time.
func exampleFraction(example string) (byte, int) {
	colons := 0
	for i := 1; i < len(example); i++ {
		if example[i] != ':' || !isDigit(example[i-1]) {
			continue
		}
		colons++
		if colons < 2 {
			continue
		}
		j := i + 1
		for j < len(example) && isDigit(example[j]) {
			j++
		}
		if j+1 >= len(example) || !strings.ContainsRune(".,:", rune(example[j])) || !isDigit(example[j+1]) {
			return 0, 0
		}
		k := j + 1
		for k < len(example) && isDigit(example[k]) {
			k++
		}
		return example[j], k - j - 1
	}
	return 0, 0
}

// zoneOf writes the zone of t the way an iso example writes its zone:
// `Z` or `+02:00`, `+0200`, or a word such as ` UTC` when t is at UTC;
// nothing when the example has no zone.
func zoneOf(t time.Time, example string) string {
	_, offset := t.Zone()
	switch {
	case strings.HasSuffix(example, "Z"):
		return t.Format("Z07:00")
	case strings.HasSuffix(example, " UTC") || strings.HasSuffix(example, " GMT"):
		if offset == 0 {
			return example[len(example)-4:]
		}
		return t.Format(" -07:00")
	case len(example) > 6 && isZoneSign(example[len(example)-6]) && example[len(example)-3] == ':':
		return t.Format("-07:00")
	case len(example) > 5 && isZoneSign(example[len(example)-5]) && allDigits(example[len(example)-4:]):
		return t.Format("-0700")
	}
	return ""
}

func isDigit(c byte) bool    { return c >= '0' && c <= '9' }
func isZoneSign(c byte) bool { return c == '+' || c == '-' }

// allDigits reports whether s is made of digits only.
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// FixedOffsetZone reads a zone written as ±HH:MM, as a time-range
// answer gives the zone of a file whose timestamps carry zones.
func FixedOffsetZone(text string) (*time.Location, bool) {
	if len(text) != 6 || !isZoneSign(text[0]) || text[3] != ':' {
		return nil, false
	}
	hours, errH := strconv.Atoi(text[1:3])
	minutes, errM := strconv.Atoi(text[4:6])
	if errH != nil || errM != nil || minutes > 59 {
		return nil, false
	}
	seconds := hours*3600 + minutes*60
	if text[0] == '-' {
		seconds = -seconds
	}
	return time.FixedZone(text, seconds), true
}
