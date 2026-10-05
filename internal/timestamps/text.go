package timestamps

import "strings"

// MaxTextBytes is the longest text Text returns.
const MaxTextBytes = 64

// hexDigits spells the escape of a byte that is not printable ASCII.
const hexDigits = "0123456789abcdef"

// Text returns the timestamp written on line, as the line writes it, and
// false when the line has none. It is the span Locate finds, with every
// byte that is not printable ASCII written as `\xHH` (a backslash too,
// so the text reads back unambiguously), cut to at most MaxTextBytes
// bytes and never inside an escape.
//
// A client shows times in the layout of a file's own lines from it, so
// it must be safe to print as it is: the matchers accept only ASCII
// letters, digits and punctuation today, and the escaping keeps that
// true for any matcher added later.
//
// It allocates the returned string, so it is for a line or two per
// file, not for the per-line path of an index build.
func (p *Parser) Text(line []byte) (string, bool) {
	_, span, ok := p.Locate(line)
	if !ok {
		return "", false
	}
	return escapeText(line[span.Start:span.End]), true
}

// escapeText writes b as Text describes: printable ASCII other than a
// backslash as it is, every other byte as `\xHH`, stopping before the
// text would pass MaxTextBytes.
func escapeText(b []byte) string {
	var out strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f && c != '\\' {
			if out.Len()+1 > MaxTextBytes {
				break
			}
			out.WriteByte(c)
			continue
		}
		if out.Len()+4 > MaxTextBytes {
			break
		}
		out.WriteString(`\x`)
		out.WriteByte(hexDigits[c>>4])
		out.WriteByte(hexDigits[c&0x0f])
	}
	return out.String()
}
