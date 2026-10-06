package timestamps

// matchSlash reads a slash date with the year last, then the clock with
// an optional fraction and an optional AM/PM: `10/06/2026 12:34:56 PM`,
// `06/10/26 14:34:56`. A two-digit year 70–99 is 19xx and 00–69 is
// 20xx.
//
// The two leading numbers are returned as written, the first in r.month
// and the second in r.day; the Parser swaps them for a day-first file.
func matchSlash(b []byte, i int) (r fields, end int, ok bool) {
	j := i
	if r.month, j, ok = number(b, j, 1, 2); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, '/'); !ok {
		return r, i, false
	}
	if r.day, j, ok = number(b, j, 1, 2); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, '/'); !ok {
		return r, i, false
	}
	start := j
	if r.year, j, ok = number(b, j, 2, 4); !ok {
		return r, i, false
	}
	switch j - start {
	case 2:
		r.year = twoDigitYear(r.year)
	case 3:
		return r, i, false
	}
	if j, ok = expect(b, j, ' '); !ok {
		return r, i, false
	}
	var c fields
	if c, j, ok = clock(b, j); !ok {
		return r, i, false
	}
	r.hour, r.minute, r.second = c.hour, c.minute, c.second
	if r.milli, j, ok = decimalFraction(b, j); !ok {
		return r, i, false
	}
	if r.hour, j, ok = amPM(b, j, r.hour); !ok {
		return r, i, false
	}
	return r, j, true
}

// twoDigitYear expands a two-digit year: 70–99 → 1970–1999, 00–69 →
// 2000–2069, the POSIX strptime rule.
func twoDigitYear(y int) int {
	if y >= 70 {
		return 1900 + y
	}
	return 2000 + y
}
