package timestamps

// Byte-level reading helpers shared by the family matchers.
//
// Every helper takes the line b and a position j, and returns the
// position just past what it read ("next"). On failure it returns ok =
// false and the caller gives up on the whole timestamp. None of them
// reads past len(b) and none allocates: they index into b and return
// integers.

// maxFractionDigits is the longest fraction of a second a timestamp may
// carry (nanoseconds). A longer digit run is not a timestamp.
const maxFractionDigits = 9

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLetter(c byte) bool { return (c|0x20) >= 'a' && (c|0x20) <= 'z' }

func isAlnum(c byte) bool { return isDigit(c) || isLetter(c) }

// number reads a run of minDigits to maxDigits decimal digits at b[j].
// The run must end there: a further digit means the field is longer than
// the format allows, so `2026-10-06 12:34:567` is not a timestamp.
func number(b []byte, j, minDigits, maxDigits int) (v, next int, ok bool) {
	n := 0
	for j+n < len(b) && n < maxDigits && isDigit(b[j+n]) {
		v = v*10 + int(b[j+n]-'0')
		n++
	}
	if n < minDigits || (j+n < len(b) && isDigit(b[j+n])) {
		return 0, j, false
	}
	return v, j + n, true
}

// twoDigits reads exactly two digits at b[j], whatever follows them.
func twoDigits(b []byte, j int) (v, next int, ok bool) {
	if j+1 >= len(b) || !isDigit(b[j]) || !isDigit(b[j+1]) {
		return 0, j, false
	}
	return int(b[j]-'0')*10 + int(b[j+1]-'0'), j + 2, true
}

// expect reads the single byte c at b[j].
func expect(b []byte, j int, c byte) (next int, ok bool) {
	if j < len(b) && b[j] == c {
		return j + 1, true
	}
	return j, false
}

// expectPadding reads one space, or two when the field after it is
// space-padded (`Oct  6`).
func expectPadding(b []byte, j int) (next int, ok bool) {
	if j, ok = expect(b, j, ' '); !ok {
		return j, false
	}
	if j < len(b) && b[j] == ' ' {
		j++
	}
	return j, true
}

// clock reads `H:M:S`, each field one or two digits.
func clock(b []byte, j int) (r fields, next int, ok bool) {
	if r.hour, j, ok = number(b, j, 1, 2); !ok {
		return r, j, false
	}
	if j, ok = expect(b, j, ':'); !ok {
		return r, j, false
	}
	if r.minute, j, ok = number(b, j, 1, 2); !ok {
		return r, j, false
	}
	if j, ok = expect(b, j, ':'); !ok {
		return r, j, false
	}
	if r.second, j, ok = number(b, j, 1, 2); !ok {
		return r, j, false
	}
	return r, j, true
}

// decimalFraction reads an optional fraction of a second after `.` or
// `,` and returns it in milliseconds, dropping digits past the third. A
// separator not followed by a digit ends the timestamp before it
// (`12:34:56, then`).
func decimalFraction(b []byte, j int) (milli, next int, ok bool) {
	if j+1 >= len(b) || (b[j] != '.' && b[j] != ',') || !isDigit(b[j+1]) {
		return 0, j, true
	}
	k, n := j+1, 0
	for k < len(b) && isDigit(b[k]) {
		if n == maxFractionDigits {
			return 0, j, false
		}
		if n < 3 {
			milli = milli*10 + int(b[k]-'0')
		}
		n++
		k++
	}
	for ; n < 3; n++ {
		milli *= 10
	}
	return milli, k, true
}

// colonMillis reads milliseconds written after a colon (`12:34:5:123`),
// which must be exactly three digits: `12:34:56:12` is not a timestamp. A
// colon followed by something other than a digit ends the timestamp
// before it (`12:34:56: message`).
func colonMillis(b []byte, j int) (milli, next int, ok bool) {
	if j+1 >= len(b) || !isDigit(b[j+1]) {
		return 0, j, true
	}
	return number(b, j+1, 3, 3)
}

// numericOffset reads a zone offset at b[j]: a sign, two hour digits and
// optionally two minute digits, with or without a colon (`+02`, `+0200`,
// `-05:30`). hasMinutes says whether the minutes were written. Hours run
// to 23 and minutes to 59.
func numericOffset(b []byte, j int) (minutes, next int, hasMinutes, ok bool) {
	if j >= len(b) || (b[j] != '+' && b[j] != '-') {
		return 0, j, false, false
	}
	negative := b[j] == '-'
	h, k, ok := twoDigits(b, j+1)
	if !ok {
		return 0, j, false, false
	}
	m := 0
	if k < len(b) && b[k] == ':' {
		if m, k, ok = twoDigits(b, k+1); !ok {
			return 0, j, false, false
		}
		hasMinutes = true
	} else if k+1 < len(b) && isDigit(b[k]) && isDigit(b[k+1]) {
		m, k, _ = twoDigits(b, k)
		hasMinutes = true
	}
	if (k < len(b) && isDigit(b[k])) || h > 23 || m > 59 {
		return 0, j, false, false
	}
	minutes = h*60 + m
	if negative {
		minutes = -minutes
	}
	return minutes, k, hasMinutes, true
}

// hasWord reports whether b holds the upper-case word w at j, not
// followed by another letter.
func hasWord(b []byte, j int, w string) bool {
	if j+len(w) > len(b) {
		return false
	}
	for k := 0; k < len(w); k++ {
		if b[j+k] != w[k] {
			return false
		}
	}
	return j+len(w) == len(b) || !isLetter(b[j+len(w)])
}

// monthNames and weekdayNames are the English three-letter names, lower
// case. A name in a line matches in any case.
var (
	monthNames   = [12]string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	weekdayNames = [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
)

// nameIndex reads a three-letter name from names at b[j], in any case,
// not followed by another letter, and returns its position in names.
func nameIndex(b []byte, j int, names []string) (index, next int, ok bool) {
	if j+3 > len(b) || (j+3 < len(b) && isLetter(b[j+3])) {
		return 0, j, false
	}
	for n, name := range names {
		if b[j]|0x20 == name[0] && b[j+1]|0x20 == name[1] && b[j+2]|0x20 == name[2] {
			return n, j + 3, true
		}
	}
	return 0, j, false
}

// monthName reads a month name and returns its number, 1–12.
func monthName(b []byte, j int) (month, next int, ok bool) {
	n, next, ok := nameIndex(b, j, monthNames[:])
	return n + 1, next, ok
}

// weekdayName reads a weekday name; the weekday itself is not checked
// against the date.
func weekdayName(b []byte, j int) (next int, ok bool) {
	_, next, ok = nameIndex(b, j, weekdayNames[:])
	return next, ok
}

// amPM reads an optional `AM`/`PM` (or `am`/`pm`), with or without a
// space before it, and applies it to a 12-hour clock: 12 AM is 00, 12 PM
// is 12, 1 PM is 13. With AM/PM the hour must be 1–12.
func amPM(b []byte, j, hour int) (hour24, next int, ok bool) {
	k := j
	if k < len(b) && b[k] == ' ' {
		k++
	}
	if k+1 >= len(b) || (b[k+1] != 'M' && b[k+1] != 'm') || (k+2 < len(b) && isLetter(b[k+2])) {
		return hour, j, true
	}
	var pm bool
	switch b[k] {
	case 'A', 'a':
	case 'P', 'p':
		pm = true
	default:
		return hour, j, true
	}
	if hour < 1 || hour > 12 {
		return 0, j, false
	}
	hour24 = hour % 12
	if pm {
		hour24 += 12
	}
	return hour24, k + 2, true
}
