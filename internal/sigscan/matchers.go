package sigscan

import (
	"bytes"
	"regexp"
	"unicode/utf8"
)

// The sizes of a finding's excerpt and of a masked value.
const (
	// excerptBytes is the most bytes of the first line an excerpt
	// keeps; it is cut on a UTF-8 boundary at or below this.
	excerptBytes = 200
	// maskKeepChars is how many characters of a redacted value stay
	// readable at its start.
	maskKeepChars = 4
	// maskMaxStars is the most '*' that replace the rest of a redacted
	// value.
	maskMaxStars = 12
)

// regexMatcher confirms a `regex` signature on one line view: the anchor
// finds a match, `not` finds none, and the value of the first match
// (its capture group) passes `entropy` and `validate`.
type regexMatcher struct {
	re, not  *regexp.Regexp
	capture  int
	entropy  *float64
	validate func([]byte) bool
}

// needsValue reports whether confirming needs the span of the first
// match, not only whether there is one.
func (m *regexMatcher) needsValue() bool {
	return m.capture > 0 || m.entropy != nil || m.validate != nil
}

// confirm tests view. ok says the line is a finding; when wantSpan is
// true (or the checks need it), vs and ve are the value's bytes in the
// view, -1 when the capture group took no part in the first match.
//
// Go note: Match never allocates; FindSubmatchIndex returns a new slice
// of positions, so it runs only on a line that needs the span.
func (m *regexMatcher) confirm(view []byte, wantSpan bool) (ok bool, vs, ve int) {
	if !wantSpan && !m.needsValue() {
		if !m.re.Match(view) || (m.not != nil && m.not.Match(view)) {
			return false, -1, -1
		}
		return true, -1, -1
	}
	loc := m.re.FindSubmatchIndex(view)
	if loc == nil || (m.not != nil && m.not.Match(view)) {
		return false, -1, -1
	}
	vs, ve = loc[2*m.capture], loc[2*m.capture+1]
	var value []byte
	if vs >= 0 {
		value = view[vs:ve]
	}
	if m.entropy != nil && shannonEntropy(value) < *m.entropy {
		return false, -1, -1
	}
	if m.validate != nil && (vs < 0 || !m.validate(value)) {
		return false, -1, -1
	}
	return true, vs, ve
}

// anchorSpan returns the bytes of the first match of re in view, or
// -1, -1 when there is none.
func anchorSpan(re *regexp.Regexp, view []byte) (int, int) {
	loc := re.FindIndex(view)
	if loc == nil {
		return -1, -1
	}
	return loc[0], loc[1]
}

// maskValue returns the masked form of a redacted value: its first
// maskKeepChars characters, then one '*' for each further character,
// at most maskMaxStars. A byte that is not UTF-8 counts as one
// character.
func maskValue(value []byte) []byte {
	keep := 0
	for chars := 0; keep < len(value) && chars < maskKeepChars; chars++ {
		_, size := utf8.DecodeRune(value[keep:])
		keep += size
	}
	stars := min(utf8.RuneCount(value[keep:]), maskMaxStars)
	out := make([]byte, 0, keep+stars)
	out = append(out, value[:keep]...)
	return append(out, bytes.Repeat([]byte{'*'}, stars)...)
}

// appendExcerpt appends the excerpt of a line view to dst: the view
// with every occurrence of value replaced by its masked form (value nil
// or empty: nothing masked), cut to excerptBytes on a UTF-8 boundary.
//
// Masking comes first, so the cut never leaves part of an unmasked
// value; and the work is bounded by the excerpt: an occurrence that
// starts past the bytes the excerpt can hold is never looked for.
//
// SECURITY: the unmasked value is never copied into dst; a redacting
// signature's finding carries only this excerpt.
func appendExcerpt(dst, view, value []byte) []byte {
	start := len(dst)
	// want is a few bytes more than the excerpt, so the cut can find a
	// UTF-8 boundary.
	want := excerptBytes + utf8.UTFMax
	if len(value) == 0 {
		dst = append(dst, view[:min(len(view), want)]...)
		return cutOnRuneBoundary(dst, start)
	}
	masked := maskValue(value)
	for i := 0; i < len(view); {
		room := want - (len(dst) - start)
		if room <= 0 {
			break
		}
		// An occurrence that can still reach the excerpt starts within
		// room bytes of i.
		window := view[i:min(len(view), i+room+len(value)-1)]
		j := bytes.Index(window, value)
		if j < 0 || j >= room {
			dst = append(dst, view[i:min(len(view), i+room)]...)
			break
		}
		dst = append(dst, view[i:i+j]...)
		dst = append(dst, masked...)
		i += j + len(value)
	}
	return cutOnRuneBoundary(dst, start)
}

// cutOnRuneBoundary cuts dst[start:] to at most excerptBytes, backing
// up to the start of a UTF-8 character when the cut falls inside one.
// Bytes that are not UTF-8 are cut at excerptBytes.
func cutOnRuneBoundary(dst []byte, start int) []byte {
	if len(dst)-start <= excerptBytes {
		return dst
	}
	cut := start + excerptBytes
	for k := cut; k > cut-utf8.UTFMax && k > start; k-- {
		if utf8.RuneStart(dst[k]) {
			return dst[:k]
		}
	}
	return dst[:cut]
}
