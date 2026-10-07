package index

import (
	"errors"
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
	format, ok, _, err := DetectTimeFormatAndHead(r, size, kind)
	return format, ok, err
}

// DetectTimeFormatAndHead is DetectTimeFormat that also returns the
// head of the text it read (at most timestamps.SampleBytes bytes), for
// a caller that looks for the first timestamp in it without reading the
// head a second time.
func DetectTimeFormatAndHead(r io.ReaderAt, size int64, kind filekind.Kind) (timestamps.Format, bool, []byte, error) {
	head, err := filekind.ReadTextHead(r, size, kind, timestamps.SampleBytes)
	if err != nil {
		return timestamps.Format{}, false, nil, fmt.Errorf("read the head of the text: %w", err)
	}
	format, ok := timestamps.Detect(head)
	return format, ok, head, nil
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
	// hasZone is the parser's Format().HasZone, kept here because
	// Format copies the format and may allocate, which a per-line call
	// must not.
	hasZone bool

	// hasMax and maxAt are the running maximum of the own timestamps
	// observed so far: maxAt.Ms is its value, and maxAt the first line
	// that holds it. Once the walk has read every line, maxAt is the
	// time section's max.
	//
	// maxAt is a struct held by value (three int64s), so moving it to a
	// new line copies 24 bytes and allocates nothing.
	hasMax bool
	maxAt  rxtypes.TimePoint

	lines       int64
	first, last rxtypes.TimePoint
	firstOffset int
	// firstText is the first timestamp as its line writes it.
	firstText     string
	backwardSteps int64
	maxBackwardMs int64

	// marks is the running maximum before each line markBefore named,
	// in line order. A checkpoint's max_before is looked up here by its
	// line number when the walk ends.
	marks []maxMark

	// frames, when set, marks the lines a seekable file's checkpoints
	// can name (see frameMarks).
	frames *frameMarks

	// zones are the change points of the offset the lines write, in line
	// order (see rxtypes.TimeIndex.ZoneOffsets); tooManyZones is set
	// once a change point past MaxZoneOffsets was seen, and zones is no
	// longer added to.
	zones        []rxtypes.ZoneOffset
	tooManyZones bool
}

// MaxZoneOffsets is the most change points of the written zone offset a
// time section records (rxtypes.TimeIndex.ZoneOffsets). A file whose
// offset changes more often records null: a request that reads it in
// another zone then searches it from its first line. The bound keeps
// the section small whatever a file holds, and a search under a file
// zone visits at most this many segments.
const MaxZoneOffsets = 1024

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
	return &timeIndexer{parser: parser, hasZone: f.HasZone}, nil
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
	t.marks = append(t.marks, maxMark{line: line, max: t.maxAt.Ms, known: t.hasMax})
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
//
// The running maximum moves to this line only when its timestamp is
// strictly higher, so of several lines that share the highest value the
// first one stays: max names the earliest line at which the file reaches
// its highest time.
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
		// The one allocation of the time section per build: the first
		// timestamped line is read a second time for its text.
		t.firstText, _ = t.parser.Text(stripLineEnd(line))
	}
	t.last = point
	t.observeZone(number, stamp)
	if !t.hasMax {
		t.hasMax, t.maxAt = true, point
		return
	}
	if behind := t.maxAt.Ms - stamp.Ms; behind > backwardStepMs {
		t.backwardSteps++
		t.maxBackwardMs = max(t.maxBackwardMs, behind)
	}
	t.maxAt = higherPoint(t.maxAt, point)
}

// higherPoint returns next when its value is strictly higher than
// current's, and current otherwise, so of two lines with the same value
// the earlier one stays.
//
// It is written as a choice between two values, with no store inside
// the if, so that the compiler emits conditional moves (CSEL on arm64,
// CMOV on amd64) rather than a branch. That matters on the per-line
// path: in a log that many threads write, such as app.log, about
// 40% of the lines set a new highest millisecond and most of the rest
// repeat it, in an order a branch predictor cannot learn. Written as a
// branch around a store to t.maxAt, the comparison measured about 3% of
// the whole index build of that file.
//
// Go note: TimePoint is three int64s, so it is passed and returned in
// registers and the call is inlined into observe; nothing here touches
// memory beyond the struct observe already holds.
func higherPoint(current, next rxtypes.TimePoint) rxtypes.TimePoint {
	chosen := current
	if next.Ms > current.Ms {
		chosen = next
	}
	return chosen
}

// observeZone records a change point when the timestamped line number
// writes another zone offset than the line before it.
//
// The offset recorded is the one that turns the line's stored value
// back into the wall clock it writes: the offset the line writes in a
// file whose timestamps carry zones (0 for a line that writes none,
// whose value is stored as written), and 0 for every line of a file
// whose timestamps carry none, where a line that writes a zone keeps
// the wall clock it shows (timestamps.Parser). So within one entry's
// lines, stored value plus offset is always the written wall clock.
//
// It allocates only at a change point, and at most MaxZoneOffsets
// times over a build: past the limit it stops recording.
func (t *timeIndexer) observeZone(number int64, stamp timestamps.Stamp) {
	if t.tooManyZones {
		return
	}
	offset := 0
	if t.hasZone {
		offset = stamp.OffsetMinutes
	}
	if n := len(t.zones); n > 0 && t.zones[n-1].OffsetMinutes == offset {
		return
	}
	if len(t.zones) == MaxZoneOffsets {
		t.tooManyZones, t.zones = true, nil
		return
	}
	t.zones = append(t.zones, rxtypes.ZoneOffset{Line: number, OffsetMinutes: offset})
}

// zoneOffsets is the recorded list: nil past the limit, and an empty,
// non-nil list for a file without a timestamped line, which JSON writes
// as [] rather than null.
func (t *timeIndexer) zoneOffsets() []rxtypes.ZoneOffset {
	if t.tooManyZones {
		return nil
	}
	if t.zones == nil {
		return []rxtypes.ZoneOffset{}
	}
	return t.zones
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
		ZoneOffsets:      t.zoneOffsets(),
	}
	// First, Last, Max and FirstText are set together, from the first
	// timestamped line on, so all four are null exactly when no line has
	// an own timestamp (validTimeSpan holds a stored index to the same).
	if t.lines > 0 {
		first, last, highest, text := t.first, t.last, t.maxAt, t.firstText
		out.First, out.Last, out.Max, out.FirstText = &first, &last, &highest, &text
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
// first, last and max lines must be lines of the file, its zone offset a
// real one, max_before must have one entry per checkpoint, never
// decrease and hold values in the years 1 to 9999, and max must agree
// with first, last and max_before (validMax). A time search trusts all
// of these, so an index that breaks one is treated as damaged.
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
	if err := validFirstText(ti); err != nil {
		return fmt.Errorf("time_index: %w", err)
	}
	if offset := ti.FirstZoneOffsetMinutes; offset != nil &&
		(*offset < -maxZoneOffsetMinutes || *offset > maxZoneOffsetMinutes) {
		return fmt.Errorf("time_index: first_zone_offset_minutes %d is beyond 18 hours", *offset)
	}
	if err := validZoneOffsets(ti); err != nil {
		return fmt.Errorf("time_index: %w", err)
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
		if v != nil && !timestamps.InValueRange(*v) {
			return fmt.Errorf("time_index: max_before entry %d holds %d ms, outside the years 1 to 9999", i, *v)
		}
		if v != nil {
			previous = v
		}
	}
	if err := validMax(ti, idx.LineIndex); err != nil {
		return fmt.Errorf("time_index: %w", err)
	}
	return nil
}

// maxBeforeAgreesWithMax checks max_before entry i, v, which belongs to
// the checkpoint on line checkpointLine, against the file's max.
//
// max names the first line that holds the file's highest value, so:
//
//   - a checkpoint after that line has that line before it, and the
//     highest value before it is exactly max's;
//   - a checkpoint on that line or before it has only lines before it
//     whose values are lower than max's (none of them holds the highest
//     value, or max would name it), so its entry is below max's value,
//     or null when none of them has a timestamp.
//
// A search under a time trusts both: the first to start at the last
// checkpoint whose entry is below the time, the second to skip a part
// of a chain whose highest value is below it.
func maxBeforeAgreesWithMax(i int, v *int64, checkpointLine int64, highest *rxtypes.TimePoint) error {
	if checkpointLine > highest.Line {
		if v == nil || *v != highest.Ms {
			return fmt.Errorf("max_before entry %d (checkpoint line %d, after max's line %d) is %s, not max %d ms",
				i, checkpointLine, highest.Line, msOrNull(v), highest.Ms)
		}
		return nil
	}
	if v != nil && *v >= highest.Ms {
		return fmt.Errorf("max_before entry %d (checkpoint line %d, up to max's line %d) holds %d ms, not below max %d ms",
			i, checkpointLine, highest.Line, *v, highest.Ms)
	}
	return nil
}

// msOrNull writes a value of max_before for an error message.
func msOrNull(v *int64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%d ms", *v)
}

// validZoneOffsets checks zone_offsets against the rest of a time
// section that validTimeSpan already accepted. A search under a file
// zone adds each entry's offset to the stored values of its lines, so
// every entry must be one the build could have written:
//
//   - null only for a file whose timestamps carry zones (a file without
//     zones always records [[first, 0]]), and at most MaxZoneOffsets
//     entries, checked before the entries are read;
//   - empty exactly when no line has a timestamp;
//   - the first entry on the first timestamped line, with the offset of
//     the first timestamp (first_zone_offset_minutes, or 0 for a file
//     without zones);
//   - lines strictly ascending, none after the last timestamped line;
//   - each offset within 18 hours, and different from the one before
//     (an entry records a change);
//   - for a file without zones, nothing past the first entry.
func validZoneOffsets(ti *rxtypes.TimeIndex) error {
	points := ti.ZoneOffsets
	if points == nil {
		if !ti.HasZone {
			return errors.New("zone_offsets is null for a file whose timestamps carry no zone")
		}
		return nil
	}
	if len(points) > MaxZoneOffsets {
		return fmt.Errorf("zone_offsets has %d entries, more than %d", len(points), MaxZoneOffsets)
	}
	if (len(points) > 0) != (ti.TimestampedLines > 0) {
		return fmt.Errorf("zone_offsets has %d entries for %d timestamped lines", len(points), ti.TimestampedLines)
	}
	if len(points) == 0 {
		return nil
	}
	firstOffset := 0
	if ti.FirstZoneOffsetMinutes != nil {
		firstOffset = *ti.FirstZoneOffsetMinutes
	}
	if points[0].Line != ti.First.Line || points[0].OffsetMinutes != firstOffset {
		return fmt.Errorf("zone_offsets starts with [%d, %d], not at the first timestamp [%d, %d]",
			points[0].Line, points[0].OffsetMinutes, ti.First.Line, firstOffset)
	}
	if !ti.HasZone && len(points) > 1 {
		return fmt.Errorf("zone_offsets has %d entries for a file whose timestamps carry no zone", len(points))
	}
	for i, p := range points {
		if p.OffsetMinutes < -maxZoneOffsetMinutes || p.OffsetMinutes > maxZoneOffsetMinutes {
			return fmt.Errorf("zone_offsets entry %d: offset %d is beyond 18 hours", i, p.OffsetMinutes)
		}
		if p.Line > ti.Last.Line {
			return fmt.Errorf("zone_offsets entry %d names line %d, after the last timestamped line %d", i, p.Line, ti.Last.Line)
		}
		if i == 0 {
			continue
		}
		if previous := points[i-1]; p.Line <= previous.Line || p.OffsetMinutes == previous.OffsetMinutes {
			return fmt.Errorf("zone_offsets entry %d [%d, %d] does not follow [%d, %d] with a change",
				i, p.Line, p.OffsetMinutes, previous.Line, previous.OffsetMinutes)
		}
	}
	return nil
}

// validFirstText checks first_text: present exactly when first is,
// and what timestamps.Parser.Text gives, at most MaxTextBytes bytes of
// printable ASCII. A client prints it as it is, so a damaged index must
// not hand it control bytes.
func validFirstText(ti *rxtypes.TimeIndex) error {
	if (ti.FirstText != nil) != (ti.First != nil) {
		return errors.New("first_text does not match first")
	}
	if ti.FirstText == nil {
		return nil
	}
	text := *ti.FirstText
	if len(text) > timestamps.MaxTextBytes {
		return fmt.Errorf("first_text is %d bytes, more than %d", len(text), timestamps.MaxTextBytes)
	}
	for i := 0; i < len(text); i++ {
		if text[i] < 0x20 || text[i] >= 0x7f {
			return fmt.Errorf("first_text holds byte %#x, which is not printable ASCII", text[i])
		}
	}
	return nil
}

// validTimeSpan checks the count of timestamped lines and the first,
// last and highest of them against lineCount, the index's count of
// lines: a negative count is refused, first, last and max are present
// exactly when the count is above zero, and all three name lines between
// 1 and lineCount, first no later than last, with values in the years 1
// to 9999 (a search adds a zone offset of up to 18 hours to them, which
// must stay far from the ends of int64). validMax checks max further.
func validTimeSpan(ti *rxtypes.TimeIndex, lineCount *int64) error {
	if ti.TimestampedLines < 0 {
		return fmt.Errorf("timestamped_lines is %d", ti.TimestampedLines)
	}
	hasLines := ti.TimestampedLines > 0
	if (ti.First != nil) != hasLines || (ti.Last != nil) != hasLines || (ti.Max != nil) != hasLines {
		return fmt.Errorf("first, last and max do not match %d timestamped lines", ti.TimestampedLines)
	}
	if !hasLines {
		return nil
	}
	lines := int64(0)
	if lineCount != nil {
		lines = *lineCount
	}
	for name, point := range map[string]*rxtypes.TimePoint{"first": ti.First, "last": ti.Last, "max": ti.Max} {
		if point.Line < 1 || point.Line > lines {
			return fmt.Errorf("%s names line %d of a file of %d lines", name, point.Line, lines)
		}
		if !timestamps.InValueRange(point.Ms) {
			return fmt.Errorf("%s holds %d ms, outside the years 1 to 9999", name, point.Ms)
		}
	}
	if ti.First.Line > ti.Last.Line {
		return fmt.Errorf("first names line %d, after last's line %d", ti.First.Line, ti.Last.Line)
	}
	return nil
}

// validMax checks max, which validTimeSpan has placed on a line of the
// file (or found null with every other point, in a file without
// timestamped lines), against first, last and the checkpoints:
//
//   - max is one of the timestamped lines, so it lies between the first
//     and the last of them, and it holds the highest value, so neither
//     first's nor last's value is above it;
//   - the three offsets are positions in one text, the text's bytes
//     (decompressed for a compressed file, so an offset is never
//     compared with the stored file's size): none is negative, they
//     keep the order of their lines, and two are equal exactly when
//     their lines are (validOffsets);
//   - each checkpoint's max_before agrees with max
//     (maxBeforeAgreesWithMax), and a file without timestamped lines
//     has no value in max_before.
//
// A search by time across several files passes over a file whose max
// comes before the time it looks for, so a max below a value of the
// file, or one that disagrees with the checkpoints a search within the
// file trusts, would skip lines it should find. A reader that seeks to
// a stored offset trusts the offsets.
func validMax(ti *rxtypes.TimeIndex, checkpoints []rxtypes.LineIndexEntry) error {
	highest := ti.Max
	if highest == nil {
		for i, v := range ti.MaxBefore {
			if v != nil {
				return fmt.Errorf("max_before entry %d holds %d ms in a file without timestamped lines", i, *v)
			}
		}
		return nil
	}
	if highest.Line < ti.First.Line || highest.Line > ti.Last.Line {
		return fmt.Errorf("max names line %d, outside the timestamped lines %d to %d",
			highest.Line, ti.First.Line, ti.Last.Line)
	}
	if highest.Ms < ti.First.Ms || highest.Ms < ti.Last.Ms {
		return fmt.Errorf("max holds %d ms, below first's %d ms or last's %d ms",
			highest.Ms, ti.First.Ms, ti.Last.Ms)
	}
	if err := validOffsets(ti.First, highest, ti.Last); err != nil {
		return err
	}
	// validTimeIndex has checked that max_before has one entry per
	// checkpoint, so the two lists are read side by side.
	for i, v := range ti.MaxBefore {
		if err := maxBeforeAgreesWithMax(i, v, checkpoints[i].LineNumber, highest); err != nil {
			return err
		}
	}
	return nil
}

// validOffsets checks the offsets of first, max and last, three points
// whose lines are in that order: first's is not negative, and from one
// point to the next the offset grows when the line does and stays the
// same when the line does, as the starts of lines in one text do.
func validOffsets(first, highest, last *rxtypes.TimePoint) error {
	if first.Offset < 0 {
		return fmt.Errorf("first offset %d is negative", first.Offset)
	}
	for _, pair := range [][2]*rxtypes.TimePoint{{first, highest}, {highest, last}} {
		earlier, later := pair[0], pair[1]
		sameLine := earlier.Line == later.Line
		if sameLine != (earlier.Offset == later.Offset) || earlier.Offset > later.Offset {
			return fmt.Errorf("offsets are not in the order of their lines: line %d at offset %d, line %d at offset %d",
				earlier.Line, earlier.Offset, later.Line, later.Offset)
		}
	}
	return nil
}
