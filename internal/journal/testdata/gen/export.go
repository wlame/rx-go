package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// field is one FIELD=value item of a journal entry.
type field struct {
	name  string
	value []byte
}

// entry is one entry of an export stream: the times and the boot the
// journal keeps in the entry itself, and its fields in order.
type entry struct {
	realtime  uint64 // microseconds since the epoch
	monotonic uint64 // microseconds since the boot started
	bootID    string // 32 lowercase hex digits
	fields    []field
}

// The names the export format gives the entry's own times and boot. The
// writer sends them as text lines, the only form journal-remote reads
// them in; the parser takes them out of the field list.
const (
	realtimeName  = "__REALTIME_TIMESTAMP"
	monotonicName = "__MONOTONIC_TIMESTAMP"
	bootIDName    = "_BOOT_ID"
)

// text makes a field from a string.
func text(name, value string) field { return field{name: name, value: []byte(value)} }

// writeExport writes entries in the journal export format
// (https://systemd.io/JOURNAL_EXPORT_FORMATS/): per entry, one item per
// line, and an empty line after the entry.
//
// An item is "NAME=value\n" when the value holds no newline and no other
// control byte, else the binary form: "NAME\n", the value's size as a
// little-endian uint64, the value, "\n". An empty value is always written
// as "NAME=\n", because journal-remote drops the binary form of an empty
// value.
func writeExport(w io.Writer, entries []entry) error {
	var buf bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&buf, "%s=%d\n%s=%d\n%s=%s\n",
			realtimeName, e.realtime, monotonicName, e.monotonic, bootIDName, e.bootID)
		for _, f := range e.fields {
			appendItem(&buf, f)
		}
		buf.WriteByte('\n')
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// appendItem writes one field to buf in the form writeExport describes.
func appendItem(buf *bytes.Buffer, f field) {
	if !needsBinaryForm(f.value) {
		buf.WriteString(f.name)
		buf.WriteByte('=')
		buf.Write(f.value)
		buf.WriteByte('\n')
		return
	}
	buf.WriteString(f.name)
	buf.WriteByte('\n')
	// encoding/binary turns the integer into its 8 bytes, lowest first,
	// which is the byte order the export format defines for the size.
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(f.value)))
	buf.Write(size[:])
	buf.Write(f.value)
	buf.WriteByte('\n')
}

// needsBinaryForm reports whether a value must be written in the binary
// form: it holds a newline or another control byte. The empty value is
// the exception writeExport describes.
func needsBinaryForm(value []byte) bool {
	for _, b := range value {
		if b < 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}

// parseExport reads an export stream, such as the output of
// `journalctl -o export`, which lists each entry's fields in the order
// the journal stores them. The entry's times and boot go to their own
// members; other names that start with "__" (__CURSOR, __SEQNUM, …) are
// skipped.
func parseExport(data []byte) ([]entry, error) {
	var entries []entry
	var current *entry
	for pos := 0; pos < len(data); {
		end := bytes.IndexByte(data[pos:], '\n')
		if end < 0 {
			return nil, fmt.Errorf("export stream: the line at byte %d has no end", pos)
		}
		line := data[pos : pos+end]
		pos += end + 1
		if len(line) == 0 {
			if current != nil {
				entries = append(entries, *current)
				current = nil
			}
			continue
		}
		if current == nil {
			current = &entry{}
		}
		name, value, isText := bytes.Cut(line, []byte("="))
		if !isText {
			var err error
			value, pos, err = binaryValue(data, pos)
			if err != nil {
				return nil, fmt.Errorf("export stream, field %s: %w", name, err)
			}
		}
		if err := current.take(string(name), value); err != nil {
			return nil, err
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}
	return entries, nil
}

// binaryValue reads the size, the value and the closing newline of a
// binary-form item whose name line ended just before pos, and returns the
// value and the position after it.
func binaryValue(data []byte, pos int) ([]byte, int, error) {
	if len(data)-pos < 8 {
		return nil, 0, fmt.Errorf("the size at byte %d is cut off", pos)
	}
	size := binary.LittleEndian.Uint64(data[pos:])
	pos += 8
	// The size is compared with what is left before it is used, so a
	// wrong size cannot make the slice below reach past the data. The
	// value and its closing newline must both fit.
	remaining := uint64(len(data) - pos) // #nosec G115 -- pos never passes len(data)
	if size >= remaining {
		return nil, 0, fmt.Errorf("a value of %d bytes at byte %d is cut off", size, pos)
	}
	end := pos + int(size) // #nosec G115 -- size is below remaining, which came from an int
	if data[end] != '\n' {
		return nil, 0, fmt.Errorf("no newline after the value that ends at byte %d", end)
	}
	return data[pos:end], end + 1, nil
}

// take records one parsed item in e.
func (e *entry) take(name string, value []byte) error {
	var err error
	switch {
	case name == realtimeName:
		e.realtime, err = strconv.ParseUint(string(value), 10, 64)
	case name == monotonicName:
		e.monotonic, err = strconv.ParseUint(string(value), 10, 64)
	case name == bootIDName:
		e.bootID = string(value)
	case strings.HasPrefix(name, "__"):
		// Another property of the entry (its cursor, its sequence
		// number), not one of its fields.
	default:
		e.fields = append(e.fields, field{name: name, value: bytes.Clone(value)})
	}
	if err != nil {
		return fmt.Errorf("export stream, %s: %w", name, err)
	}
	return nil
}

// hasField reports whether e has a field called name.
func (e entry) hasField(name string) bool {
	for _, f := range e.fields {
		if f.name == name {
			return true
		}
	}
	return false
}
