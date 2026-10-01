package sigscan

import (
	"bytes"
	"math"
)

// utf8BOM is the UTF-8 byte-order mark. At the start of the text it is
// not part of line 1's view; offsets still count it.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// lineFunc receives one line of the text (see line); the line is
// borrowed, valid only during the call. It returns false to stop the
// walk.
type lineFunc func(l *line) bool

// noLimit is a splitter limit that never stops it.
const noLimit = math.MaxInt64

// splitter cuts a stream of text into lines and calls a lineFunc for
// each. The text is pushed in pieces of any size; a line that spans
// pieces is carried as its first bytes (enough for its view) and its
// length, never whole, so a line of any length costs at most the view
// size in memory.
type splitter struct {
	lineBytes int
	fn        lineFunc
	// pos is the text offset of the next byte pushed.
	pos int64
	// limit stops the walk before a line that starts after it: a chunk
	// owns the lines that start at or before its nominal end.
	limit int64
	// The partial line: where it starts, its first bytes (at most keep),
	// its length so far, and its last byte (for "\r\n").
	partialStart int64
	partial      []byte
	partialLen   int64
	partialLast  byte
	keep         int
	stopped      bool
	// cur is the line handed to fn, reused from line to line.
	cur line
}

// newSplitter returns a splitter whose first byte is at text offset
// pos. lineBytes is the view size.
func newSplitter(lineBytes int, pos, limit int64, fn lineFunc) *splitter {
	keep := lineBytes + len(utf8BOM)
	return &splitter{lineBytes: lineBytes, fn: fn, pos: pos, limit: limit, keep: keep, partial: make([]byte, 0, keep)}
}

// push feeds the next piece of text. It returns false once the walk
// stopped: the line function returned false, or a line starts after the
// limit.
func (s *splitter) push(data []byte) bool {
	for len(data) > 0 {
		if s.stopped {
			return false
		}
		if s.partialLen == 0 && s.pos > s.limit {
			s.stopped = true
			return false
		}
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			s.addPartial(data)
			return true
		}
		if s.partialLen == 0 {
			start := s.pos
			s.pos += int64(i) + 1
			s.emit(data[:i], int64(i), lastByte(data[:i]), start, true)
		} else {
			s.addPartial(data[:i])
			s.pos++ // the '\n'
			s.emit(s.partial, s.partialLen, s.partialLast, s.partialStart, true)
			s.partial, s.partialLen = s.partial[:0], 0
		}
		data = data[i+1:]
	}
	return !s.stopped
}

// finish emits a last line that has no line break: the text ended in
// the middle of a line.
func (s *splitter) finish() {
	if s.partialLen > 0 && !s.stopped {
		s.emit(s.partial, s.partialLen, s.partialLast, s.partialStart, false)
		s.partial, s.partialLen = s.partial[:0], 0
	}
}

// addPartial adds b to the partial line, keeping only its first bytes.
func (s *splitter) addPartial(b []byte) {
	if len(b) == 0 {
		return
	}
	if s.partialLen == 0 {
		s.partialStart = s.pos
	}
	if room := s.keep - len(s.partial); room > 0 {
		s.partial = append(s.partial, b[:min(room, len(b))]...)
	}
	s.partialLen += int64(len(b))
	s.partialLast = b[len(b)-1]
	s.pos += int64(len(b))
}

// emit builds the view of one line and calls the line function. content
// holds the line's first bytes (all of them when it is short), length
// its full length without the "\n", last its last byte; ended says the
// line ends with "\n" (a last line without one keeps a final "\r").
func (s *splitter) emit(content []byte, length int64, last byte, start int64, ended bool) {
	view, viewLength, shift := lineView(content, length, last, start, ended, s.lineBytes)
	next := start + length
	if ended {
		next++
	}
	s.cur = line{view: view, length: viewLength, start: start, next: next, shift: shift}
	if !s.fn(&s.cur) {
		s.stopped = true
	}
}

// lineView returns the view of a line, the length of the line it is
// cut from, and how many bytes of the line come before the view: the
// "\r" of a "\r\n" ending dropped, a byte-order mark at text offset 0
// dropped, then the first lineBytes bytes. content holds the line's
// first bytes (at least lineBytes + 3 of them when the line is longer);
// length and last describe the whole line without its "\n".
func lineView(content []byte, length int64, last byte, start int64, ended bool, lineBytes int) ([]byte, int64, int) {
	if ended && length > 0 && last == '\r' {
		length--
		if int64(len(content)) > length {
			content = content[:length]
		}
	}
	shift := 0
	if start == 0 && length >= int64(len(utf8BOM)) && bytes.HasPrefix(content, utf8BOM) {
		content = content[len(utf8BOM):]
		length -= int64(len(utf8BOM))
		shift = len(utf8BOM)
	}
	if len(content) > lineBytes {
		content = content[:lineBytes]
	}
	return content, length, shift
}

// lastByte returns the last byte of b, or 0 for an empty b.
func lastByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[len(b)-1]
}

// isBlank reports whether a view is empty or holds only spaces and
// tabs.
func isBlank(view []byte) bool {
	for _, c := range view {
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}
