package timestamps

// matchDotted reads the European dotted date, day first, then the clock
// with an optional fraction: `06.10.2026 12:34:56`.
func matchDotted(b []byte, i int) (r fields, end int, ok bool) {
	j := i
	if r.day, j, ok = number(b, j, 1, 2); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, '.'); !ok {
		return r, i, false
	}
	if r.month, j, ok = number(b, j, 1, 2); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, '.'); !ok {
		return r, i, false
	}
	if r.year, j, ok = number(b, j, 4, 4); !ok {
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
