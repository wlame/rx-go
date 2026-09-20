package timestamps

// matchSyslog reads the BSD syslog form, which has no year: month name,
// day (space-padded or not), the clock with an optional fraction.
// `Oct  6 12:34:56`, `Dec 10 12:34:56.123`. The year comes later, from
// the file's mtime (see inferYear); r.year stays 0 here.
func matchSyslog(b []byte, i int) (r fields, end int, ok bool) {
	j := i
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
	return r, j, true
}
