package journal

// The timestamp at the start of an entry's first line, and the parse of
// _SOURCE_REALTIME_TIMESTAMP that chooses it.

import (
	"bytes"
	"math"
	"strings"
	"time"
)

// realtimeLimit bounds a valid realtime: VALID_REALTIME(u) is
// u > 0 && u < (1ULL << 55) (journal-file.h:172-175).
const realtimeLimit = 1 << 55

// validRealtime reports whether usec is a realtime systemd accepts.
func validRealtime(usec uint64) bool {
	return usec > 0 && usec < realtimeLimit
}

// displayRealtime returns the time a line shows (parse_display_realtime,
// logs-show.c:455-490): _SOURCE_REALTIME_TIMESTAMP when it parses with
// safe_atou64 to a valid realtime (logs-show.c:470), else the ENTRY
// object's realtime (logs-show.c:476). An invalid source value never
// skips the entry; an invalid result does (logs-show.c:377-378, 625-626),
// reported here as false. _SOURCE_MONOTONIC_TIMESTAMP is not used
// (logs-show.c:467).
func displayRealtime(entryRealtime uint64, fields *pickedFields) (uint64, bool) {
	if source, ok := fields.get(fieldSourceRealtime); ok {
		if usec, ok := safeAtou64(source); ok && validRealtime(usec) {
			return usec, true
		}
	}
	return entryRealtime, validRealtime(entryRealtime)
}

// systemdWhitespace is systemd's WHITESPACE (string-util.h:14), the bytes
// safe_atou64 skips before the number.
const systemdWhitespace = " \t\n\r"

// safeAtou64 is systemd's safe_atou64, which is safe_atollu_full(s, 0)
// (parse-util.h:80-87, parse-util.c:492-528). It returns false where
// systemd returns an error.
func safeAtou64(value []byte) (uint64, bool) {
	// The value is read as a C string: it ends at the first NUL
	// (parse_field stores it NUL-terminated, logs-show.c:141).
	if nul := bytes.IndexByte(value, 0); nul >= 0 {
		value = value[:nul]
	}
	// s += strspn(s, WHITESPACE) (parse-util.c:503).
	for len(value) > 0 && strings.IndexByte(systemdWhitespace, value[0]) >= 0 {
		value = value[1:]
	}
	// mangle_base (parse-util.c:375-399): a "0b" or "0o" prefix chooses
	// base 2 or 8 and is removed; otherwise strtoull chooses the base.
	value, base := mangleBase(value)
	number, digitsEnd, overflow := strtoull(value, base)
	switch {
	case overflow: // errno ERANGE (parse-util.c:517-518)
		return 0, false
	case digitsEnd == 0, digitsEnd != len(value): // no digit, or bytes left over (parse-util.c:519-520)
		return 0, false
	case number != 0 && value[0] == '-': // a negative number (parse-util.c:521-522)
		return 0, false
	}
	return number, true
}

// mangleBase removes a "0b"/"0B" or "0o"/"0O" prefix and returns the base
// it chooses, or 0 (strtoull decides) when there is none.
func mangleBase(s []byte) ([]byte, uint64) {
	if len(s) >= 2 && s[0] == '0' {
		switch s[1] {
		case 'b', 'B':
			return s[2:], 2
		case 'o', 'O':
			return s[2:], 8
		}
	}
	return s, 0
}

// cWhitespace is what glibc's strtoull skips first: isspace in the C
// locale, which unlike systemdWhitespace includes '\v' and '\f'.
const cWhitespace = " \t\n\v\f\r"

// strtoull is glibc's strtoull(s, &end, base) for base 0, 2 or 8, as
// glibc 2.38 and later run it: it returns the number, the index of the
// first byte after the digits (0 when there is no digit, as glibc then
// points end at s), and whether the value overflowed 2^64-1. A leading
// '-' negates modulo 2^64, as in C.
func strtoull(s []byte, base uint64) (number uint64, digitsEnd int, overflow bool) {
	i := 0
	for i < len(s) && strings.IndexByte(cWhitespace, s[i]) >= 0 {
		i++
	}
	negative := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		negative = s[i] == '-'
		i++
	}
	base, i = numberBase(s, i, base)
	first := i
	for ; i < len(s); i++ {
		digit := digitValue(s[i])
		if digit >= base {
			break
		}
		// Overflow is checked before the multiplication, which would
		// otherwise wrap silently; once it overflowed, the remaining
		// digits are still consumed, as glibc consumes them.
		if number > (math.MaxUint64-digit)/base {
			overflow = true
		}
		number = number*base + digit
	}
	if i == first {
		return 0, 0, false
	}
	if overflow {
		return math.MaxUint64, i, true
	}
	if negative {
		number = -number // unsigned negation wraps modulo 2^64, as C's does
	}
	return number, i, false
}

// numberBase applies strtoull's prefix rules at s[i]: in base 0, "0x" or
// "0X" followed by a hex digit means base 16; in base 0 or 2, "0b" or
// "0B" followed by 0 or 1 means base 2 (glibc 2.38 and later); otherwise
// base 0 means 8 after a leading '0' and 10 else. It returns the base and
// the index of the first digit.
func numberBase(s []byte, i int, base uint64) (uint64, int) {
	switch {
	case base == 0 && hasRadixPrefix(s, i, 'x', 16):
		return 16, i + 2
	case (base == 0 || base == 2) && hasRadixPrefix(s, i, 'b', 2):
		return 2, i + 2
	case base == 0 && i < len(s) && s[i] == '0':
		return 8, i
	case base == 0:
		return 10, i
	}
	return base, i
}

// hasRadixPrefix reports whether s[i:] starts with '0', then letter in
// either case, then a digit of radix: strtoull takes "0x" or "0b" as a
// prefix only when a digit follows it, and otherwise reads the '0' alone.
func hasRadixPrefix(s []byte, i int, letter byte, radix uint64) bool {
	// c|0x20 maps an ASCII upper-case letter to its lower case.
	return i+2 < len(s) && s[i] == '0' && s[i+1]|0x20 == letter && digitValue(s[i+2]) < radix
}

// digitValue is the value of an ASCII digit or letter in bases up to 36,
// and 36 for any other byte, which no base accepts.
func digitValue(c byte) uint64 {
	switch {
	case c >= '0' && c <= '9':
		return uint64(c - '0')
	case c >= 'a' && c <= 'z':
		return uint64(c-'a') + 10
	case c >= 'A' && c <= 'Z':
		return uint64(c-'A') + 10
	}
	return 36
}

// appendStamp appends usec as "%Y-%m-%dT%H:%M:%S.uuuuuu+00:00": gmtime of
// the whole seconds (time-util.c:1668-1685 under --utc), strftime at
// logs-show.c:404-407, the microseconds at logs-show.c:430 and the zone
// offset of gmtime at logs-show.c:438-441. usec is a valid realtime,
// below 2^55, so the year is 1970..3111 and the stamp is always stampLen
// bytes. Go's calendar is the proleptic Gregorian one glibc's gmtime
// uses.
func appendStamp(dst []byte, usec uint64) []byte {
	t := time.UnixMicro(int64(usec)).UTC() // #nosec G115 -- a valid realtime is below 2^55
	year, month, day := t.Date()
	hour, minute, second := t.Clock()
	dst = appendZeroPadded(dst, year, 4)
	dst = append(dst, '-')
	dst = appendZeroPadded(dst, int(month), 2)
	dst = append(dst, '-')
	dst = appendZeroPadded(dst, day, 2)
	dst = append(dst, 'T')
	dst = appendZeroPadded(dst, hour, 2)
	dst = append(dst, ':')
	dst = appendZeroPadded(dst, minute, 2)
	dst = append(dst, ':')
	dst = appendZeroPadded(dst, second, 2)
	dst = append(dst, '.')
	dst = appendZeroPadded(dst, int(usec%1_000_000), 6)
	return append(dst, "+00:00"...)
}

// appendZeroPadded appends the decimal digits of v, a number of at most
// width digits, with leading zeros up to width.
func appendZeroPadded(dst []byte, v, width int) []byte {
	var digits [8]byte
	for i := width - 1; i >= 0; i-- {
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return append(dst, digits[:width]...)
}
