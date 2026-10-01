package sigscan

import (
	"bytes"
	"cmp"
	"regexp"
	"slices"
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

// span is a byte range [start, end) of a line view.
type span [2]int

// maxCopiedValues bounds how many distinct masked values redactSpans
// looks for copies of on one line.
const maxCopiedValues = 8

// redactSpans returns what a redacting signature masks on a line view,
// sorted, without overlaps:
//
//   - for every match of re, its group `capture` when the group took
//     part in the match, else the whole match: masked by position;
//   - every other copy of such a value on the line (a password echoed
//     later without its `password=`), for the first maxCopiedValues
//     values longer than the characters a mask keeps.
//
// Spans that overlap are joined into one, which masks a little more
// than each would alone and never leaves part of a value visible.
//
// Only the matches that can reach the excerpt are taken: every match
// after the first adds at least one byte to the excerpt (its own first
// character, or the byte before it, since two matches never touch at
// an empty one), so at most excerptBytes + utf8.UTFMax + 1 of them can
// show. Copies are looked for in the whole view, at most
// maxCopiedValues times.
//
// Go note: FindAllSubmatchIndex returns, for each match, the start and
// end of the match and then of each group, -1 for a group that took no
// part.
func redactSpans(re *regexp.Regexp, capture int, view []byte) []span {
	matches := re.FindAllSubmatchIndex(view, excerptBytes+utf8.UTFMax+1)
	spans := make([]span, 0, len(matches))
	for _, loc := range matches {
		if vs := loc[2*capture]; vs >= 0 {
			spans = append(spans, span{vs, loc[2*capture+1]})
			continue
		}
		spans = append(spans, span{loc[0], loc[1]})
	}
	return joinSpans(append(spans, valueCopies(view, spans)...))
}

// valueCopies returns every occurrence on view of the values under
// spans (overlapping ones included), for the first maxCopiedValues
// distinct values a mask changes.
func valueCopies(view []byte, spans []span) []span {
	var copies []span
	seen := map[string]bool{}
	for _, sp := range spans {
		value := view[sp[0]:sp[1]]
		if utf8.RuneCount(value) <= maskKeepChars || seen[string(value)] {
			continue
		}
		if len(seen) == maxCopiedValues {
			break
		}
		seen[string(value)] = true
		for from := 0; from < len(view); {
			i := bytes.Index(view[from:], value)
			if i < 0 {
				break
			}
			copies = append(copies, span{from + i, from + i + len(value)})
			from += i + 1
		}
	}
	return copies
}

// joinSpans sorts spans and joins those that overlap.
func joinSpans(spans []span) []span {
	slices.SortFunc(spans, func(a, b span) int { return cmp.Compare(a[0], b[0]) })
	out := spans[:0]
	for _, sp := range spans {
		if n := len(out); n > 0 && sp[0] < out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], sp[1])
			continue
		}
		out = append(out, sp)
	}
	return out
}

// appendExcerpt appends the excerpt of a line view to dst: the view
// with each of masks (sorted, from redactSpans) replaced by its masked
// form, cut to excerptBytes on a UTF-8 boundary. Masking goes by
// position, so a copy of a value elsewhere on the line that is not a
// match stays as it is, and an overlapping copy cannot shift a mask.
//
// Masking comes first, so the cut never leaves part of an unmasked
// value, and the work is bounded by the excerpt: the view is copied only
// until the excerpt is full.
//
// SECURITY: the bytes of a masked span are never copied into dst
// beyond the characters its mask keeps; a redacting signature's finding
// carries only this excerpt.
func appendExcerpt(dst, view []byte, masks []span) []byte {
	start := len(dst)
	// want is a few bytes more than the excerpt, so the cut can find a
	// UTF-8 boundary.
	want := excerptBytes + utf8.UTFMax
	pos := 0
	for _, m := range masks {
		from := max(m[0], pos)
		if from >= m[1] {
			continue // empty, or inside the span masked before it
		}
		room := want - (len(dst) - start)
		if room <= 0 {
			break
		}
		dst = append(dst, view[pos:min(from, pos+room)]...)
		if from-pos >= room {
			pos = from
			break
		}
		dst = append(dst, maskValue(view[from:m[1]])...)
		pos = m[1]
	}
	if room := want - (len(dst) - start); room > 0 && pos < len(view) {
		dst = append(dst, view[pos:min(len(view), pos+room)]...)
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
