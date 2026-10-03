package timestamps

// matchISO reads the iso family: a 4-digit year, then month and day of
// one or two digits separated by `-` or `/` (the same both times), then
// `T` or one space, the clock, an optional fraction after `.` or `,`, or
// one to three digits of milliseconds after `:`, and an optional zone: `Z`, `±HH:MM`,
// `±HHMM`, `±HH`, or a space then `UTC`, `GMT` or `±HHMM`/`±HH:MM`.
//
// Any other zone word after a space (`MST`, `CEST`) is ambiguous, is not
// read, and leaves the timestamp zone-less: it ends before the space.
func matchISO(b []byte, i int) (r fields, end int, ok bool) {
	j := i
	if r.year, j, ok = number(b, j, 4, 4); !ok {
		return r, i, false
	}
	if j >= len(b) || (b[j] != '-' && b[j] != '/') {
		return r, i, false
	}
	sep := b[j]
	j++
	if r.month, j, ok = number(b, j, 1, 2); !ok {
		return r, i, false
	}
	if j, ok = expect(b, j, sep); !ok {
		return r, i, false
	}
	if r.day, j, ok = number(b, j, 1, 2); !ok {
		return r, i, false
	}
	if j >= len(b) || (b[j] != 'T' && b[j] != ' ') {
		return r, i, false
	}
	var c fields
	if c, j, ok = clock(b, j+1); !ok {
		return r, i, false
	}
	r.hour, r.minute, r.second = c.hour, c.minute, c.second
	if j < len(b) && b[j] == ':' {
		r.milli, j, ok = colonMillis(b, j)
	} else {
		r.milli, j, ok = decimalFraction(b, j)
	}
	if !ok {
		return r, i, false
	}
	if r.offsetMinutes, r.zoned, j, ok = isoZone(b, j); !ok {
		return r, i, false
	}
	return r, j, true
}

// isoZone reads the optional zone after an iso time. A sign followed by a
// digit right after the time must be a valid offset, or the line has no
// timestamp: reading `+24:00` as zone-less would misplace it by a day.
// After a space, anything that is not a zone is message text.
func isoZone(b []byte, j int) (minutes int, zoned bool, next int, ok bool) {
	if j >= len(b) {
		return 0, false, j, true
	}
	switch b[j] {
	case 'Z':
		return 0, true, j + 1, true
	case '+', '-':
		if j+1 >= len(b) || !isDigit(b[j+1]) {
			return 0, false, j, true
		}
		if minutes, next, _, ok = numericOffset(b, j); !ok {
			return 0, false, j, false
		}
		return minutes, true, next, true
	case ' ':
		if hasWord(b, j+1, "UTC") || hasWord(b, j+1, "GMT") {
			return 0, true, j + 4, true
		}
		var hasMinutes bool
		if minutes, next, hasMinutes, ok = numericOffset(b, j+1); ok && hasMinutes {
			return minutes, true, next, true
		}
	}
	return 0, false, j, true
}
