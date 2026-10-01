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
// masked: the view as valid UTF-8, cut to excerptBytes on a character
// boundary (appendValidText). A line with secrets on it is masked first
// (appendRedactedExcerpt).
func appendExcerpt(dst, view []byte) []byte {
	out, _ := appendValidText(dst, view[:min(len(view), excerptBytes+utf8.UTFMax)], excerptBytes)
	return out
}

// replacementChar is U+FFFD in UTF-8: what a byte that is not part of a
// valid UTF-8 character becomes in an excerpt.
var replacementChar = []byte(string(utf8.RuneError))

// appendValidText appends text to dst as valid UTF-8, at most limit
// bytes of it (excerptBytes for an excerpt): each byte that is not part
// of a valid UTF-8 character becomes U+FFFD, and the cut falls on a
// character boundary, never inside a character or a replacement. It
// returns the result and how many bytes of text went into it. So every
// carrier of an excerpt or a line (an answer, the cache, JSON) holds
// the same string: JSON would otherwise replace such bytes in some of
// them only.
//
// text holds at least limit + utf8.UTFMax bytes, or all there is: a
// replacement never takes less room than the byte it replaces, so the
// result is full before a character that the end of text cuts short
// could be read as invalid.
func appendValidText(dst, text []byte, limit int) ([]byte, int) {
	start, i := len(dst), 0
	for i < len(text) {
		r, size := utf8.DecodeRune(text[i:])
		piece := text[i : i+size]
		if r == utf8.RuneError && size == 1 {
			piece = replacementChar
		}
		if len(dst)-start+len(piece) > limit {
			break
		}
		dst = append(dst, piece...)
		i += size
	}
	return dst, i
}
