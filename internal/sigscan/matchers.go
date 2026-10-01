package sigscan

import (
	"regexp"
	"unicode/utf8"
)

// The sizes of a finding's excerpt and of a masked value.
const (
	// excerptBytes is the most bytes of the first line an excerpt
	// keeps; it is cut on a UTF-8 boundary at or below this.
	excerptBytes = 200
	// maskKeepChars is the most characters of a redacted value that stay
	// readable at its start (a quarter of a shorter value's).
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

// keepChars is how many characters of a value of n characters its mask
// keeps: a quarter of them, at most maskKeepChars (20 characters keep
// 4, 8 keep 2, 4 keep 1, 3 keep none).
func keepChars(n int) int {
	return min(n/4, maskKeepChars)
}

// keptBytes is how many bytes at the start of value its mask keeps: its
// first keepChars characters. A byte that is not UTF-8 counts as one
// character.
func keptBytes(value []byte) int {
	keep := keepChars(utf8.RuneCount(value))
	n := 0
	for range keep {
		_, size := utf8.DecodeRune(value[n:])
		n += size
	}
	return n
}

// appendExcerpt appends the excerpt of a line view to dst, nothing
// masked: the view cut to excerptBytes on a UTF-8 boundary. A redacting
// signature's excerpt is appendRedactedExcerpt's.
func appendExcerpt(dst, view []byte) []byte {
	start := len(dst)
	dst = append(dst, view[:min(len(view), excerptBytes+utf8.UTFMax)]...)
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
