package index

import (
	"fmt"
	"io"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/seekableindex"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// backwardStepMs is how far below the running maximum a timestamp must
// be to count as a backward step. Applications writing to one file can
// disagree by a second without the file being out of order.
const backwardStepMs = 1000

// DetectTimeFormat decides the timestamp format of the open file r,
// size bytes long, whose Kind is kind, from the first
// timestamps.SampleBytes of its text (decompressed for a compressed
// file). It returns false when the file has no format it recognizes.
//
// Every reader that needs a file's format calls this one function, an
// index build and a lookup without an index alike, so both read the
// same bytes and agree on the format by construction. It reads at most
// SampleBytes of a plain file; a compressed file's decoder may read a
// little further (filekind.ReadTextHead). The error is the read's: an
// I/O error or a stream damaged within the head.
//
// Go note: r is read by position (io.ReaderAt), so the read position of
// an *os.File passed as r is left where it was, and a walk that reads
// the same handle afterwards starts at its beginning.
func DetectTimeFormat(r io.ReaderAt, size int64, kind filekind.Kind) (timestamps.Format, bool, error) {
	head, err := filekind.ReadTextHead(r, size, kind, timestamps.SampleBytes)
	if err != nil {
		return timestamps.Format{}, false, fmt.Errorf("read the head of the text: %w", err)
	}
	format, ok := timestamps.Detect(head)
	return format, ok, nil
}

// LineStamp returns the own timestamp of one line of a file's text, as
// every reader of a time index must read it: line is the line as read,
// its line break included or not, and the parser sees it without its
// trailing \r and \n bytes. An index build and a search without an
// index both read a line's timestamp through this function, so they
// agree on it by construction.
//
// It allocates nothing (see timestamps.Parser.Own).
func LineStamp(p *timestamps.Parser, line []byte) (timestamps.Stamp, bool) {
	return p.Own(stripLineEnd(line))
}

// timeIndexer collects the time section of an index while a walk reads
// the file's text. The walk calls markBefore when it is about to read a
// line that a checkpoint names, and observe for every line, in order.
//
// It is not safe for use by several goroutines: one walk owns it.
type timeIndexer struct {
	parser *timestamps.Parser

	// hasMax and maxMs are the running maximum of the own timestamps
	// observed so far.
	hasMax bool
	maxMs  int64

	lines         int64
	first, last   rxtypes.TimePoint
	firstOffset   int
	backwardSteps int64
	maxBackwardMs int64

	// marks is the running maximum before each line markBefore named,
	// in line order. A checkpoint's max_before is looked up here by its
	// line number when the walk ends.
	marks []maxMark

	// frames, when set, marks the lines a seekable file's checkpoints
	// can name (see frameMarks).
	frames *frameMarks
}

// maxMark is the running maximum of the own timestamps of every line
// numbered below line; known is false when none of them had one.
type maxMark struct {
	line  int64
	max   int64
	known bool
}

// newTimeIndexer returns the indexer for a file whose timestamps have
// format f and whose mtime is mtimeNs (the source_mtime_ns the index
// records, so a rebuild of the same file reads the same years).
func newTimeIndexer(f timestamps.Format, mtimeNs int64) (*timeIndexer, error) {
	parser, err := timestamps.NewParser(f, mtimeNs)
	if err != nil {
		return nil, err
	}
	return &timeIndexer{parser: parser}, nil
}

// useFrames makes the indexer mark the lines a seekable file's
// checkpoints can name, given the frames of its seek table.
func (t *timeIndexer) useFrames(frames []seekable.FrameInfo) {
	t.frames = &frameMarks{frames: frames, firstLines: make([]int64, len(frames))}
}

// markBefore records the running maximum before line, which the walk
// has not observed yet. A second mark for the same line adds nothing:
// the maximum before a line is a property of the line.
func (t *timeIndexer) markBefore(line int64) {
	if n := len(t.marks); n > 0 && t.marks[n-1].line == line {
		return
	}
	t.marks = append(t.marks, maxMark{line: line, max: t.maxMs, known: t.hasMax})
}

// observe reads the own timestamp of one line: number is its 1-based
// line number, and [start, end) the bytes it takes in the text, its
// newline included.
//
// It runs on every line of every index build, so it allocates nothing
// (Parser.Own looks at most timestamps.WindowBytes of the line and
// returns a struct by value) and does a few comparisons beside the
// parse. markBefore appends to a slice, but only for a checkpoint's
// line, which is one line in many thousands.
func (t *timeIndexer) observe(line []byte, number, start, end int64) {
	if t.frames != nil {
		t.frames.markLine(t, number, start, end)
	}
	stamp, ok := LineStamp(t.parser, line)
	if !ok {
		return
	}
	t.lines++
	point := rxtypes.TimePoint{Ms: stamp.Ms, Line: number, Offset: start}
	if t.lines == 1 {
		t.first = point
		t.firstOffset = stamp.OffsetMinutes
	}
	t.last = point
	if !t.hasMax {
		t.hasMax, t.maxMs = true, stamp.Ms
		return
	}
	if behind := t.maxMs - stamp.Ms; behind > backwardStepMs {
		t.backwardSteps++
		t.maxBackwardMs = max(t.maxBackwardMs, behind)
	}
	t.maxMs = max(t.maxMs, stamp.Ms)
}

// result builds the time section once the walk has read every line.
// checkpoints is the index's line_index: max_before gets one entry per
// checkpoint, the running maximum before the checkpoint's line.
//
// A checkpoint whose line was never marked is an error rather than a
// guess: the latest timestamp before a checkpoint is what a time search
// trusts to skip everything before it, so a wrong value would make it
// answer with the wrong line.
func (t *timeIndexer) result(checkpoints []rxtypes.LineIndexEntry) (*rxtypes.TimeIndex, error) {
	maxBefore, err := alignMarks(checkpoints, t.marks)
	if err != nil {
		return nil, err
	}
	format := t.parser.Format()
	out := &rxtypes.TimeIndex{
		Format:           string(format.Family),
		Anchored:         format.Anchored,
		DayFirst:         format.DayFirst,
		HasZone:          format.HasZone,
		YearFromMtime:    format.YearFromMtime(),
		TimestampedLines: t.lines,
		BackwardSteps:    t.backwardSteps,
		MaxBackwardMs:    t.maxBackwardMs,
		MaxBefore:        maxBefore,
	}
	if t.lines > 0 {
		first, last := t.first, t.last
		out.First, out.Last = &first, &last
		if format.HasZone {
			offset := t.firstOffset
			out.FirstZoneOffsetMinutes = &offset
		}
	}
	return out, nil
}

// alignMarks returns, for each checkpoint, the mark of its line. Both
// lists are in line order, and either can name one line several times
// (frames inside one long line all name that line), so the mark cursor
// moves only past lines below the checkpoint's, never past an equal
// one. That makes this one pass over both lists.
func alignMarks(checkpoints []rxtypes.LineIndexEntry, marks []maxMark) ([]*int64, error) {
	out := make([]*int64, len(checkpoints))
	j := 0
	for i, cp := range checkpoints {
		for j < len(marks) && marks[j].line < cp.LineNumber {
			j++
		}
		if j == len(marks) || marks[j].line != cp.LineNumber {
			return nil, fmt.Errorf("time index: checkpoint %d names line %d, which the walk did not mark",
				i, cp.LineNumber)
		}
		if marks[j].known {
			v := marks[j].max
			out[i] = &v
		}
	}
	return out, nil
}

// frameMarks marks, during the walk of a seekable file's text, every
// line one of its checkpoints can name. seekableindex places those
// checkpoints while it decodes the frames, in another goroutine, so the
// walk cannot ask it which lines they are; instead it marks a superset
// from the seek table alone:
//
//   - the line that holds the first byte of each frame that has any
//     text (the frame's own checkpoint); and
//   - every line that starts inside a frame and lies a multiple of
//     seekableindex.CheckpointLineInterval lines after that frame's
//     first line (where the interior checkpoints of a frame with many
//     lines go).
//
// A mark that no checkpoint names is never looked up, so the superset
// costs one mark per frame and per 10,000 lines.
type frameMarks struct {
	frames []seekable.FrameInfo
	// firstLines[k] is the line holding frame k's first byte, filled
	// in as the walk reaches it.
	firstLines []int64
	// next is the first frame whose first byte no line has held yet.
	next int
	// current is the frame holding the start of the line being read.
	current int
}

// markLine marks the line [start, end), numbered number, on t when a
// seekable checkpoint can name it. The walk calls it for every line
// before the line's own timestamp is observed.
func (f *frameMarks) markLine(t *timeIndexer, number, start, end int64) {
	// Frames begin in text order, and every frame that began before
	// this line was handled by an earlier line, so each frame is
	// visited once over the whole walk.
	for f.next < len(f.frames) && f.frames[f.next].DecompressedOffset < end {
		f.firstLines[f.next] = number
		if f.frames[f.next].DecompressedSize > 0 {
			t.markBefore(number)
		}
		f.next++
	}
	// The frame holding this line's start. Frames of no text hold no
	// byte, so they are passed over.
	for f.current < len(f.frames) && start >= f.frames[f.current].DecompressedEnd() {
		f.current++
	}
	if f.current == len(f.frames) {
		return
	}
	if after := number - f.firstLines[f.current]; after > 0 && after%seekableindex.CheckpointLineInterval == 0 {
		t.markBefore(number)
	}
}

// maxZoneOffsetMinutes is the largest zone offset a timestamp can
// carry, east or west of UTC: 18 hours, the bound RFC 3339 readers and
// Go's time package accept.
const maxZoneOffsetMinutes = 18 * 60

// validTimeIndex reports why a stored time section cannot be used, or
// nil when it can: its format must be one this build can parse, its
// first and last lines must be lines of the file, its zone offset a
// real one, and max_before must have one entry per checkpoint and never
// decrease. A time search trusts all of these, so an index that breaks
// one is treated as damaged.
func validTimeIndex(idx *rxtypes.UnifiedFileIndex) error {
	ti := idx.TimeIndex
	if ti == nil {
		return nil
	}
	format := timestamps.Format{
		Family:   timestamps.Family(ti.Format),
		Anchored: ti.Anchored,
		DayFirst: ti.DayFirst,
		HasZone:  ti.HasZone,
	}
	if err := format.Validate(); err != nil {
		return fmt.Errorf("time_index: %w", err)
	}
	if err := validTimeSpan(ti, idx.LineCount); err != nil {
		return fmt.Errorf("time_index: %w", err)
	}
	if offset := ti.FirstZoneOffsetMinutes; offset != nil &&
		(*offset < -maxZoneOffsetMinutes || *offset > maxZoneOffsetMinutes) {
		return fmt.Errorf("time_index: first_zone_offset_minutes %d is beyond 18 hours", *offset)
	}
	if len(ti.MaxBefore) != len(idx.LineIndex) {
		return fmt.Errorf("time_index: max_before has %d entries for %d checkpoints",
			len(ti.MaxBefore), len(idx.LineIndex))
	}
	var previous *int64
	for i, v := range ti.MaxBefore {
		if previous != nil && (v == nil || *v < *previous) {
			return fmt.Errorf("time_index: max_before decreases at entry %d", i)
		}
		if v != nil {
			previous = v
		}
	}
	return nil
}

// validTimeSpan checks the count of timestamped lines and the first and
// last of them against lineCount, the index's count of lines: a
// negative count is refused, first and last are present exactly when
// the count is above zero, and both name lines between 1 and lineCount,
// first no later than last.
func validTimeSpan(ti *rxtypes.TimeIndex, lineCount *int64) error {
	if ti.TimestampedLines < 0 {
		return fmt.Errorf("timestamped_lines is %d", ti.TimestampedLines)
	}
	hasLines := ti.TimestampedLines > 0
	if (ti.First != nil) != hasLines || (ti.Last != nil) != hasLines {
		return fmt.Errorf("first and last do not match %d timestamped lines", ti.TimestampedLines)
	}
	if !hasLines {
		return nil
	}
	lines := int64(0)
	if lineCount != nil {
		lines = *lineCount
	}
	for name, point := range map[string]*rxtypes.TimePoint{"first": ti.First, "last": ti.Last} {
		if point.Line < 1 || point.Line > lines {
			return fmt.Errorf("%s names line %d of a file of %d lines", name, point.Line, lines)
		}
	}
	if ti.First.Line > ti.Last.Line {
		return fmt.Errorf("first names line %d, after last's line %d", ti.First.Line, ti.Last.Line)
	}
	return nil
}
