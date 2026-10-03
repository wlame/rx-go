package sigscan

import (
	"cmp"
	"slices"
	"unicode/utf8"

	"github.com/wlame/rx-go/internal/signatures"
)

// LineMasker masks whole lines of a file the way a scan masks the
// excerpts of its findings, for the text of a finding read at answer
// time (its lines, and the lines around it): every value a redacting
// signature matches on the line is masked by the same redaction
// function, under the same bounds, failing closed past them. A line
// masked and cut to 200 bytes is the excerpt a scan gives it.
//
// A line can be longer than the line view the scan matched (the
// scan's LineBytes, RX_SIGNATURES_LINE_BYTES). Such a line is matched
// twice, on the view and on the whole text it is given, and the values
// of both are masked: the first finds every value the scan's view
// matched (an anchor that ends at the view's end matches there and
// nowhere else), the second the values past the view, which no scan
// saw.
//
// A LineMasker is built once for many lines and is used by one
// goroutine at a time: it keeps its gate counters and buffers between
// lines, so a line costs no allocation beyond its result.
type LineMasker struct {
	set       redactorSet
	viewBytes int
	gs        *gateScratch
	values    []span
	scratch   []byte
	// steps counts the redaction's automaton steps over every line
	// masked, for the work-bound test.
	steps int
}

// NewLineMasker compiles redactors for masking lines: every signature
// with `redact: true` and something to mask (an anchor, or the values
// of an entropy or base64-run measure); the others are left out, so the
// caller passes the set a scan's Options.Redactors takes. No redactors
// give a masker that returns every line as it is (valid UTF-8, cut).
// viewBytes is the line view of the scan
// whose findings the lines belong to; 0 or less means
// DefaultLineBytes. The error is the keyword automaton's, refused as a
// scan refuses it.
func NewLineMasker(redactors []*signatures.Signature, viewBytes int) (*LineMasker, error) {
	set, err := compileRedactors(redactors)
	if err != nil {
		return nil, err
	}
	if viewBytes <= 0 {
		viewBytes = DefaultLineBytes
	}
	return &LineMasker{set: set, viewBytes: viewBytes, gs: set.newScratch()}, nil
}

// MaskLine returns text, one line without its line ending, with every
// value a redacting signature matches on it masked, then each byte
// that is not part of a valid UTF-8 character replaced by U+FFFD, cut
// to at most limit bytes on a character boundary. masked reports
// whether a mask is in the result.
//
// Masking comes before the cut, so a value that starts before the cut
// and ends after it is masked as a whole: a caller that cuts lines
// gives text longer than limit (the cut plus a margin) and lets
// MaskLine cut it.
//
// Work is linear in len(text): one pass of the redactors' keyword
// automaton and one RE2 search per redactor whose gate passes, then
// the redaction's at most 2 × 8 × (len(text) + 512) steps within its
// bounds (2 × 64 × 256 + 4 × len(text) past them), twice for a line
// longer than the view.
func (m *LineMasker) MaskLine(text []byte, limit int) (string, bool) {
	limit = max(limit, 0)
	m.values = m.values[:0]
	view := text[:min(len(text), m.viewBytes)]
	m.values = m.set.appendLineValues(m.values, view, m.gs)
	if len(text) > len(view) {
		m.values = m.set.appendLineValues(m.values, text, m.gs)
	}
	if len(m.values) == 0 {
		out, _ := appendValidText(nil, text[:min(len(text), limit+utf8.UTFMax)], limit)
		return string(out), false
	}
	// The view's values and the whole line's repeat each other where
	// both passes saw them; a repeated span is one match, so it is
	// kept once and does not count twice toward the match bound.
	slices.SortStableFunc(m.values, func(a, b span) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	})
	m.values = slices.Compact(m.values)
	r := redaction{view: text, values: m.values}
	m.scratch = r.appendTo(m.scratch[:0], limit+utf8.UTFMax)
	m.steps += r.steps
	out, used := appendValidText(nil, m.scratch, limit)
	return string(out), r.masked && r.firstMask < used
}
