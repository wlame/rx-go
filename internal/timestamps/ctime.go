package timestamps

// matchCtime reads the C library's ctime form, as the Apache error log
// writes it: weekday, month name, day (space-padded or not), the clock
// with an optional fraction, then the year. `Tue Oct  6 12:34:56 2026`,
// `Tue Oct 06 12:34:56.123456 2026`. The weekday is read but not checked
// against the date.
func matchCtime(b []byte, i int) (r fields, end int, ok bool) {
	j := i
	if j, ok = weekdayName(b, j); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, ' '); !ok {
		return r, i, false
	}
	if r.month, j, ok = monthName(b, j); !ok {
		return r, i, false
	}
	if j, ok = expectPadding(b, j); !ok {
		return r, i, false
	}
	if r.day, j, ok = number(b, j, 1, 2); !ok {
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
	if j, ok = expect(b, j, ' '); !ok {
		return r, i, false
	}
	if r.year, j, ok = number(b, j, 4, 4); !ok {
		return r, i, false
	}
	return r, j, true
}
