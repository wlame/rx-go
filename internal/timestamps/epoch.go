package timestamps

// The epoch family accepts instants from 2000-01-01T00:00:00Z up to, not
// including, 2100-01-01T00:00:00Z. Outside that span a 10- or 13-digit
// number is far more likely an ID or a counter than a time. Written
// without leading zeros, epoch seconds and milliseconds reach 10 and 13
// digits only on 2001-09-09, so in practice the span starts there.
const (
	epochMinMs = int64(946684800000)  // 2000-01-01T00:00:00Z
	epochMaxMs = int64(4102444800000) // 2100-01-01T00:00:00Z, excluded
)

// Digit counts of the two epoch forms.
const (
	epochSecondDigits = 10
	epochMilliDigits  = 13
)

// matchEpoch reads a Unix time: 10 digits of seconds with an optional
// fraction after `.`, or 13 digits of milliseconds. Where it may start
// (column 0, or right after `[`) is the family table's rule.
func matchEpoch(b []byte, i int) (r fields, end int, ok bool) {
	j := i
	var v int64
	for j < len(b) && isDigit(b[j]) && j-i <= epochMilliDigits {
		v = v*10 + int64(b[j]-'0')
		j++
	}
	switch j - i {
	case epochSecondDigits:
		v *= msPerSecond
		if j < len(b) && b[j] == '.' {
			var milli int
			if milli, j, ok = decimalFraction(b, j); !ok {
				return r, i, false
			}
			v += int64(milli)
		}
	case epochMilliDigits:
	default:
		return r, i, false
	}
	if v < epochMinMs || v >= epochMaxMs {
		return r, i, false
	}
	r.isEpoch, r.epochMs = true, v
	return r, j, true
}
