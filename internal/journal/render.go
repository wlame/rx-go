package journal

// This file renders one journal entry as the text
//
//	journalctl --utc -q -o short-iso-precise
//
// of systemd 257.13 prints for it, with stdout not a terminal and no
// SYSTEMD_COLORS or SYSTEMD_URLIFY in the environment. That command runs
// output_short in src/shared/logs-show.c; the comments cite the systemd
// v257.13 sources by file and line. render_time.go holds the timestamp,
// render_message.go the message's own rules.
//
// All lengths are in bytes. Nothing in this mode measures display
// columns, limits a length or ellipsizes a line.

import "bytes"

// Field is one FIELD=value item of an entry, in the entry's stored order
// (sorted by DATA object offset, as systemd stores them).
type Field struct {
	Name  string
	Value []byte
}

// Entry is what the renderer needs of one journal entry.
type Entry struct {
	Realtime uint64 // the ENTRY object's realtime, microseconds since the epoch
	Fields   []Field
}

// AppendText appends e's text (one line, or more for a multi-line
// message, each ended by '\n') to dst and reports whether e is shown.
// An entry without MESSAGE is not shown and appends nothing.
//
// The text is the bytes journalctl prints for the entry: a 32-byte UTC
// stamp, the hostname, the identifier and the pid, ": " and the message,
// whose further lines are indented under the first. An entry is also not
// shown when neither its _SOURCE_REALTIME_TIMESTAMP nor its Realtime is a
// valid time (above 0 and below 2^55 microseconds). journalctl never
// renders an entry whose Realtime is not valid: it refuses the ENTRY
// object first (journal-file.c:1007-1011). The false result does not tell
// that case from a missing MESSAGE, so a caller that counts entries by
// kind checks Realtime itself.
//
// Like the standard library's append functions, AppendText returns the
// extended slice, which may have a new backing array: use the returned
// slice, not dst. It allocates nothing when dst has room for the text.
func AppendText(dst []byte, e Entry) ([]byte, bool) {
	fields := pickFields(e.Fields)
	message, hasMessage := fields.get(fieldMessage)
	if !hasMessage {
		// logs-show.c:598-601: "Skipping message without MESSAGE= field."
		return dst, false
	}
	usec, valid := displayRealtime(e.Realtime, &fields)
	if !valid {
		// logs-show.c:377-378 and 625-626: no valid time, the entry is
		// skipped.
		return dst, false
	}
	prefix := choosePrefix(&fields)
	indent := prefix.indent()

	// One reservation for the whole text, so that an entry costs at most
	// one allocation, and none when dst already has room.
	dst = grow(dst, prefix.length()+messageRoom(message, indent))
	dst = appendStamp(dst, usec)
	dst = prefix.appendTo(dst)
	return appendMessage(dst, message, indent), true
}

// shortField names one of the fields output_short reads. The constants
// index the arrays of pickedFields; iota numbers them 0, 1, 2, ...
type shortField int

const (
	fieldMessage shortField = iota
	fieldHostname
	fieldIdentifier
	fieldComm
	fieldPID
	fieldSyslogPID
	fieldSourceRealtime
	shortFieldCount // the number of fields above, not a field
)

// shortFieldNames is each field's name. A name is matched whole: systemd
// compares an item against the name followed by '=' (logs-show.c:133-137,
// with the table at logs-show.c:557-572), so MESSAGE_ID is not MESSAGE.
// output_short reads a few more fields (PRIORITY, _TRANSPORT,
// CONFIG_FILE, DOCUMENTATION, the unit fields) that only choose colors or
// hyperlinks, which this mode never prints, and
// _SOURCE_MONOTONIC_TIMESTAMP, which it ignores (logs-show.c:467).
var shortFieldNames = [shortFieldCount]string{
	fieldMessage:        "MESSAGE",
	fieldHostname:       "_HOSTNAME",
	fieldIdentifier:     "SYSLOG_IDENTIFIER",
	fieldComm:           "_COMM",
	fieldPID:            "_PID",
	fieldSyslogPID:      "SYSLOG_PID",
	fieldSourceRealtime: "_SOURCE_REALTIME_TIMESTAMP",
}

// identifierFields and pidFields are the fields tried, in order, for the
// name and for the pid of a line (logs-show.c:642-670): the first one
// present and printable is used.
var (
	identifierFields = [...]shortField{fieldIdentifier, fieldComm}
	pidFields        = [...]shortField{fieldPID, fieldSyslogPID}
)

// pickedFields holds, for each field of shortFieldNames, the value of its
// last item in the entry's stored order and whether the entry has the
// field at all. Presence is kept apart from the value because an empty
// value is a present field and prints: an empty hostname as one space,
// an empty pid as "[]". The struct is a fixed-size value (arrays, not
// slices), so making one needs no heap allocation.
type pickedFields struct {
	value   [shortFieldCount][]byte
	present [shortFieldCount]bool
}

// pickFields returns the values output_short uses. When a field occurs
// more than once, the last item in stored order wins: parse_field
// replaces the previous value (logs-show.c:145), and the items are
// enumerated in stored order (sd-journal.c:2859-2904).
func pickFields(fields []Field) pickedFields {
	var picked pickedFields
	for _, f := range fields {
		if field, ok := shortFieldOf(f.Name); ok {
			picked.value[field] = f.Value
			picked.present[field] = true
		}
	}
	return picked
}

// shortFieldOf returns the field a name stands for, if output_short reads
// it.
func shortFieldOf(name string) (shortField, bool) {
	for field, fieldName := range shortFieldNames {
		if name == fieldName {
			return shortField(field), true
		}
	}
	return 0, false
}

// get returns the field's value and whether the entry has the field.
// The receiver is a pointer so that the arrays are not copied per call.
func (p *pickedFields) get(field shortField) ([]byte, bool) {
	return p.value[field], p.present[field]
}

// firstPrintable returns the value of the first of fields that the entry
// has and that shall_print accepts.
func (p *pickedFields) firstPrintable(fields []shortField) ([]byte, bool) {
	for _, field := range fields {
		if value, ok := p.get(field); ok && shallPrint(value) {
			return value, true
		}
	}
	return nil, false
}

// stampLen is the length of every stamp (appendStamp).
const stampLen = 32

// unknownIdentifier is what a line shows when no identifier can be
// printed (logs-show.c:662).
const unknownIdentifier = " unknown"

// linePrefix is what output_short writes between the stamp and the
// message, chosen from the entry's fields. Each part is printed raw: a
// tab or a newline in a hostname is written as it is.
type linePrefix struct {
	hostname, identifier, pid          []byte
	hasHostname, hasIdentifier, hasPID bool
}

// choosePrefix picks the hostname (logs-show.c:637-640), the identifier
// (SYSLOG_IDENTIFIER, else _COMM, logs-show.c:655-662) and the pid
// (_PID, else SYSLOG_PID, logs-show.c:664-670). A part is printed only
// when shall_print accepts it, else it is left out (the identifier is
// then " unknown").
func choosePrefix(fields *pickedFields) linePrefix {
	var prefix linePrefix
	if hostname, ok := fields.get(fieldHostname); ok && shallPrint(hostname) {
		prefix.hostname, prefix.hasHostname = hostname, true
	}
	prefix.identifier, prefix.hasIdentifier = fields.firstPrintable(identifierFields[:])
	prefix.pid, prefix.hasPID = fields.firstPrintable(pidFields[:])
	return prefix
}

// counted is systemd's n: the stamp and the hostname, identifier and pid
// it printed, in bytes (logs-show.c:629, 639, 657, 660, 666, 669).
// " unknown" is printed without being counted (logs-show.c:662).
func (p linePrefix) counted() int {
	n := stampLen
	if p.hasHostname {
		n += 1 + len(p.hostname)
	}
	if p.hasIdentifier {
		n += 1 + len(p.identifier)
	}
	if p.hasPID {
		n += 2 + len(p.pid)
	}
	return n
}

// indent is the number of spaces before each line of the message after
// the first: n + 2, the 2 being ": " (logs-show.c:739). After " unknown"
// it is 8 bytes short of the visible prefix, as in journalctl.
func (p linePrefix) indent() int {
	return p.counted() + len(": ")
}

// length is the number of bytes from the start of the stamp to the
// message.
func (p linePrefix) length() int {
	n := p.counted() + len(": ")
	if !p.hasIdentifier {
		n += len(unknownIdentifier)
	}
	return n
}

// appendTo appends the hostname, identifier, pid and ": " that follow the
// stamp (logs-show.c:637-672).
func (p linePrefix) appendTo(dst []byte) []byte {
	if p.hasHostname {
		dst = append(dst, ' ')
		dst = append(dst, p.hostname...)
	}
	if p.hasIdentifier {
		dst = append(dst, ' ')
		dst = append(dst, p.identifier...)
	} else {
		dst = append(dst, unknownIdentifier...)
	}
	if p.hasPID {
		dst = append(dst, '[')
		dst = append(dst, p.pid...)
		dst = append(dst, ']')
	}
	return append(dst, ": "...)
}

// maxBlobLineLen is the longest blob line: format_bytes never prints more
// than seven characters ("1023.9K") for a size that fits in an int.
const maxBlobLineLen = len("[1023.9K blob data]\n")

// messageRoom is an upper bound of the bytes appendMessage writes for
// message. Stripping makes a tab 8 bytes and nothing else longer, and it
// keeps every newline; each newline but a final one gains indent spaces,
// and a final newline may be added. A blob line takes at most
// maxBlobLineLen bytes.
func messageRoom(message []byte, indent int) int {
	tabs := bytes.Count(message, []byte{'\t'})
	newlines := bytes.Count(message, []byte{'\n'})
	return max(len(message)+7*tabs+indent*newlines+1, maxBlobLineLen)
}

// appendMessage appends the message part of the text: the message
// stripped of tabs, color and OSC sequences and carriage returns; then,
// when the stripped bytes are not printable UTF-8, a blob line in its
// place (logs-show.c:704-705), and otherwise the stripped message with
// every line after the first indented (logs-show.c:739).
//
// The stripped message is written straight into dst and tested there,
// so no buffer is needed beside dst; a blob replaces it by cutting dst
// back to where the message started.
func appendMessage(dst, message []byte, indent int) []byte {
	start := len(dst)
	dst = appendStripped(dst, message)
	if !utf8IsPrintable(dst[start:]) {
		size := len(dst) - start // the size after stripping
		return appendBlobLine(dst[:start], size)
	}
	return indentLines(dst, start, indent)
}

// appendBlobLine appends "[SIZE blob data]" and a newline, the line an
// unprintable message prints (logs-show.c:704-705).
func appendBlobLine(dst []byte, size int) []byte {
	dst = append(dst, '[')
	dst = appendFormatBytes(dst, uint64(size)) // #nosec G115 -- a length is never negative
	return append(dst, " blob data]\n"...)
}

// indentLines turns the printable message in dst[start:] into the lines
// print_multiline writes in full-width mode (logs-show.c:213-319): the
// message is split on '\n', the first piece follows ": ", every later
// piece gets indent spaces before it, and every piece ends with '\n'.
// An empty message prints "\n" (logs-show.c:240-241), and a final '\n'
// ends the last piece instead of starting an empty one, since the loop
// stops at the end of the message (logs-show.c:243-245).
//
// In bytes: a '\n' is added when the message does not end with one, and
// then every '\n' but the last is followed by indent spaces. The spaces
// are inserted in place, moving each piece right from the last piece to
// the first, so that every byte moves once and nothing is allocated
// beyond the room AppendText reserved.
func indentLines(dst []byte, start, indent int) []byte {
	if len(dst) == start || dst[len(dst)-1] != '\n' {
		dst = append(dst, '\n')
	}
	inner := bytes.Count(dst[start:], []byte{'\n'}) - 1 // the newlines followed by more text
	if inner == 0 {
		return dst
	}
	end := len(dst)
	// grow changes nothing here when AppendText's reservation holds; it
	// keeps this function correct on its own. Reslicing dst up to its new
	// length makes the reserved bytes addressable.
	dst = grow(dst, inner*indent)
	dst = dst[:end+inner*indent]

	// read is the end of the bytes not yet moved, write the end of the
	// bytes not yet written; write - read is always the room still to
	// fill, (newlines left) * indent, so a move never overwrites a byte
	// that has not moved yet.
	read, write := end, len(dst)
	for searchEnd := end - 1; inner > 0; inner-- {
		newline := start + bytes.LastIndexByte(dst[start:searchEnd], '\n')
		// copy works like memmove: the source and the destination may
		// overlap, as they do here.
		write -= copy(dst[write-(read-newline-1):write], dst[newline+1:read])
		write -= indent
		fillSpaces(dst[write : write+indent])
		read, searchEnd = newline+1, newline
	}
	return dst
}

// grow returns dst with room to append at least n more bytes without a
// new backing array. When dst lacks the room it makes one new array, of
// at least twice the old capacity, so that a buffer many entries are
// appended to is copied a logarithmic number of times, as with append.
// slices.Grow does the same in a normal build, but under the race
// detector it also allocates a temporary of n bytes, which would double
// what a long message costs in the race-enabled test run.
func grow(dst []byte, n int) []byte {
	if cap(dst)-len(dst) >= n {
		return dst
	}
	grown := make([]byte, len(dst), max(len(dst)+n, 2*cap(dst)))
	copy(grown, dst)
	return grown
}

// fillSpaces sets every byte of b to a space.
func fillSpaces(b []byte) {
	for i := range b {
		b[i] = ' '
	}
}
