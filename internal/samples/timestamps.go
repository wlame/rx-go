package samples

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sort"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrNoTimeFormat is returned by Resolve for a time query on a file in
// whose first mebibyte no timestamp format is recognized. The caller
// answers it as a usage error, like an invalid query.
var ErrNoTimeFormat = errors.New("no timestamp format recognized in the first 1 MiB")

// ErrTooManyTimestamps is returned by Resolve for a request with more
// than MaxTimestampValues time queries.
var ErrTooManyTimestamps = errors.New("too many timestamp values")

// MaxTimestampValues is the most time queries one request may hold.
// The searches for all of them share their passes (one pass from the
// start without an index; with one, passes that never read a line twice
// for one query, see searchPlan), and their lines are read in one more
// pass, so the bound limits the work per pass, not the number of passes.
const MaxTimestampValues = 1000

// IsUsageError reports whether err, from Resolve, is a mistake in the
// request rather than a failure to read the file: an invalid time
// query, too many of them, or a time query on a file without
// timestamps. The CLI exits 2 on it and the HTTP API answers 400.
func IsUsageError(err error) bool {
	return errors.Is(err, timestamps.ErrInvalidQuery) ||
		errors.Is(err, ErrNoTimeFormat) ||
		errors.Is(err, ErrTooManyTimestamps) ||
		errors.Is(err, ErrTooManyLines) ||
		errors.Is(err, ErrTooManyBytes)
}

// tailStepBytes is how much of a file's text one step of the read back
// from its end covers, looking for the last timestamped line.
const tailStepBytes = 1 << 20

// fileTimes is what the samples answer knows about a file's timestamps:
// its format, the frame the answer reads them in, a parser of that
// frame and, when the file has a line index whose time section can give
// values of that frame, the section, the index's checkpoints and the
// segments that turn the section's values into the frame's.
type fileTimes struct {
	// parser reads each line's own timestamp in frame.
	parser *timestamps.Parser
	// detected is the file's format as detection decided it, which
	// time_format reports whatever the frame.
	detected timestamps.Format
	frame    timeFrame
	// section and checkpoints are the stored index's time_index and
	// line_index; section is nil without an index, and when the
	// section's values cannot be turned into the answer's frame (a file
	// zone on a file whose zone offsets change too often to record).
	section     *rxtypes.TimeIndex
	checkpoints []rxtypes.LineIndexEntry
	// segments turn section's stored values into the answer's frame:
	// one segment of offset 0 when the section holds values of the
	// frame, and the file's zone_offsets under a file zone (see
	// segmentsToFrame). Set exactly when section is.
	segments []zoneSegment
}

// zoneSegment is a run of lines whose stored time values all differ
// from the answer's frame by one offset: from line firstLine on, until
// the next segment, a line's value in the frame is its stored value
// plus offsetMs.
//
// Under a file zone the frame is the wall clock each line writes, and
// an index of a file whose timestamps carry zones stores the instant,
// that wall clock minus the offset the line writes. zone_offsets
// records where that written offset changes, so it gives the segments
// directly.
type zoneSegment struct {
	firstLine int64
	offsetMs  int64
}

// storedFrame is the segments of a section whose values are already in
// the answer's frame: every line, offset 0.
var storedFrame = []zoneSegment{{firstLine: 1, offsetMs: 0}}

// segmentsToFrame returns the segments that turn the values ti stores
// for a file of format detected into frame f, and false when there are
// none: f reads the stored frame (storedFrame), or it is a file zone on
// a file whose timestamps carry zones and ti records where their offset
// changes (one segment per entry of zone_offsets). A null zone_offsets
// (too many changes to record) leaves the stored instants without a way
// back to the written wall clocks.
func segmentsToFrame(f timeFrame, detected timestamps.Format, ti *rxtypes.TimeIndex) ([]zoneSegment, bool) {
	if f.readsStoredFrame(detected) {
		return storedFrame, true
	}
	if ti.ZoneOffsets == nil {
		return nil, false
	}
	segments := make([]zoneSegment, len(ti.ZoneOffsets))
	for i, z := range ti.ZoneOffsets {
		segments[i] = zoneSegment{firstLine: z.Line, offsetMs: int64(z.OffsetMinutes) * msPerMinute}
	}
	return segments, true
}

// frameValueAt turns stored, the value the index stores for line, into
// the answer's frame: plus the offset of the segment that holds the
// line. A line before the first segment has no timestamp, and is given
// the first segment's offset; without segments (a file with no
// timestamped line) the value is returned as it is.
func frameValueAt(segments []zoneSegment, line, stored int64) int64 {
	if len(segments) == 0 {
		return stored
	}
	k := sort.Search(len(segments), func(i int) bool { return segments[i].firstLine > line })
	return stored + segments[max(k-1, 0)].offsetMs
}

// timeFrame is how one answer reads the values of a file's lines: as
// UTC instants (hasZone), or as wall clocks read in zone.
//
// Without a file zone it is the file's own frame: instants for a file
// whose timestamps carry zones, wall clocks read in RX_LOG_TZ for one
// whose timestamps carry none. With a file zone (Request.FileZone) every
// value is the wall clock its line writes, read in that zone, and a
// zone written on a line is ignored.
type timeFrame struct {
	hasZone bool
	zone    config.Zone
}

// frameFor returns the frame req reads a file of format detected in.
func (r Request) frameFor(detected timestamps.Format) timeFrame {
	if r.FileZone.Location != nil {
		return timeFrame{hasZone: false, zone: r.FileZone}
	}
	return timeFrame{hasZone: detected.HasZone, zone: config.LogTZ()}
}

// parserFormat returns the format a Parser reading in f is built with:
// detected, with HasZone set to the frame's. A Parser of a format
// without zones gives a line that writes a zone the wall clock it shows
// (timestamps.Parser keeps every value of such a file in one frame), so
// one per-line path serves the file's own frame and a file zone alike,
// at no cost per line.
func (f timeFrame) parserFormat(detected timestamps.Format) timestamps.Format {
	detected.HasZone = f.hasZone
	return detected
}

// readsStoredFrame reports whether a line index of a file of format
// detected holds its time values in f. An index holds them in the
// file's own frame (instants when detected has zones, wall clocks when
// it has none), so only a file zone on a file whose timestamps carry
// zones reads another: each stored instant gives back the wall clock its
// line wrote only with the offset the line wrote (segmentsToFrame).
func (f timeFrame) readsStoredFrame(detected timestamps.Format) bool {
	return f.hasZone == detected.HasZone
}

// instant returns the UTC instant of fileMs, a value in f, and false
// when it falls outside the years 1 to 9999.
func (f timeFrame) instant(fileMs int64) (int64, bool) {
	return timestamps.InstantOf(fileMs, f.hasZone, f.zone.Location)
}

// newFileTimes returns the fileTimes of a file of format detected read
// in req's frame, without an index section. mtimeNs is the file's mtime,
// which gives a year-less family its year.
func newFileTimes(req Request, detected timestamps.Format, mtimeNs int64) (*fileTimes, error) {
	frame := req.frameFor(detected)
	parser, err := timestamps.NewParser(frame.parserFormat(detected), mtimeNs)
	if err != nil {
		return nil, err
	}
	return &fileTimes{parser: parser, detected: detected, frame: frame}, nil
}

// formatOfSection is the format an index's time section records.
func formatOfSection(ti *rxtypes.TimeIndex) timestamps.Format {
	return timestamps.Format{
		Family: timestamps.Family(ti.Format), Anchored: ti.Anchored,
		DayFirst: ti.DayFirst, HasZone: ti.HasZone,
	}
}

// timesForMode returns timesOf(req, kind) for an answer in req's mode.
//
// A time query cannot be answered without the format, so in timestamps
// mode an error reading the head of the text is the answer's error. In
// the other modes the format only fills time_format: a head that cannot
// be read (a stream damaged within its first mebibyte) leaves it null
// and is logged, and the lookup goes on to answer the lines it can, as
// it did before it reported a format.
func timesForMode(req Request, kind filekind.Kind) (*fileTimes, error) {
	times, err := timesOf(req, kind)
	if err == nil || req.Mode() == TimestampsMode {
		return times, err
	}
	slog.Default().Warn("time_format_not_read", "path", req.Path, "error", err.Error())
	return nil, nil
}

// timesOf returns what req.Source's timestamps are, or nil when the
// file has none recognized. With a line index the format is the index's
// (it read the same head with the same function), and nothing is read;
// without one, the head of the text is read (at most
// timestamps.SampleBytes of text) and the format detected from it.
//
// The index's time section serves the answer when its values can be
// turned into the answer's frame (segmentsToFrame): always without a
// file zone and for a file whose timestamps carry no zone, and under a
// file zone on a file whose timestamps carry zones when the index
// records where their offset changes. When they cannot, the answer
// searches from the first line, as without an index, and still uses the
// index's checkpoints to read the lines it found.
func timesOf(req Request, kind filekind.Kind) (*fileTimes, error) {
	if idx := loadIndexOrNone(req); idx != nil {
		if idx.TimeIndex == nil {
			return nil, nil
		}
		ti := idx.TimeIndex
		detected := formatOfSection(ti)
		// The mtime the index was built with: a syslog line's year
		// comes from it. The index describes the file only while that
		// mtime is the file's, so this is the pin's mtime too.
		times, err := newFileTimes(req, detected, idx.SourceMtimeNs)
		if err != nil {
			return nil, err
		}
		if segments, ok := segmentsToFrame(times.frame, detected, ti); ok {
			times.section, times.checkpoints, times.segments = ti, idx.LineIndex, segments
		}
		return times, nil
	}
	detected, ok, err := detectTimeFormat(req.Source, kind)
	if err != nil || !ok {
		return nil, err
	}
	return newFileTimes(req, detected, req.Source.Info().ModTime().UnixNano())
}

// detectTimeFormat reads the head of src's text through the samples
// seam and decides its format with the one function an index build
// uses, index.DetectTimeFormat.
func detectTimeFormat(src paths.Pinned, kind filekind.Kind) (timestamps.Format, bool, error) {
	f, err := openFileForSamples(src)
	if err != nil {
		return timestamps.Format{}, false, err
	}
	defer func() { _ = f.Close() }()
	file, ok := f.(positionalFile)
	if !ok {
		return timestamps.Format{}, false, fmt.Errorf("detect the timestamp format of %s: the file cannot be read by position", src.Path())
	}
	info, err := file.Stat()
	if err != nil {
		return timestamps.Format{}, false, err
	}
	return index.DetectTimeFormat(file, info.Size(), kind)
}

// positionalFile is an open file read by position, with its size: an
// *os.File, or a test's counting wrapper of one.
type positionalFile interface {
	io.ReaderAt
	Stat() (os.FileInfo, error)
}

// describe returns the time_format member of the answer, or nil for a
// file without timestamps.
func (t *fileTimes) describe() *rxtypes.SamplesTimeFormat {
	if t == nil {
		return nil
	}
	return timeFormatIn(t.detected, t.frame)
}

// lineAt is the answer to one search bound: the first line, in file
// order, whose own timestamp is at least the bound, and that timestamp;
// line is -1 when no line has one.
type lineAt struct {
	line  int64
	stamp timestamps.Stamp
}

// notFound is the lineAt of a bound no line reaches.
var notFound = lineAt{line: -1}

// timeWindow is what one time query asks the lines machinery for: line
// `first` with context (single), or lines first..last (range). key is
// the lines-mode key of that request.
type timeWindow struct {
	value       string
	first, last int64
	isRange     bool
}

// resolveTimestamps answers a timestamps-mode request.
//
// Each query becomes one ordinary line, or a span of lines, by the rule
// "the line at T is the first line, in file order, whose own timestamp
// is T or later" (searchBounds). Those line numbers are then handed to
// the lines-mode machinery (resolveLineWindows), which reads the lines
// and their context exactly as `--lines=N` would: a time query never
// numbers lines or picks context its own way.
func resolveTimestamps(req Request, kind filekind.Kind, times *fileTimes, resp *collected) error {
	if len(req.Timestamps) > MaxTimestampValues {
		return fmt.Errorf("%w: %d given, at most %d per request", ErrTooManyTimestamps, len(req.Timestamps), MaxTimestampValues)
	}
	if times == nil {
		return fmt.Errorf("%w of %s", ErrNoTimeFormat, req.Path)
	}
	text := textSourceFor(req, kind)
	resolved, err := times.resolvedQueries(req, kind, text)
	if err != nil {
		return err
	}
	var bounds []int64
	for _, r := range resolved {
		bounds = append(bounds, searchBoundsOf(r)...)
	}
	found, err := times.searchBounds(req.context(), text, bounds)
	if err != nil {
		return err
	}

	var windows []timeWindow
	for i, r := range resolved {
		value := req.Timestamps[i]
		w, ok := windowFor(value, r, found)
		if !ok {
			// No line at T, or a range with no line in it: the -1 and
			// null a line number past the end of the file answers.
			resp.Timestamps[value] = -1
			resp.Samples[value] = nil
			continue
		}
		windows = append(windows, w)
	}
	return answerTimeWindows(req, kind, windows, resp)
}

// resolvedQueries turns the request's time queries into the file's
// frame: the bounds a caller resolved already (Request.TimeBounds), or
// else each value parsed and resolved against this file.
func (t *fileTimes) resolvedQueries(req Request, kind filekind.Kind, text textSource) ([]timestamps.Resolved, error) {
	if req.TimeBounds != nil {
		return t.boundsOf(req)
	}
	queries := make([]timestamps.Query, len(req.Timestamps))
	for i, value := range req.Timestamps {
		q, err := timestamps.ParseQuery(value, t.parser)
		if err != nil {
			return nil, err
		}
		queries[i] = q
	}
	fileContext, err := t.resolveContext(req, kind, text, queries)
	if err != nil {
		return nil, err
	}
	resolved := make([]timestamps.Resolved, len(queries))
	for i, q := range queries {
		r, err := timestamps.Resolve(q, fileContext)
		if err != nil {
			return nil, err
		}
		resolved[i] = r
	}
	return resolved, nil
}

// searchBoundsOf lists the bounds a resolved query needs searched: the
// start of a single query or range, and for a range's end T2 the first
// line later than T2, which is at least T2+1 at millisecond precision.
func searchBoundsOf(r timestamps.Resolved) []int64 {
	var bounds []int64
	if !r.Start.Open {
		bounds = append(bounds, r.Start.Ms)
	}
	if r.Range && !r.End.Open {
		bounds = append(bounds, r.End.Ms+1)
	}
	return bounds
}

// windowFor turns a resolved query and the searched bounds into the
// lines it asks for, or false when it asks for none.
//
// A range runs from the line at T1 (line 1 for an open start) to the
// line before the first line later than T2 (the last line for an open
// end, or when no line is later). It is empty when no line is at T1, or
// when its end comes before its start, which a file whose writers
// disagree about the time can give.
func windowFor(value string, r timestamps.Resolved, found map[int64]lineAt) (timeWindow, bool) {
	if !r.Range {
		at := found[r.Start.Ms]
		return timeWindow{value: value, first: at.line, last: at.line}, at.line > 0
	}
	first := int64(1)
	if !r.Start.Open {
		first = found[r.Start.Ms].line
	}
	// The end of the text: the lines machinery stops reading there.
	last := int64(math.MaxInt64)
	if !r.End.Open {
		if after := found[r.End.Ms+1]; after.line > 0 {
			last = after.line - 1
		}
	}
	if first < 1 || last < first {
		return timeWindow{}, false
	}
	return timeWindow{value: value, first: first, last: last, isRange: true}, true
}

// answerTimeWindows reads every window through the lines-mode machinery
// in one request and files each answer under its query value.
func answerTimeWindows(req Request, kind filekind.Kind, windows []timeWindow, resp *collected) error {
	if len(windows) == 0 {
		return nil
	}
	lines := make([]OffsetOrRange, 0, len(windows))
	asked := map[string]bool{}
	for _, w := range windows {
		position := w.position()
		if key := position.Key(); !asked[key] {
			asked[key] = true
			lines = append(lines, position)
		}
	}
	sub := req
	sub.Timestamps, sub.Lines = nil, lines
	// The lines are read once per window, under a budget of their own;
	// the answer then files each window's lines under every query that
	// asked for it, and those are taken from the answer's budget, since
	// line_timestamps and the encoded answer hold them once per query.
	answer := newCollected(&rxtypes.SamplesResponse{Offsets: map[string]int64{}, Lines: map[string]int64{}, Samples: map[string][]string{}}, req)
	if err := resolveLineWindows(sub, kind, answer); err != nil {
		return err
	}
	for _, w := range windows {
		sample := answer.Samples[w.position().Key()]
		if len(sample) == 0 {
			// The search read this line, so the lines machinery must
			// find it too; a disagreement is a numbering defect, and
			// answering it would give the wrong line.
			return fmt.Errorf("samples of %s: line %d, found at %q, was not read back", req.Path, w.first, w.value)
		}
		for _, line := range sample {
			if err := resp.budget.take(int64(len(line))); err != nil {
				return err
			}
		}
		resp.Samples[w.value] = sample
		resp.starts[w.value] = answer.starts[w.position().Key()]
		resp.Timestamps[w.value] = w.first
	}
	return nil
}

// position is the lines-mode request of a window.
func (w timeWindow) position() OffsetOrRange {
	if !w.isRange {
		return OffsetOrRange{Start: w.first}
	}
	last := w.last
	return OffsetOrRange{Start: w.first, End: &last}
}

// resolveContext gathers what timestamps.Resolve needs for queries:
// the zones, and the file's first and last timestamps when a query
// needs them. With an index they are stored; without one, the first is
// found by a search from the start and the last by a read back from
// the end (lastStamp), each only when a query needs it.
func (t *fileTimes) resolveContext(req Request, kind filekind.Kind, text textSource, queries []timestamps.Query) (timestamps.ResolveContext, error) {
	c, needsSpan, needsFirst := t.baseContext(queries)
	if !needsSpan && !needsFirst {
		return c, nil
	}
	if t.section != nil {
		if first, last := t.section.First, t.section.Last; first != nil && last != nil {
			c.FirstMs = frameValueAt(t.segments, first.Line, first.Ms)
			c.LastMs = frameValueAt(t.segments, last.Line, last.Ms)
			c.HasSpan = true
		}
		if t.section.FirstZoneOffsetMinutes != nil {
			c.FirstOffsetMinutes = *t.section.FirstZoneOffsetMinutes
		}
		return c, nil
	}

	found, err := t.searchBounds(req.context(), text, []int64{math.MinInt64})
	if err != nil {
		return c, err
	}
	first := found[math.MinInt64]
	if first.line < 0 {
		// No line has a timestamp: there is no span, and no offset.
		return c, nil
	}
	c.FirstOffsetMinutes = first.stamp.OffsetMinutes
	if !needsSpan {
		return c, nil
	}
	last, err := t.lastStamp(req, kind)
	if err != nil {
		return c, err
	}
	c.FirstMs, c.LastMs, c.HasSpan = first.stamp.Ms, last.Ms, true
	return c, nil
}

// hasWallEndpoint reports whether an endpoint of q has no zone.
func hasWallEndpoint(q timestamps.Query) bool {
	for _, e := range [...]timestamps.Endpoint{q.Start, q.End} {
		if e.Kind == timestamps.EndpointWall || e.Kind == timestamps.EndpointTimeOfDay {
			return true
		}
	}
	return false
}

// searchBounds answers, for each bound, the first line in file order
// whose own timestamp is at least the bound (lineAt).
//
// Without an index every bound is answered by one pass from the first
// line. With one, the index says where a bound's line cannot be, and
// each pass starts after that (searchPlan): the answer is the same
// either way, the index only saves reading.
//
// Bounds whose passes start and stop at the same place share one pass.
// A stream compressed file (gzip, bzip2, xz, plain zstd) is decompressed
// from its first byte whatever the checkpoint, so all its bounds share
// one pass, from the earliest start, to the end of the text.
func (t *fileTimes) searchBounds(ctx context.Context, text textSource, bounds []int64) (map[int64]lineAt, error) {
	found := make(map[int64]lineAt, len(bounds))
	var pending []int64
	for _, bound := range bounds {
		if _, seen := found[bound]; !seen {
			found[bound] = notFound
			pending = append(pending, bound)
		}
	}
	if t.section == nil {
		return found, t.searchFrom(ctx, text, wholeText, pending, found)
	}
	_, streamed := text.(streamedText)
	plans := make(map[int64]*searchPlan, len(pending))
	for _, bound := range pending {
		plans[bound] = t.newSearchPlan(bound)
	}
	// Each round runs, for every bound not answered yet, the pass of the
	// next segment that may hold its line. A round moves every plan on
	// by at least one segment, so there are at most as many rounds as
	// segments (index.MaxZoneOffsets).
	for len(pending) > 0 {
		passes := map[searchPass][]int64{}
		for _, bound := range pending {
			if pass, ok := plans[bound].next(); ok {
				passes[pass] = append(passes[pass], bound)
			}
		}
		if streamed {
			passes = mergedPasses(passes)
		}
		if err := t.runPasses(ctx, text, passes, found); err != nil {
			return nil, err
		}
		pending = pending[:0]
		for bound, plan := range plans {
			if found[bound].line < 0 && !plan.done() && !streamed {
				pending = append(pending, bound)
			}
		}
		// The map's order is random; a sorted list keeps the passes of a
		// round, and so the reads, the same from run to run.
		sort.Slice(pending, func(i, j int) bool { return pending[i] < pending[j] })
	}
	return found, nil
}

// searchPass is one pass of a search: from the line start at or before
// byte offset start up to, not including, line stop.
type searchPass struct {
	start int64
	stop  int64
}

// wholeText is the pass over the whole text, from its first line to its
// end.
var wholeText = searchPass{start: 0, stop: math.MaxInt64}

// runPasses runs every pass, in text order, answering the bounds each
// lists.
func (t *fileTimes) runPasses(ctx context.Context, text textSource, passes map[searchPass][]int64, found map[int64]lineAt) error {
	order := make([]searchPass, 0, len(passes))
	for pass := range passes {
		order = append(order, pass)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].start != order[j].start {
			return order[i].start < order[j].start
		}
		return order[i].stop < order[j].stop
	})
	for _, pass := range order {
		if err := t.searchFrom(ctx, text, pass, passes[pass], found); err != nil {
			return err
		}
	}
	return nil
}

// mergedPasses puts every bound in one pass from the earliest start to
// the end of the text. The pass reads each line's own timestamp in the
// answer's frame, so a line it finds for a bound before the bound's own
// start is impossible (the index ruled those lines out), and reading on
// past a segment's end finds what the next segment's pass would.
func mergedPasses(passes map[searchPass][]int64) map[searchPass][]int64 {
	merged := searchPass{start: math.MaxInt64, stop: math.MaxInt64}
	var all []int64
	for pass, bounds := range passes {
		merged.start = min(merged.start, pass.start)
		all = append(all, bounds...)
	}
	if len(all) == 0 {
		return nil
	}
	return map[searchPass][]int64{merged: all}
}

// searchPlan walks one bound through the index's segments, in file
// order, giving the pass of each segment that may hold the bound's line.
//
// Within segment k a line's value in the answer's frame is its stored
// value plus the segment's offset, so it reaches the bound exactly when
// its stored value reaches the bound minus that offset: the segment's
// target. max_before (the latest stored value of every line before a
// checkpoint) then says, as for a file read in its own frame, where the
// segment's lines cannot reach the target:
//
//   - the segment is skipped when the first checkpoint at or after its
//     end has a max_before below the target: no line before that
//     checkpoint, the whole segment included, reaches it;
//   - otherwise its pass starts at the last checkpoint inside the
//     segment whose max_before is below the target, or at the last
//     checkpoint at or before the segment's first line, and stops at the
//     segment's end.
//
// The first segment whose pass finds a line gives the answer: every
// line before it lies in a segment skipped or read through. With one
// segment (a file read in its own frame) this is the search from the
// checkpoint max_before names, to the end of the text.
//
// A segment's pass reads about one index step when the stored values
// rise through the file, as they do across a change of summer time.
// When an earlier segment holds stored values above a later segment's
// (an offset that grows while the wall clock goes back), max_before
// cannot place the later segment's line, and its pass reads from the
// segment's first line, up to the segment's end at most.
//
// One pass serves a run of segments. A pass that stops at the end of
// segment k reads on into segment k+1 instead whenever segment k+1's own
// pass would start at a checkpoint at or before its first line, that is
// inside text this pass reads anyway; the run ends at the first segment
// whose pass would start at a checkpoint inside it. Reading a segment
// the plan would have skipped finds nothing in it (no line there reaches
// the bound), and such a segment holds no checkpoint, so it lies within
// one index step. Without this, every segment of a step that holds many
// offset changes would restart from the step's checkpoint, and a step
// of n segments would be read about n times.
//
// INVARIANT: the passes of one bound never overlap, so a bound reads
// each line of the text at most once (plus a read buffer per pass). A
// run ends before the first segment j whose own pass would start at a
// checkpoint inside it, one at or after its first line whose max_before
// is below j's target; a segment whose checkpoints all have a
// max_before at or above its target is read on in the run. The next
// pass starts at that checkpoint of j, or at the last checkpoint at or
// before a later segment's first line, which is that checkpoint or a
// later one. Either way it starts at or after the line where the run
// stopped.
type searchPlan struct {
	t     *fileTimes
	bound int64
	// segment is the next segment to look at.
	segment int
}

// newSearchPlan returns the plan of bound over t's segments.
func (t *fileTimes) newSearchPlan(bound int64) *searchPlan {
	return &searchPlan{t: t, bound: bound}
}

// done reports whether every segment has been looked at.
func (p *searchPlan) done() bool { return p.segment >= len(p.t.segments) }

// next returns the pass of the next segment that may hold the bound's
// line, run on through the segments after it that the same pass can
// serve, and false when no segment left may hold the line.
func (p *searchPlan) next() (searchPass, bool) {
	t := p.t
	for ; p.segment < len(t.segments); p.segment++ {
		target, first, stop := p.segmentAt(p.segment)
		if t.segmentBelow(target, stop) {
			continue
		}
		start, _ := t.segmentStart(target, first, stop)
		pass := searchPass{start: start, stop: stop}
		// Run on while the next segment's own pass would start at or
		// before its first line, inside the text this pass reads.
		for p.segment++; p.segment < len(t.segments); p.segment++ {
			target, first, stop := p.segmentAt(p.segment)
			if _, inside := t.segmentStart(target, first, stop); inside {
				break
			}
			pass.stop = stop
		}
		return pass, true
	}
	return searchPass{}, false
}

// segmentAt returns segment k's target (the bound minus the segment's
// offset), its first line and the line after its last
// (math.MaxInt64 for the last segment).
func (p *searchPlan) segmentAt(k int) (target, first, stop int64) {
	segments := p.t.segments
	stop = math.MaxInt64
	if k+1 < len(segments) {
		stop = segments[k+1].firstLine
	}
	return minusOffset(p.bound, segments[k].offsetMs), segments[k].firstLine, stop
}

// minusOffset is bound minus offsetMs, held at the int64 range: a bound
// of math.MinInt64 (the first timestamped line) stays the lowest value.
func minusOffset(bound, offsetMs int64) int64 {
	if offsetMs > 0 && bound < math.MinInt64+offsetMs {
		return math.MinInt64
	}
	if offsetMs < 0 && bound > math.MaxInt64+offsetMs {
		return math.MaxInt64
	}
	return bound - offsetMs
}

// segmentBelow reports whether every line before line stop has a stored
// value below target, by the max_before of the first checkpoint at or
// after stop. Without such a checkpoint it cannot tell, and says no.
//
// max_before never decreases, and a null (no earlier timestamp) only
// precedes the values (validTimeIndex refuses anything else), so the
// lists searched here are ordered the way sort.Search needs.
func (t *fileTimes) segmentBelow(target, stop int64) bool {
	i := sort.Search(len(t.checkpoints), func(i int) bool { return t.checkpoints[i].LineNumber >= stop })
	if i == len(t.checkpoints) {
		return false
	}
	before := t.section.MaxBefore[i]
	return before == nil || *before < target
}

// segmentStart is the byte offset a pass for target in the segment of
// lines [first, stop) starts from: the last checkpoint inside the
// segment whose max_before is below target (inside is true), or else the
// last checkpoint at or before line first (0 without one; inside is
// false).
func (t *fileTimes) segmentStart(target, first, stop int64) (start int64, inside bool) {
	cps, maxBefore := t.checkpoints, t.section.MaxBefore
	lo := sort.Search(len(cps), func(i int) bool { return cps[i].LineNumber >= first })
	hi := sort.Search(len(cps), func(i int) bool { return cps[i].LineNumber >= stop })
	reached := lo + sort.Search(hi-lo, func(i int) bool {
		v := maxBefore[lo+i]
		return v != nil && *v >= target
	})
	if reached > lo {
		return cps[reached-1].ByteOffset, true
	}
	at := sort.Search(len(cps), func(i int) bool { return cps[i].LineNumber > first }) - 1
	if at < 0 {
		return 0, false
	}
	return cps[at].ByteOffset, false
}

// searchFrom runs one pass, from the line start at or before byte offset
// pass.start, answering bounds (each still notFound in found) as the
// lines go by, and stops once every one is answered, at line pass.stop,
// or at the end of the text.
//
// A line answers every pending bound its own timestamp reaches. The
// bounds are kept sorted, so those are a prefix of them, and a line
// costs one comparison however many bounds are pending.
func (t *fileTimes) searchFrom(ctx context.Context, text textSource, pass searchPass, bounds []int64, found map[int64]lineAt) error {
	pending := append([]int64(nil), bounds...)
	sort.Slice(pending, func(i, j int) bool { return pending[i] < pending[j] })
	cursor, err := text.openNear(pass.start, 0)
	if err != nil {
		return err
	}
	defer func() { _ = cursor.close() }()

	lines := newStampReader(withContext(ctx, cursor), t.parser, searchBufferBytes(t.section != nil))
	for number := cursor.line; len(pending) > 0 && number < pass.stop; number++ {
		stamp, ok, length, err := lines.next()
		if err != nil {
			return err
		}
		if length == 0 {
			return nil
		}
		for ok && len(pending) > 0 && pending[0] <= stamp.Ms {
			found[pending[0]] = lineAt{line: number, stamp: stamp}
			pending = pending[1:]
		}
	}
	return nil
}

// searchBufferBytes sizes the read buffer of a search pass: small with
// an index, whose pass reads at most one step and should not read far
// past its answer; large without, whose pass may read the whole file.
func searchBufferBytes(indexed bool) int {
	if indexed {
		return 4 * 1024
	}
	return 64 * 1024
}

// stampReader reads a text line by line and gives each line's own
// timestamp as an index build reads it (index.LineStamp), without
// keeping the line: a search without an index reads every line of a
// multi-gigabyte file, so a line costs no allocation.
type stampReader struct {
	br     *bufio.Reader
	parser *timestamps.Parser
	// head holds the first bytes of a line longer than br's buffer,
	// which is all the parser looks at.
	head [timestamps.WindowBytes]byte
}

// newStampReader returns a stampReader over r with a read buffer of
// size bytes, which must exceed timestamps.WindowBytes.
func newStampReader(r io.Reader, parser *timestamps.Parser, size int) *stampReader {
	return &stampReader{br: bufio.NewReaderSize(r, size), parser: parser}
}

// next reads one line and returns its own timestamp (ok false when it
// has none) and its length in bytes, line break included. A length of 0
// means the text has ended: a text that ends with a line break gives a
// final empty read, and that is not a line.
func (s *stampReader) next() (stamp timestamps.Stamp, ok bool, length int64, err error) {
	chunk, err := s.br.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		if err != nil && !errors.Is(err, io.EOF) {
			return stamp, false, 0, err
		}
		if len(chunk) == 0 {
			return stamp, false, 0, nil
		}
		stamp, ok = index.LineStamp(s.parser, chunk)
		return stamp, ok, int64(len(chunk)), nil
	}
	// A line longer than the buffer. The parser sees the line without
	// its trailing line-break bytes (\r and \n), cut to WindowBytes.
	// Keep its first bytes, and find where its content ends: that is
	// after the last byte that is not \r or \n, which only a line of
	// many \r bytes places within the window.
	kept := copy(s.head[:], chunk)
	contentEnd := int64(contentEndOf(chunk))
	length = int64(len(chunk))
	for errors.Is(err, bufio.ErrBufferFull) {
		chunk, err = s.br.ReadSlice('\n')
		if end := contentEndOf(chunk); end > 0 {
			contentEnd = length + int64(end)
		}
		length += int64(len(chunk))
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return stamp, false, 0, err
	}
	stamp, ok = s.parser.Own(s.head[:min(int64(kept), contentEnd)])
	return stamp, ok, length, nil
}

// contentEndOf returns the length of b without its trailing \r and \n
// bytes.
func contentEndOf(b []byte) int {
	n := len(b)
	for n > 0 && (b[n-1] == '\n' || b[n-1] == '\r') {
		n--
	}
	return n
}

// lastStamp returns the own timestamp of the file's last timestamped
// line, for a time-of-day query on a file without an index (an index
// stores it). It is called only when the file has a timestamped line.
//
// A plain file and a seekable zstd file are read back from the end of
// their text in steps of tailStepBytes until a step holds a timestamped
// line, which is one step in practice. A stream-compressed file cannot
// be entered at its end, so its whole text is decompressed: the cost
// that reading such a file without an index always has.
func (t *fileTimes) lastStamp(req Request, kind filekind.Kind) (timestamps.Stamp, error) {
	if !readsByPosition(kind) {
		return t.lastStampOfStream(req.context(), req.Source, kind.Format)
	}
	f, err := openFileForSamples(req.Source)
	if err != nil {
		return timestamps.Stamp{}, err
	}
	defer func() { _ = f.Close() }()
	file, ok := f.(positionalFile)
	if !ok {
		return timestamps.Stamp{}, fmt.Errorf("read the end of %s: the file cannot be read by position", req.Source.Path())
	}
	text, size, err := textByPosition(req.context(), file, kind, noDecodeLimit)
	if err != nil {
		return timestamps.Stamp{}, err
	}
	// No limit: the answer must equal the indexed one, which knows the
	// last timestamp wherever it is, so the read goes back as far as the
	// last timestamped line. A time-of-day query on a file without an
	// index reads the file from its start for the line anyway.
	stamp, found, err := lastStampFromEnd(req.context(), text, size, t.parser, size)
	if err == nil && !found {
		err = fmt.Errorf("%s: no timestamped line found reading back from the end", req.Source.Path())
	}
	return stamp, err
}

// noDecodeLimit is the decodeLimit of a read by position that decodes
// as many frames of a seekable file as its reads need.
const noDecodeLimit = 0

// errDecodeLimit is the error of a read by position of a seekable file
// that would decode more text than its decodeLimit.
var errDecodeLimit = errors.New("the frames this read needs decode to more text than its limit")

// textByPosition returns the text of file, a plain or seekable zstd
// file, as a reader by position, and the text's length. For a seekable
// file, the reader decodes at most decodeLimit bytes of frames over its
// life, or any number with noDecodeLimit; a read that needs more fails
// with errDecodeLimit. Under a head limit in ctx (ResolveFromHead), a
// read that ends past the head fails with errPastHead (limitToHead).
func textByPosition(ctx context.Context, file positionalFile, kind filekind.Kind, decodeLimit int64) (io.ReaderAt, int64, error) {
	if !kind.IsCompressed() {
		info, err := file.Stat()
		if err != nil {
			return nil, 0, err
		}
		return limitToHead(ctx, file, info.Size()), info.Size(), nil
	}
	frames := kind.Table.Frames
	size := int64(0)
	if len(frames) > 0 {
		size = frames[len(frames)-1].DecompressedEnd()
	}
	text := &seekableTextAt{ctx: ctx, file: file, table: kind.Table, decoder: seekable.NewDecoder(), decodeLimit: decodeLimit}
	return limitToHead(ctx, text, size), size, nil
}

// seekableTextAt reads a seekable zstd file's text by position. It
// decodes one frame at a time and keeps two decoded frames, so a run of
// reads that moves through the text a little at a time, forward or
// back, decodes each frame once: the read back from the end for a
// file's last timestamp steps back a mebibyte at a time, and the
// lookback sweep of line_timestamps reads short spans in ascending
// order. A newly decoded frame replaces the kept frame farthest from
// it, which is the one such a sweep has left behind. Each read used to
// decode every frame it touched again.
//
// It holds at most two decoded frames, each as long as the seek table
// says (the decoder refuses a frame that decodes to another length).
//
// SECURITY: a frame decodes whole, and holds up to 128 MiB of text, so
// the bytes a read asks for say little about the work it costs. With a
// decodeLimit, the reader adds up the text of every frame it decodes
// and refuses, before decoding it, a frame that would take the sum past
// the limit; the seek table gives a frame's text length before it is
// decoded, and the decoder holds the frame to it.
type seekableTextAt struct {
	// ctx is checked before each frame is decoded.
	ctx     context.Context
	file    io.ReaderAt
	table   *seekable.SeekTable
	decoder *seekable.Decoder
	// kept are the frames decoded last, the most recent first.
	kept [2]decodedFrame
	// decodeLimit is the most text the reader decodes over its life;
	// noDecodeLimit (zero) for no limit. decoded is the text decoded so
	// far, a frame decoded twice counted twice.
	decodeLimit, decoded int64
}

// decodedFrame is one frame's text; data is nil for no frame.
type decodedFrame struct {
	index int
	data  []byte
}

// ReadAt implements io.ReaderAt over the decompressed text.
func (s *seekableTextAt) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("read the text at %d: a negative offset", offset)
	}
	n := 0
	for n < len(p) {
		at := offset + int64(n)
		// The frame holding byte at: the first that ends after it. The
		// table's frames are contiguous, so it also starts at or before
		// it.
		index := sort.Search(len(s.table.Frames), func(i int) bool {
			return s.table.Frames[i].DecompressedEnd() > at
		})
		if index == len(s.table.Frames) {
			return n, io.EOF
		}
		data, err := s.frame(index)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], data[at-s.table.Frames[index].DecompressedOffset:])
	}
	return n, nil
}

// frame returns frame index's text, from the frames kept or decoded
// now, and keeps it.
func (s *seekableTextAt) frame(index int) ([]byte, error) {
	for _, kept := range s.kept {
		if kept.data != nil && kept.index == index {
			return kept.data, nil
		}
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	size := s.table.Frames[index].DecompressedSize
	// Written as a subtraction so the sum cannot overflow.
	if s.decodeLimit != noDecodeLimit && size > s.decodeLimit-s.decoded {
		return nil, fmt.Errorf("frame %d holds %d bytes of text, %d decoded already: %w",
			index, size, s.decoded, errDecodeLimit)
	}
	data, err := decodeFrameAt(s.decoder, s.file, index, s.table)
	if err != nil {
		return nil, err
	}
	s.decoded += size
	s.kept[s.slotFor(index)] = decodedFrame{index: index, data: data}
	return data, nil
}

// slotFor returns the slot of kept that frame index replaces: an empty
// one, else the one whose frame is farthest from index. A read that
// moves back through the text reads each step's frames in ascending
// order, so the frame it needs next is the one below the frame it reads
// now, and the one above is done with; a read that moves forward needs
// the reverse. Either way the farthest frame is the one left behind.
func (s *seekableTextAt) slotFor(index int) int {
	for slot, kept := range s.kept {
		if kept.data == nil {
			return slot
		}
	}
	distance := func(slot int) int { return max(s.kept[slot].index-index, index-s.kept[slot].index) }
	if distance(0) > distance(1) {
		return 0
	}
	return 1
}

// lastStampOfStream reads a compressed stream to its end and returns
// the own timestamp of its last timestamped line.
func (t *fileTimes) lastStampOfStream(ctx context.Context, src paths.Pinned, format compression.Format) (timestamps.Stamp, error) {
	cursor, err := streamedText{src: src, format: format}.openAt(0)
	if err != nil {
		return timestamps.Stamp{}, err
	}
	defer func() { _ = cursor.close() }()
	lines := newStampReader(withContext(ctx, cursor), t.parser, 64*1024)
	var last timestamps.Stamp
	for {
		stamp, ok, length, err := lines.next()
		if err != nil {
			return timestamps.Stamp{}, err
		}
		if length == 0 {
			return last, nil
		}
		if ok {
			last = stamp
		}
	}
}

// lastStampFromEnd reads text, size bytes long, back from its end and
// returns the own timestamp of its last timestamped line that starts in
// the last limit bytes, or false when no such line has one. It reads
// what tailWindows reads, no more.
func lastStampFromEnd(ctx context.Context, text io.ReaderAt, size int64, parser *timestamps.Parser, limit int64) (timestamps.Stamp, bool, error) {
	var stamp timestamps.Stamp
	found := false
	err := tailWindows(ctx, text, size, limit, func(_ int64, window []byte) bool {
		stamp, found = parser.Own(window)
		return found
	})
	if err != nil {
		return timestamps.Stamp{}, false, err
	}
	return stamp, found, nil
}

// tailWindows reads text, size bytes long, back from its end in steps of
// tailStepBytes and calls visit with the start of each line that starts
// in the last limit bytes, the last line first, and the bytes the parser
// looks at for it: the line without its trailing \r and \n bytes, cut
// to timestamps.WindowBytes, as index.LineStamp gives them to it. It
// stops when visit returns true. The window is valid only during the
// call.
//
// It reads each byte of the last limit bytes once, and the one byte
// before them, and nothing else, however long a line or a run of \r
// bytes. A step reads the text in [start, end). The line starts it
// visits are those in (start, end]: whether a position begins a line is
// told by the byte before it, which the step holds. A window reaches up
// to timestamps.WindowBytes past the step's end, into text the step
// before in the loop (the one after in the text) read already and hands
// on in ahead. The line start at the floor itself is visited last, and
// reads the byte before it.
//
// A line whose window ends in \r bytes needs one fact from past the
// bytes in hand: whether those \r bytes run on to a \n (line-break
// bytes, dropped) or to another byte (content, kept). The step after in
// the text has read those bytes too, so each step works out the fact
// for the next and hands it on in endsLine.
//
// Reading each step's text once, with nothing past it, is what lets a
// seekable file's reader keep the frames a step needs: a step touches
// the frames of [start, end) only, and the next step's frames end with
// the lowest of them.
func tailWindows(ctx context.Context, text io.ReaderAt, size, limit int64, visit func(lineStart int64, window []byte) bool) error {
	floor := max(0, size-limit)
	// endsLine says whether the text from the end of this step's bytes
	// on holds only \r bytes before a \n or the end of the text; ahead
	// is the text from the step's end on, as far as a window reaches.
	// The first step ends at the end of the text.
	endsLine := true
	var ahead []byte
	for end := size; end > floor; {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := max(floor, end-tailStepBytes)
		// buf is the text from start to the end of ahead.
		buf := make([]byte, end-start+int64(len(ahead)))
		if err := readFullAt(text, buf[:end-start], start); err != nil {
			return err
		}
		copy(buf[end-start:], ahead)
		// The end of the text begins no line.
		for lineStart := min(end, size-1); lineStart > start; lineStart-- {
			if buf[lineStart-1-start] != '\n' {
				continue
			}
			if visit(lineStart, lineWindow(buf[lineStart-start:], endsLine)) {
				return nil
			}
		}
		// The bytes the next step hands its windows end WindowBytes past
		// start (or at the end of the text), which is within buf.
		next := min(int64(len(buf)), timestamps.WindowBytes)
		endsLine = onlyCarriageReturnsBeforeLineEnd(buf[next:], endsLine)
		ahead = bytes.Clone(buf[:next])
		end = start
	}
	return visitFloor(text, floor, size, ahead, endsLine, visit)
}

// visitFloor calls visit for the line start at floor, the lowest
// position tailWindows looks at, when floor begins a line: it is the
// start of the text or follows a \n, the one byte it reads. ahead and
// endsLine are what the step that began at floor handed on.
func visitFloor(text io.ReaderAt, floor, size int64, ahead []byte, endsLine bool, visit func(lineStart int64, window []byte) bool) error {
	if floor >= size {
		return nil
	}
	if floor > 0 {
		var before [1]byte
		if err := readFullAt(text, before[:], floor-1); err != nil {
			return err
		}
		if before[0] != '\n' {
			return nil
		}
	}
	visit(floor, lineWindow(ahead, endsLine))
	return nil
}

// readFullAt fills buf with the text at offset. ReadAt may report
// io.EOF with a full buffer at the end of the text; only a short read
// is a failure.
func readFullAt(text io.ReaderAt, buf []byte, offset int64) error {
	n, err := text.ReadAt(buf, offset)
	if err != nil && (!errors.Is(err, io.EOF) || n < len(buf)) {
		return err
	}
	return nil
}

// lineWindow returns the bytes the parser looks at for the line whose
// text from its start on, as far as the caller has it in hand, is rest:
// the line without its trailing \r and \n bytes, cut to
// timestamps.WindowBytes, as index.LineStamp gives them to it. rest
// holds at least timestamps.WindowBytes bytes, or all of the text to its
// end. endsLine says whether the text after rest holds only \r bytes
// before a \n or the end of the text.
//
// When the line ends within rest, its content is in hand: the line
// break is searched for in all of rest, so a line a little longer than
// the window, such as one whose window ends with the \r of a \r\n, is
// read right. When the line runs past rest, the \r bytes at the end of
// rest are line-break bytes when endsLine is set, and content when it
// is not (a byte other than \r and \n follows them).
func lineWindow(rest []byte, endsLine bool) []byte {
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		return windowOfContent(rest[:i+1])
	}
	if endsLine {
		return windowOfContent(rest)
	}
	return rest[:min(len(rest), timestamps.WindowBytes)]
}

// onlyCarriageReturnsBeforeLineEnd reports whether the text from b's
// first byte on holds only \r bytes before a \n or the end of the text,
// given endsLine, the same answer for the text after b. It reads b only
// as far as its first byte other than \r.
func onlyCarriageReturnsBeforeLineEnd(b []byte, endsLine bool) bool {
	for _, c := range b {
		if c != '\r' {
			return c == '\n'
		}
	}
	return endsLine
}

// trimLineEnd drops the trailing \r and \n bytes of b.
func trimLineEnd(b []byte) []byte { return b[:contentEndOf(b)] }

// windowOfContent returns the window the parser looks at for a whole
// line: its content, cut to timestamps.WindowBytes.
func windowOfContent(line []byte) []byte {
	content := trimLineEnd(line)
	return content[:min(len(content), timestamps.WindowBytes)]
}

// decodeFrameAt decompresses one frame of a seekable file read by
// position. It is a variable so a test can count the bytes decoded.
var decodeFrameAt = func(d *seekable.Decoder, file io.ReaderAt, index int, table *seekable.SeekTable) ([]byte, error) {
	return d.DecompressFrameAt(file, index, table)
}
