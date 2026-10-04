package journal

// The rules output_short applies to field bytes: the stripping of a
// message, the printable test every printed field passes, and the size
// of a blob line.

import "strconv"

// Bytes strip_tab_ansi and the printable test look at.
const (
	escape = 0x1b // ESC, which starts a color or OSC sequence
	bell   = 0x07 // BEL, which ends an OSC sequence
)

// tabSpaces is what a tab in a message becomes: exactly 8 spaces,
// wherever the tab is; there are no tab stops (string-util.c:755).
const tabSpaces = "        "

// stripSpecial marks the bytes strip_tab_ansi does not copy unchanged in
// its ordinary state. An array indexed by the byte is a lookup table: a
// byte not listed is false, its zero value.
var stripSpecial = [256]bool{'\t': true, '\r': true, escape: true}

// appendStripped appends message as strip_tab_ansi (string-util.c:697-846)
// leaves it, the first thing done to a message without --all
// (logs-show.c:606-607):
//
//   - a tab becomes 8 spaces;
//   - a run of carriage returns is dropped before '\n' and at the end of
//     the message, and written as it is before any other byte;
//   - a color sequence, ESC '[' then digits, ';' or NUL, then 'm', is
//     removed; an OSC sequence, ESC ']' then bytes 0x20..0x7e, then BEL
//     or ESC '\', is removed;
//   - ESC followed by any other byte is written with that byte, which is
//     not interpreted; a lone ESC at the end is written;
//   - every other byte is copied.
//
// Stripping never writes a byte twice and makes nothing but a tab
// longer, and it keeps every '\n'.
func appendStripped(dst, message []byte) []byte {
	for i := 0; i < len(message); {
		// Copy the run of ordinary bytes up to the next special one in
		// one append; most messages are a single run.
		run := i
		for run < len(message) && !stripSpecial[message[run]] {
			run++
		}
		dst = append(dst, message[i:run]...)
		if run == len(message) {
			break
		}
		switch message[run] {
		case '\t':
			dst = append(dst, tabSpaces...)
			i = run + 1
		case '\r':
			dst, i = appendCarriageReturns(dst, message, run)
		default: // escape
			dst, i = appendEscape(dst, message, run)
		}
	}
	return dst
}

// appendCarriageReturns handles the run of '\r' at message[at:]. systemd
// counts them and writes them before the next byte, except before '\n',
// and drops them at the end of the message (string-util.c:744-752). It
// returns the index after the run.
func appendCarriageReturns(dst, message []byte, at int) ([]byte, int) {
	end := at
	for end < len(message) && message[end] == '\r' {
		end++
	}
	if end < len(message) && message[end] != '\n' {
		dst = append(dst, message[at:end]...)
	}
	return dst, end
}

// appendEscape handles the ESC at message[at] and returns where scanning
// continues. A color or OSC sequence that completes is removed. Otherwise
// ESC and the byte after it are written, and scanning continues after
// that byte. For a sequence that does not complete (an aborted candidate,
// string-util.c:784-829) that means: "ESC [" or "ESC ]" is written and
// the bytes after the bracket are scanned again as ordinary text, so a
// tab among them is expanded. For any other byte X (string-util.c:775-779)
// it means that X is written without being interpreted: a tab after ESC
// stays a tab, and a second ESC starts no sequence.
func appendEscape(dst, message []byte, at int) ([]byte, int) {
	if at+1 == len(message) {
		return append(dst, escape), at + 1 // a lone ESC at the end
	}
	var end int
	var removed bool
	switch message[at+1] {
	case '[':
		end, removed = colorSequenceEnd(message, at+2)
	case ']':
		end, removed = oscSequenceEnd(message, at+2)
	}
	if removed {
		return dst, end
	}
	return append(dst, message[at:at+2]...), at + 2
}

// colorSequenceEnd scans the bytes of a color sequence that start at
// message[from] (after "ESC [") and returns the index after its 'm'.
// Digits, ';' and NUL continue it; NUL does because systemd tests the
// byte with strchr("01234567890;m", c), which finds the string's
// terminating NUL (string-util.c:788). Any other byte, or the end of
// the message, aborts it (string-util.c:784-797).
func colorSequenceEnd(message []byte, from int) (int, bool) {
	for i := from; i < len(message); i++ {
		switch c := message[i]; {
		case c == 'm':
			return i + 1, true
		case c >= '0' && c <= '9', c == ';', c == 0:
			continue
		default:
			return 0, false
		}
	}
	return 0, false
}

// oscSequenceEnd scans the bytes of an OSC sequence that start at
// message[from] (after "ESC ]") and returns the index after its
// terminator, BEL or ESC '\'. Bytes 0x20..0x7e continue it; a control
// byte, 0x7f, any byte from 0x80, an ESC not followed by '\', or the end
// of the message abort it (string-util.c:799-829).
func oscSequenceEnd(message []byte, from int) (int, bool) {
	for i := from; i < len(message); i++ {
		switch c := message[i]; {
		case c == bell:
			return i + 1, true
		case c == escape:
			if i+1 < len(message) && message[i+1] == '\\' {
				return i + 2, true
			}
			return 0, false
		case c >= 0x20 && c <= 0x7e:
			continue
		default:
			return 0, false
		}
	}
	return 0, false
}

// printThreshold is PRINT_CHAR_THRESHOLD (logs-show.c:46): a hostname,
// identifier or pid of this many bytes or more is not printed.
const printThreshold = 300

// shallPrint is shall_print without --all (logs-show.c:198-211): a field
// is printed when it is shorter than printThreshold bytes and printable.
func shallPrint(value []byte) bool {
	return len(value) < printThreshold && utf8IsPrintable(value)
}

// utf8IsPrintable is systemd's utf8_is_printable, which is
// utf8_is_printable_newline(s, len, true) (utf8.h:30, utf8.c:109-130):
// b splits completely into valid UTF-8 sequences of code points that are
// not control characters. Tab and newline are printable.
func utf8IsPrintable(b []byte) bool {
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 { // one byte: printable unless a control character
			if unicharIsControl(uint32(c)) {
				return false
			}
			i++
			continue
		}
		value, size, ok := decodeSystemdUTF8(b[i:])
		if !ok || unicharIsControl(value) {
			return false
		}
		i += size
	}
	return true
}

// decodeSystemdUTF8 decodes the multi-byte sequence at the start of b as
// utf8_encoded_valid_unichar (utf8.c:529-569) and utf8_encoded_to_unichar
// do. The first byte gives the length (utf8.c:48-63); a sequence that
// runs past the end of b, a byte that is not a continuation byte, an
// overlong form and a code point unichar_is_valid refuses are invalid.
// 5- and 6-byte forms decode to values above U+10FFFF or are overlong,
// so they are always invalid.
func decodeSystemdUTF8(b []byte) (uint32, int, bool) {
	size := encodedExpectedLen(b[0])
	if size == 0 || size > len(b) {
		return 0, 0, false
	}
	// The first byte's value bits: the low 7 - size bits.
	value := uint32(b[0]) & (0xff >> (size + 1))
	for _, c := range b[1:size] {
		if c&0xc0 != 0x80 { // every later byte must be 10xxxxxx
			return 0, 0, false
		}
		value = value<<6 | uint32(c&0x3f)
	}
	if unicharEncodedLen(value) != size || !unicharIsValid(value) {
		return 0, 0, false
	}
	return value, size, true
}

// utf8LeadBytes lists, for each sequence length, the bits of a first
// byte that are fixed and their values (utf8.c:48-63). A byte matching
// none (10xxxxxx, 0xfe, 0xff) starts no sequence.
var utf8LeadBytes = [...]struct {
	mask, want byte
	size       int
}{
	{0x80, 0x00, 1}, {0xe0, 0xc0, 2}, {0xf0, 0xe0, 3}, {0xf8, 0xf0, 4}, {0xfc, 0xf8, 5}, {0xfe, 0xfc, 6},
}

// encodedExpectedLen is utf8_encoded_expected_len: the length of the
// sequence c starts, or 0 when c starts none.
func encodedExpectedLen(c byte) int {
	for _, lead := range utf8LeadBytes {
		if c&lead.mask == lead.want {
			return lead.size
		}
	}
	return 0
}

// utf8EncodedLimits holds, for n = 1..5, the first code point that needs
// more than n bytes (utf8_unichar_to_encoded_len, utf8.c:512-526).
var utf8EncodedLimits = [...]uint32{0x80, 0x800, 0x10000, 0x200000, 0x4000000}

// unicharEncodedLen is the number of bytes systemd's encoder uses for
// value; any other length is an overlong form.
func unicharEncodedLen(value uint32) int {
	for i, limit := range utf8EncodedLimits {
		if value < limit {
			return i + 1
		}
	}
	return len(utf8EncodedLimits) + 1
}

// unicharIsValid is unichar_is_valid (utf8.c:21-32): not past the end of
// Unicode, not a UTF-16 surrogate, not U+FDD0..U+FDEF, and its low 16
// bits not FFFE or FFFF.
func unicharIsValid(value uint32) bool {
	switch {
	case value >= 0x110000:
		return false
	case value&0xfffff800 == 0xd800:
		return false
	case value >= 0xfdd0 && value <= 0xfdef:
		return false
	case value&0xfffe == 0xfffe:
		return false
	}
	return true
}

// unicharIsControl is unichar_is_control (utf8.c:35-45): the C0 range
// except tab and newline, DEL and the C1 range.
func unicharIsControl(value uint32) bool {
	return (value < ' ' && value != '\t' && value != '\n') || (value >= 0x7f && value <= 0x9f)
}

// iecUnits are format_bytes' IEC units, largest first (format-util.c:13-20).
var iecUnits = [...]struct {
	suffix byte
	factor uint64
}{
	{'E', 1 << 60}, {'P', 1 << 50}, {'T', 1 << 40}, {'G', 1 << 30}, {'M', 1 << 20}, {'K', 1 << 10},
}

// appendFormatBytes appends size as FORMAT_BYTES writes it
// (format_bytes_full with IEC units, one decimal below the point and a
// trailing B, format-util.c:8-66, format-util.h:79-86). Below 1024 it is
// the number and "B"; else the number of the largest unit not above
// size, then "." and the tenths digit when that digit is not 0, then the
// unit's letter. Every division truncates; the tenths digit of a unit
// comes from the size in the next smaller unit (format-util.c:41-56).
func appendFormatBytes(dst []byte, size uint64) []byte {
	smallest := iecUnits[len(iecUnits)-1].factor
	for i, unit := range iecUnits {
		if size < unit.factor {
			continue
		}
		var tenths uint64
		if i != len(iecUnits)-1 {
			tenths = (size / iecUnits[i+1].factor * 10 / smallest) % 10
		} else {
			tenths = (size * 10 / unit.factor) % 10
		}
		dst = strconv.AppendUint(dst, size/unit.factor, 10)
		if tenths > 0 {
			dst = append(dst, '.', byte('0'+tenths))
		}
		return append(dst, unit.suffix)
	}
	dst = strconv.AppendUint(dst, size, 10)
	return append(dst, 'B')
}
