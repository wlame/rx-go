package timestamps

// matchCLF reads the Common Log Format time of Apache and nginx access
// logs, brackets included: `[06/Oct/2026:12:34:56 +0000]`. The zone is
// part of the format, so every value is an instant.
func matchCLF(b []byte, i int) (r fields, end int, ok bool) {
	j := i
	if j, ok = expect(b, j, '['); !ok {
		return r, i, false
	}
	if r.day, j, ok = number(b, j, 1, 2); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, '/'); !ok {
		return r, i, false
	}
	if r.month, j, ok = monthName(b, j); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, '/'); !ok {
		return r, i, false
	}
	if r.year, j, ok = number(b, j, 4, 4); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, ':'); !ok {
		return r, i, false
	}
	var c fields
	if c, j, ok = clock(b, j); !ok {
		return r, i, false
	}
	r.hour, r.minute, r.second = c.hour, c.minute, c.second
	if j, ok = expect(b, j, ' '); !ok {
		return r, i, false
	}
	var hasMinutes bool
	if r.offsetMinutes, j, hasMinutes, ok = numericOffset(b, j); !ok || !hasMinutes {
		return r, i, false
	}
	r.zoned = true
	if j, ok = expect(b, j, ']'); !ok {
		return r, i, false
	}
	return r, j, true
}
