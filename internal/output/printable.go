package output

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Printable returns s with every byte a terminal would act on rather
// than show written out as an escape: the C0 controls (ESC, carriage
// return, newline, tab and the rest), DEL, the C1 controls, and any byte
// that is not part of valid UTF-8 (a lone 0x9b is CSI to a terminal in
// 8-bit mode). ESC becomes `\x1b`, U+009B `\u009b`, an invalid byte
// `\xff`; everything else is kept as it is.
//
// SECURITY: a file name, a path or a reason is not rx's to trust: a
// file named "\x1b[2J…" printed raw clears the screen of whoever lists
// it, or rewrites the line it is on. Every such string the human output
// prints goes through Printable. The text of a matched or sampled line
// does not: it is the file's content the user asked to see, printed as
// it is, as ripgrep and grep print it.
func Printable(s string) string {
	return escapeControls(s, false)
}

// PrintableMessage is Printable for a message of several lines: a
// newline and a tab are kept, every other control is escaped. It is for
// an error message, which can hold a path and can span lines (a regex
// error with its caret line).
func PrintableMessage(s string) string {
	return escapeControls(s, true)
}

// escapeControls does Printable's work; keepLines keeps "\n" and "\t".
// It returns s itself when nothing needs escaping, which is the common
// case.
func escapeControls(s string, keepLines bool) string {
	if !needsEscape(s, keepLines) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case escapes(r, size, keepLines):
			if r < 0x80 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// needsEscape reports whether s holds anything escapeControls changes.
func needsEscape(s string, keepLines bool) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if escapes(r, size, keepLines) {
			return true
		}
		i += size
	}
	return false
}

// escapes reports whether the rune r, size bytes of the text, is
// written out: an invalid byte, or a control that keepLines does not
// keep (it keeps only a newline and a tab).
func escapes(r rune, size int, keepLines bool) bool {
	if r == utf8.RuneError && size == 1 {
		return true
	}
	if keepLines && (r == '\n' || r == '\t') {
		return false
	}
	return isControl(r)
}

// isControl reports whether r is a C0 control, DEL or a C1 control.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}
