package sigscan

import (
	"bytes"
	"regexp"
	"unicode/utf8"
)

// Redaction masks every secret of a redacting signature on a line: the
// value of every match of its anchor, and every other copy of such a
// value on the line, so a secret echoed later without its `key=` is
// masked too. It fails closed: when a line is beyond what the masking
// can follow in linear time, the line is masked from its first secret
// on rather than risk showing one.
//
// The masking works on the values, not on the matches: every match's
// value is an occurrence of one of the line's distinct values, so
// masking every occurrence of the values masks every match and every
// copy at once. Occurrences are found by one KMP scan per value over
// the line, in order, overlapping ones joined into one masked run as
// they come; the text is written out run by run until it has the
// length the caller wants (the excerpt, or a whole line).
//
// The bounds, and what passing one does:
//
//   - more than maxRedactMatches matches, or more than maxRedactSeen
//     distinct values: some values are not known, so the whole line is
//     masked;
//   - more than maxRedactValues distinct values, or a value longer than
//     maxRedactValueBytes: the line shows only its text before the
//     earliest occurrence of any value, at most failClosedPrefixBytes,
//     and is masked from there.
//
// Work: at most 2 × maxRedactValues × (len(view) + maxRedactValueBytes)
// automaton steps when the line is within the bounds, and at most
// 2 × maxRedactSeen × failClosedPrefixBytes + 4 × len(view) when it is
// not: linear in the line.
const (
	maxRedactMatches      = 4096
	maxRedactSeen         = 64
	maxRedactValues       = 8
	maxRedactValueBytes   = 256
	failClosedPrefixBytes = 256
)

// span is a byte range [start, end) of a line view.
type span [2]int

// redactMatches returns the value of every match of re on view, in
// order: its group `capture` when the group took part in the match,
// else the whole match. At most maxRedactMatches + 1 are returned; more
// than maxRedactMatches means the line has more.
//
// Go note: FindAllSubmatchIndex returns, for each match, the start and
// end of the match and then of each group, -1 for a group that took no
// part.
func redactMatches(re *regexp.Regexp, capture int, view []byte) []span {
	matches := re.FindAllSubmatchIndex(view, maxRedactMatches+1)
	values := make([]span, 0, len(matches))
	for _, loc := range matches {
		if vs := loc[2*capture]; vs >= 0 {
			values = append(values, span{vs, loc[2*capture+1]})
			continue
		}
		values = append(values, span{loc[0], loc[1]})
	}
	return values
}

// appendRedactedExcerpt appends the excerpt of a line with secrets on
// it: the view masked as redaction describes, cut to excerptBytes on a
// UTF-8 boundary. values are the line's match values (redactMatches of
// every redacting signature, in order). masked reports whether a mask is
// in the excerpt.
func appendRedactedExcerpt(dst, view []byte, values []span) (out []byte, masked bool) {
	start := len(dst)
	r := redaction{view: view, values: values}
	out = cutOnRuneBoundary(r.appendTo(dst, excerptBytes+utf8.UTFMax), start)
	return out, r.masked && start+r.firstMask < len(out)
}

// redaction masks one line. It is a pure function of the view and the
// match values; steps counts the automaton steps, for the work bound,
// and masked says a mask was written, the first at firstMask in the
// output.
type redaction struct {
	view      []byte
	values    []span
	steps     int
	masked    bool
	firstMask int
}

// maskedRun is a run of the view to mask: [start, end), of which the
// bytes before keepEnd stay readable (the characters the masks of the
// values in it keep).
type maskedRun struct {
	start, end, keepEnd int
}

// appendTo appends the masked view to dst, stopping once it has
// appended at least want bytes (the caller cuts).
func (r *redaction) appendTo(dst []byte, want int) []byte {
	distinct, firstStart, tooMany := r.distinctValues()
	switch {
	case len(distinct) == 0:
		return append(dst, r.view[:min(len(r.view), want)]...)
	case tooMany:
		return r.appendRuns(dst, want, []maskedRun{{start: 0, end: len(r.view)}})
	case !withinBounds(distinct):
		return r.appendFailClosed(dst, want, distinct, firstStart)
	}
	scans := make([]*copyScan, len(distinct))
	for i, v := range distinct {
		scans[i] = newCopyScan(r.view, v, len(r.view), &r.steps)
	}
	return r.appendScans(dst, want, scans)
}

// distinctValues returns the line's distinct non-empty values, in the
// order they first match, and where the first of them starts. tooMany
// says the line has more matches or distinct values than can be
// followed.
func (r *redaction) distinctValues() (distinct [][]byte, firstStart int, tooMany bool) {
	if len(r.values) > maxRedactMatches {
		return [][]byte{nil}, 0, true
	}
	seen := map[string]bool{}
	firstStart = -1
	for _, sp := range r.values {
		v := r.view[sp[0]:sp[1]]
		if len(v) == 0 || seen[string(v)] {
			continue
		}
		if firstStart < 0 {
			firstStart = sp[0]
		}
		if len(seen) == maxRedactSeen {
			return distinct, firstStart, true
		}
		seen[string(v)] = true
		distinct = append(distinct, v)
	}
	return distinct, firstStart, false
}

// withinBounds reports whether every copy of the values can be followed
// along the whole line.
func withinBounds(distinct [][]byte) bool {
	if len(distinct) > maxRedactValues {
		return false
	}
	for _, v := range distinct {
		if len(v) > maxRedactValueBytes {
			return false
		}
	}
	return true
}

// appendFailClosed masks a line beyond the bounds: it shows the text
// before the earliest occurrence of any value (the first match, or a
// copy before it), at most failClosedPrefixBytes, and masks the rest,
// keeping none of it readable.
func (r *redaction) appendFailClosed(dst []byte, want int, distinct [][]byte, firstStart int) []byte {
	cut := min(firstStart, failClosedPrefixBytes)
	for _, v := range distinct {
		end := min(len(r.view), cut+len(v)-1)
		if s, ok := newCopyScan(r.view, v, end, &r.steps).nextOccurrence(); ok {
			cut = min(cut, s)
		}
	}
	return r.appendRuns(dst, want, []maskedRun{{start: cut, end: len(r.view), keepEnd: cut}})
}

// appendScans writes the view with the runs of the scans masked, in
// order: the scan whose next run starts first is taken, and a run that
// overlaps the one before is joined to it.
func (r *redaction) appendScans(dst []byte, want int, scans []*copyScan) []byte {
	view := r.view
	start, pos := len(dst), 0
	var cur maskedRun
	has := false
	for len(dst)-start < want {
		next, ok := firstRun(scans)
		if has && (!ok || next.start >= cur.end) {
			dst, pos = r.appendRun(dst, start, pos, cur, want-(len(dst)-start))
			has = false
		}
		if !ok {
			break
		}
		switch {
		case !has:
			cur, has = next, true
		default:
			cur.end = max(cur.end, next.end)
			cur.keepEnd = min(cur.keepEnd, next.keepEnd)
		}
	}
	if room := want - (len(dst) - start); room > 0 && pos < len(view) {
		dst = append(dst, view[pos:min(len(view), pos+room)]...)
	}
	return dst
}

// firstRun takes, from the scans, the masked run that starts first.
func firstRun(scans []*copyScan) (maskedRun, bool) {
	best := -1
	for i, s := range scans {
		if s.peek() && (best < 0 || s.run.start < scans[best].run.start) {
			best = i
		}
	}
	if best < 0 {
		return maskedRun{}, false
	}
	run := scans[best].run
	scans[best].ready = false
	return run, true
}

// appendRuns writes the view with the given runs (sorted, apart)
// masked.
func (r *redaction) appendRuns(dst []byte, want int, runs []maskedRun) []byte {
	view := r.view
	start, pos := len(dst), 0
	for _, run := range runs {
		if len(dst)-start >= want {
			return dst
		}
		dst, pos = r.appendRun(dst, start, pos, run, want-(len(dst)-start))
	}
	if room := want - (len(dst) - start); room > 0 && pos < len(view) {
		dst = append(dst, view[pos:min(len(view), pos+room)]...)
	}
	return dst
}

// appendRun writes the text from pos to the run (at most room bytes),
// then the run masked: its bytes before keepEnd, then one '*' per
// further character, at least one and at most maskMaxStars. It returns
// the position after the run. start is where the output began.
func (r *redaction) appendRun(dst []byte, start, pos int, run maskedRun, room int) ([]byte, int) {
	view := r.view
	if run.start-pos >= room {
		return append(dst, view[pos:pos+room]...), run.start
	}
	dst = append(dst, view[pos:run.start]...)
	if !r.masked {
		r.masked, r.firstMask = true, len(dst)-start
	}
	keepEnd := max(run.start, min(run.keepEnd, run.end-1))
	for keepEnd > run.start && !utf8.RuneStart(view[keepEnd]) {
		keepEnd--
	}
	dst = append(dst, view[run.start:keepEnd]...)
	return append(dst, bytes.Repeat([]byte{'*'}, starsFor(view[keepEnd:run.end]))...), run.end
}

// starsFor is the number of '*' that replace masked: one per character,
// at least one, at most maskMaxStars. It reads at most maskMaxStars
// characters.
func starsFor(masked []byte) int {
	n, i := 0, 0
	for i < len(masked) && n < maskMaxStars {
		_, size := utf8.DecodeRune(masked[i:])
		i += size
		n++
	}
	return max(n, 1)
}

// copyScan finds the occurrences of one value in view[:end], in order,
// with the Knuth-Morris-Pratt automaton: every byte of the view is read
// once, and the automaton falls back at most as many times as it
// advanced, so a scan costs at most 2 × (end + len(value)) steps.
// Occurrences that overlap are joined into one run as they are found;
// no list of occurrences is kept.
type copyScan struct {
	view, value []byte
	end         int
	// fail[i] is the length of the longest proper prefix of value[:i+1]
	// that is also its suffix (the KMP prefix function).
	fail  []int
	kept  int // keptBytes(value)
	i, q  int // the next view byte to read; the matched length of value
	steps *int
	// run is the next run, ready when peek has found it; pending is an
	// occurrence found after it that starts the run after.
	run        maskedRun
	ready      bool
	pending    int
	hasPending bool
	exhausted  bool
}

func newCopyScan(view, value []byte, end int, steps *int) *copyScan {
	s := &copyScan{view: view, value: value, end: end, fail: make([]int, len(value)), kept: keptBytes(value), steps: steps}
	for i, k := 1, 0; i < len(value); i++ {
		for k > 0 && value[i] != value[k] {
			k = s.fail[k-1]
			*steps++
		}
		*steps++
		if value[i] == value[k] {
			k++
		}
		s.fail[i] = k
	}
	return s
}

// nextOccurrence returns the start of the next occurrence of the value.
func (s *copyScan) nextOccurrence() (int, bool) {
	for s.i < s.end {
		c := s.view[s.i]
		s.i++
		for s.q > 0 && s.value[s.q] != c {
			s.q = s.fail[s.q-1]
			*s.steps++
		}
		*s.steps++
		if s.value[s.q] == c {
			s.q++
		}
		if s.q == len(s.value) {
			// Fall back so the next, overlapping occurrence is found too.
			s.q = s.fail[s.q-1]
			return s.i - len(s.value), true
		}
	}
	return 0, false
}

// peek makes the next run ready; false when there is none.
func (s *copyScan) peek() bool {
	if s.ready {
		return true
	}
	if s.exhausted {
		return false
	}
	start, ok := s.pending, s.hasPending
	s.hasPending = false
	if !ok {
		if start, ok = s.nextOccurrence(); !ok {
			s.exhausted = true
			return false
		}
	}
	s.run = maskedRun{start: start, end: start + len(s.value), keepEnd: start + s.kept}
	for {
		next, found := s.nextOccurrence()
		if !found {
			s.exhausted = true
			break
		}
		if next >= s.run.end {
			s.pending, s.hasPending = next, true
			break
		}
		s.run.end = next + len(s.value)
	}
	s.ready = true
	return true
}
