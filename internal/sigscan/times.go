package sigscan

import (
	"fmt"
	"io"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/timestamps"
)

// Times in a scan.
//
// A scan reads the timestamps of the file's lines exactly as an index
// build does: the format comes from index.DetectTimeFormat, which reads
// the head of the text with the index's own function, and each line's
// own timestamp from index.LineStamp with a parser built from that
// format and the file's mtime (a format that writes no year takes it
// from the mtime). So a line's time in a finding equals the time the
// index records for that line. No stored index is read: the answer is
// the same whether one exists or not.
//
// Times are kept in the file's own frame, as the index keeps them:
// milliseconds since the Unix epoch, the UTC instant for a file whose
// timestamps carry zones, the wall clock read as UTC for one whose
// timestamps carry none. Reading them in another zone is the answer's
// business, not the scan's.
//
// Every finding gets a time (Finding.TimeMS): the own timestamp of its
// first line, or else the own timestamp of the nearest earlier line
// that has one, when that line starts at most Options.LookbackBytes
// before the finding's first line (the line_timestamps rule of
// `rx samples`). Otherwise, or in a file without timestamps, it has
// none.
//
// Across chunk edges: a chunk's walk knows only the stamps of its own
// lines. A finding whose first line has no stamped line before it in
// its chunk is left pending (timePending); the chunk reports its last
// stamped line, and the merge, which takes the chunks in text order,
// carries the last stamp of the text so far and settles the pending
// findings by the same distance rule. A stamped line seen in the chunk
// but too far back decides "none" at once: any earlier one is farther.

// timeSource reads the own timestamp of one line of a scanned file. It
// is built once per scan and only read afterwards, so every goroutine
// shares it (a timestamps.Parser holds no per-line state).
type timeSource struct {
	parser *timestamps.Parser
	// lookback is how far back, in bytes of text, a line without a
	// timestamp of its own takes one from an earlier line.
	lookback int64
}

// detectTimes decides the timestamp format of the text in r (size bytes
// of a file of kind kind) with the index's own detection, and returns
// the timeSource of a file with that format and mtime, or nil when the
// text has no format rx recognizes. The error is the read's: the head
// could not be read or decoded.
func detectTimes(r io.ReaderAt, size int64, kind filekind.Kind, mtimeNs int64, lookback int) (*timeSource, error) {
	format, ok, err := index.DetectTimeFormat(r, size, kind)
	if err != nil {
		return nil, fmt.Errorf("timestamp format: %w", err)
	}
	if !ok {
		return nil, nil
	}
	parser, err := timestamps.NewParser(format, mtimeNs)
	if err != nil {
		return nil, fmt.Errorf("timestamp format: %w", err)
	}
	return &timeSource{parser: parser, lookback: int64(max(lookback, 0))}, nil
}

// stampOf returns the own timestamp of line l (milliseconds in the
// file's frame), the zone offset the line writes (minutes east of UTC:
// 0 for a line that writes none, Z, UTC, GMT or an epoch value) and
// whether it has one.
//
// The index parses each line as it is in the text, its line ending
// stripped. The view differs from that in one place only: a byte-order
// mark at the start of the text is not in line 1's view. The parser
// skips such a mark itself (timestamps.Parser.Locate), so the view and
// the line the index reads give one answer. The parser looks at the
// first timestamps.WindowBytes bytes of a line only, so a view (at least
// 1 KiB) always holds what it reads.
//
// It allocates nothing: every line is parsed in place.
func (t *timeSource) stampOf(l *line) (int64, int, bool) {
	s, ok := index.LineStamp(t.parser, l.view)
	return s.Ms, s.OffsetMinutes, ok
}

// stampRef is a line with a timestamp of its own: its time, the zone
// offset it writes (minutes east of UTC) and where the line starts in
// the text. ok is false for "no such line".
type stampRef struct {
	ms, start int64
	off       int
	ok        bool
}

// timeState says what is known of a finding's time.
type timeState uint8

const (
	// timeNone: the finding has no time.
	timeNone timeState = iota
	// timeKnown: the finding's time is timeRef.ms.
	timeKnown
	// timePending: no stamped line comes before the finding's first
	// line in its chunk; the merge decides from the stamps before the
	// chunk.
	timePending
)

// timeRef is a finding's time as a walk knows it.
type timeRef struct {
	ms int64
	// off is the zone offset the stamped line the time comes from
	// writes, in minutes east of UTC: what turns ms, an instant in a file
	// whose timestamps carry zones, back into the wall clock the line
	// shows (an answer read in another zone needs it).
	off   int
	state timeState
}

// timeTracker follows the stamped lines of one walk, in order, and
// gives each finding the time of its first line. One goroutine owns a
// tracker.
type timeTracker struct {
	// src is the file's timeSource; nil when the file has no timestamp
	// format, and then every finding's time is none.
	src *timeSource
	// last is the last stamped line at or before the current line.
	last stampRef
	// settled says last is the true nearest stamped line before the
	// walk's lines, as in the merge's replay, which starts from the
	// stamps of the whole text before the chunk. A chunk's own walk is
	// not settled: before its first stamped line, a finding is pending.
	settled bool
}

// observe reads the own timestamp of line l, records it on the line for
// the measures (l.stamped, l.ms), and makes l the last stamped line
// when it has one. It is called for every line, in order, before any
// finding of the line is made, in a file with a timestamp format: a
// line comes from the splitter with stamped false, which is the answer
// in a file without one.
func (t *timeTracker) observe(l *line) {
	if ms, off, ok := t.src.stampOf(l); ok {
		l.stamped, l.ms, l.off = true, ms, off
		t.last = stampRef{ms: ms, start: l.start, off: off, ok: true}
	}
}

// at returns the time of a finding whose first line starts at start:
// the last stamped line's time when it starts at most the lookback
// before (the line itself, at distance 0, when it is stamped).
func (t *timeTracker) at(start int64) timeRef {
	if t == nil || t.src == nil {
		return timeRef{}
	}
	if !t.last.ok {
		if t.settled {
			return timeRef{}
		}
		return timeRef{state: timePending}
	}
	return within(t.last, start, t.src.lookback)
}

// within returns the time a stamped line gives a finding whose first
// line starts at start: its own when it starts at most lookback bytes
// before, none otherwise.
func within(stamp stampRef, start, lookback int64) timeRef {
	if !stamp.ok || start-stamp.start > lookback {
		return timeRef{state: timeNone}
	}
	return timeRef{ms: stamp.ms, off: stamp.off, state: timeKnown}
}

// settle decides a pending time from the last stamped line of the text
// before the chunk (carry); a time already decided is left as it is.
func (r *timeRef) settle(start int64, carry stampRef, lookback int64) {
	if r.state == timePending {
		*r = within(carry, start, lookback)
	}
}

// pointer returns the time as Finding.TimeMS holds it: nil unless
// known.
func (r timeRef) pointer() *int64 {
	if r.state != timeKnown {
		return nil
	}
	ms := r.ms
	return &ms
}

// offsetMinutes returns the offset as Finding.TimeOffsetMinutes holds
// it: 0 unless the time is known.
func (r timeRef) offsetMinutes() int {
	if r.state != timeKnown {
		return 0
	}
	return r.off
}
