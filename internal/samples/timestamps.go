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
// Each costs a search of up to one index step, so the bound keeps the
// reading one request can ask for proportional to what it gets back.
const MaxTimestampValues = 1000

// IsUsageError reports whether err, from Resolve, is a mistake in the
// request rather than a failure to read the file: an invalid time
// query, too many of them, or a time query on a file without
// timestamps. The CLI exits 2 on it and the HTTP API answers 400.
func IsUsageError(err error) bool {
	return errors.Is(err, timestamps.ErrInvalidQuery) ||
		errors.Is(err, ErrNoTimeFormat) ||
		errors.Is(err, ErrTooManyTimestamps)
}

// tailStepBytes is how much of a file's text one step of the read back
// from its end covers, looking for the last timestamped line.
const tailStepBytes = 1 << 20

// fileTimes is what the samples answer knows about a file's timestamps:
// its parser and, when the file has a line index, the index's time
// section and checkpoints.
type fileTimes struct {
	parser *timestamps.Parser
	// section and checkpoints are the stored index's time_index and
	// line_index; section is nil without an index.
	section     *rxtypes.TimeIndex
	checkpoints []rxtypes.LineIndexEntry
	logZone     config.Zone
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
func timesOf(req Request, kind filekind.Kind) (*fileTimes, error) {
	logZone := config.LogTZ()
	if idx := loadIndexOrNone(req); idx != nil {
		if idx.TimeIndex == nil {
			return nil, nil
		}
		ti := idx.TimeIndex
		format := timestamps.Format{
			Family: timestamps.Family(ti.Format), Anchored: ti.Anchored,
			DayFirst: ti.DayFirst, HasZone: ti.HasZone,
		}
		// The mtime the index was built with: a syslog line's year
		// comes from it. The index describes the file only while that
		// mtime is the file's, so this is the pin's mtime too.
		parser, err := timestamps.NewParser(format, idx.SourceMtimeNs)
		if err != nil {
			return nil, err
		}
		return &fileTimes{parser: parser, section: ti, checkpoints: idx.LineIndex, logZone: logZone}, nil
	}
	format, ok, err := detectTimeFormat(req.Source, kind)
	if err != nil || !ok {
		return nil, err
	}
	parser, err := timestamps.NewParser(format, req.Source.Info().ModTime().UnixNano())
	if err != nil {
		return nil, err
	}
	return &fileTimes{parser: parser, logZone: logZone}, nil
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
	format := t.parser.Format()
	// A file whose timestamps carry zones reads a line without one as
	// UTC; a file whose timestamps carry none was written in RX_LOG_TZ.
	assumed := "UTC"
	if !format.HasZone {
		assumed = t.logZone.Name
	}
	return &rxtypes.SamplesTimeFormat{
		Format: string(format.Family), HasZone: format.HasZone, AssumedZone: assumed,
	}
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
func resolveTimestamps(req Request, kind filekind.Kind, times *fileTimes, resp *rxtypes.SamplesResponse) error {
	if len(req.Timestamps) > MaxTimestampValues {
		return fmt.Errorf("%w: %d given, at most %d per request", ErrTooManyTimestamps, len(req.Timestamps), MaxTimestampValues)
	}
	if times == nil {
		return fmt.Errorf("%w of %s", ErrNoTimeFormat, req.Path)
	}
	queries := make([]timestamps.Query, len(req.Timestamps))
	for i, value := range req.Timestamps {
		q, err := timestamps.ParseQuery(value, times.parser)
		if err != nil {
			return err
		}
		queries[i] = q
	}
	text := textSourceFor(req, kind)
	fileContext, err := times.resolveContext(req, kind, text, queries)
	if err != nil {
		return err
	}
	resolved := make([]timestamps.Resolved, len(queries))
	var bounds []int64
	for i, q := range queries {
		r, resolveErr := timestamps.Resolve(q, fileContext)
		if resolveErr != nil {
			return resolveErr
		}
		resolved[i] = r
		bounds = append(bounds, searchBoundsOf(r)...)
	}
	found, err := times.searchBounds(text, bounds)
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
func answerTimeWindows(req Request, kind filekind.Kind, windows []timeWindow, resp *rxtypes.SamplesResponse) error {
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
	answer := &rxtypes.SamplesResponse{Offsets: map[string]int64{}, Lines: map[string]int64{}, Samples: map[string][]string{}}
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
		resp.Samples[w.value] = sample
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
	format := t.parser.Format()
	queryZone := config.QueryTZ()
	c := timestamps.ResolveContext{HasZone: format.HasZone, LogZone: t.logZone.Location, QueryZone: queryZone.Location}

	needsSpan, needsFirst := false, false
	for _, q := range queries {
		needsSpan = needsSpan || q.NeedsSpan()
		// A zoned file reads a query without a zone at its first
		// timestamp's offset, unless RX_QUERY_TZ names a zone.
		needsFirst = needsFirst || (format.HasZone && c.QueryZone == nil && hasWallEndpoint(q))
	}
	if !needsSpan && !needsFirst {
		return c, nil
	}
	if t.section != nil {
		if t.section.First != nil && t.section.Last != nil {
			c.FirstMs, c.LastMs, c.HasSpan = t.section.First.Ms, t.section.Last.Ms, true
		}
		if t.section.FirstZoneOffsetMinutes != nil {
			c.FirstOffsetMinutes = *t.section.FirstZoneOffsetMinutes
		}
		return c, nil
	}

	found, err := t.searchBounds(text, []int64{math.MinInt64})
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
// line. With one, a bound's pass starts at the last checkpoint whose
// max_before (the latest timestamp of every line before it) is below
// the bound: no earlier line can answer it, and the next checkpoint's
// max_before is at least the bound, so a line before that checkpoint
// does. The pass then reads at most one index step. The answer is the
// same either way; the index only says where it cannot be.
//
// Bounds that start at the same checkpoint share one pass. A stream
// compressed file (gzip, bzip2, xz, plain zstd) is decompressed from
// its first byte whatever the checkpoint, so all its bounds share one
// pass, from the earliest start.
func (t *fileTimes) searchBounds(text textSource, bounds []int64) (map[int64]lineAt, error) {
	found := make(map[int64]lineAt, len(bounds))
	groups := map[int64][]int64{}
	for _, bound := range bounds {
		if _, seen := found[bound]; seen {
			continue
		}
		found[bound] = notFound
		start := t.searchStart(bound)
		groups[start] = append(groups[start], bound)
	}
	if _, streamed := text.(streamedText); streamed && len(groups) > 1 {
		groups = mergedGroups(groups)
	}
	starts := make([]int64, 0, len(groups))
	for start := range groups {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	for _, start := range starts {
		if err := t.searchFrom(text, start, groups[start], found); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// mergedGroups puts every bound in one group at the earliest start.
func mergedGroups(groups map[int64][]int64) map[int64][]int64 {
	earliest := int64(math.MaxInt64)
	var all []int64
	for start, bounds := range groups {
		earliest = min(earliest, start)
		all = append(all, bounds...)
	}
	return map[int64][]int64{earliest: all}
}

// searchStart is the byte offset of the checkpoint a search for bound
// starts from: the last whose max_before is below bound, or 0 without
// an index.
//
// max_before never decreases, and a null (no earlier timestamp) only
// precedes the values (validTimeIndex refuses anything else), so "is
// at least bound" is false and then true along the list, which is what
// a binary search needs.
func (t *fileTimes) searchStart(bound int64) int64 {
	if t.section == nil || len(t.checkpoints) == 0 {
		return 0
	}
	maxBefore := t.section.MaxBefore
	reached := sort.Search(len(maxBefore), func(i int) bool {
		return maxBefore[i] != nil && *maxBefore[i] >= bound
	})
	if reached == 0 {
		return 0
	}
	return t.checkpoints[reached-1].ByteOffset
}

// searchFrom runs one pass from the line start at or before byte offset
// start, answering bounds (each still notFound in found) as the lines go
// by, and stops once every one is answered or the text ends.
//
// A line answers every pending bound its own timestamp reaches. The
// bounds are kept sorted, so those are a prefix of them, and a line
// costs one comparison however many bounds are pending.
func (t *fileTimes) searchFrom(text textSource, start int64, bounds []int64, found map[int64]lineAt) error {
	pending := append([]int64(nil), bounds...)
	sort.Slice(pending, func(i, j int) bool { return pending[i] < pending[j] })
	cursor, err := text.openNear(start, 0)
	if err != nil {
		return err
	}
	defer func() { _ = cursor.close() }()

	lines := newStampReader(cursor, t.parser, searchBufferBytes(t.section != nil))
	for number := cursor.line; len(pending) > 0; number++ {
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
	readsByPosition := !kind.IsCompressed() || (kind.IsSeekable() && kind.Table != nil)
	if !readsByPosition {
		return t.lastStampOfStream(req.Source, kind.Format)
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
	text, size, err := textByPosition(file, kind)
	if err != nil {
		return timestamps.Stamp{}, err
	}
	stamp, found, err := lastStampFromEnd(text, size, t.parser)
	if err == nil && !found {
		err = fmt.Errorf("%s: no timestamped line found reading back from the end", req.Source.Path())
	}
	return stamp, err
}

// textByPosition returns the text of file, a plain or seekable zstd
// file, as a reader by position, and the text's length.
func textByPosition(file positionalFile, kind filekind.Kind) (io.ReaderAt, int64, error) {
	if !kind.IsCompressed() {
		info, err := file.Stat()
		if err != nil {
			return nil, 0, err
		}
		return file, info.Size(), nil
	}
	frames := kind.Table.Frames
	size := int64(0)
	if len(frames) > 0 {
		size = frames[len(frames)-1].DecompressedEnd()
	}
	return seekableTextAt{file: file, table: kind.Table, decoder: seekable.NewDecoder()}, size, nil
}

// seekableTextAt reads a seekable zstd file's text by position,
// decompressing the frames that hold the bytes asked for.
type seekableTextAt struct {
	file    io.ReaderAt
	table   *seekable.SeekTable
	decoder *seekable.Decoder
}

// ReadAt implements io.ReaderAt over the decompressed text.
func (s seekableTextAt) ReadAt(p []byte, offset int64) (int, error) {
	data, err := s.decoder.DecompressRangeAt(context.Background(), s.file, s.table, offset, int64(len(p)))
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// lastStampOfStream reads a compressed stream to its end and returns
// the own timestamp of its last timestamped line.
func (t *fileTimes) lastStampOfStream(src paths.Pinned, format compression.Format) (timestamps.Stamp, error) {
	cursor, err := streamedText{src: src, format: format}.openAt(0)
	if err != nil {
		return timestamps.Stamp{}, err
	}
	defer func() { _ = cursor.close() }()
	lines := newStampReader(cursor, t.parser, 64*1024)
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

// lastStampFromEnd reads text, size bytes long, back from its end in
// steps of tailStepBytes and returns the own timestamp of its last
// timestamped line, or false when no line has one.
//
// A step covers the line starts in [start, end); it reads one byte
// before start, to tell whether start begins a line, and up to
// timestamps.WindowBytes after end, the most of a line the parser looks
// at. The latest line start whose line has a timestamp is the answer.
func lastStampFromEnd(text io.ReaderAt, size int64, parser *timestamps.Parser) (timestamps.Stamp, bool, error) {
	for end := size; end > 0; {
		start := max(0, end-tailStepBytes)
		from := max(0, start-1)
		buf := make([]byte, min(size, end+timestamps.WindowBytes)-from)
		n, err := text.ReadAt(buf, from)
		// ReadAt may report io.EOF with a full buffer at the end of the
		// text; only a short read is a failure.
		if err != nil && (!errors.Is(err, io.EOF) || n < len(buf)) {
			return timestamps.Stamp{}, false, err
		}
		for lineStart := end - 1; lineStart >= start; lineStart-- {
			if lineStart > 0 && buf[lineStart-1-from] != '\n' {
				continue
			}
			window, err := lineWindow(text, buf[lineStart-from:], lineStart, size)
			if err != nil {
				return timestamps.Stamp{}, false, err
			}
			if stamp, ok := parser.Own(window); ok {
				return stamp, true, nil
			}
		}
		end = start
	}
	return timestamps.Stamp{}, false, nil
}

// lineWindow returns the bytes the parser looks at for the line that
// starts at lineStart: the line without its trailing \r and \n bytes,
// cut to timestamps.WindowBytes, as index.LineStamp gives them to it.
// rest is the text from lineStart on that the caller has in hand.
//
// When the line ends within the window, rest holds all of it. When it
// runs past the window, the parser sees the window whole, unless every
// byte from the window's last one to the line break is a \r: then
// those bytes are line-break bytes and are dropped. Only that case
// reads further, past the run of \r bytes.
func lineWindow(text io.ReaderAt, rest []byte, lineStart, size int64) ([]byte, error) {
	limit := min(len(rest), timestamps.WindowBytes)
	if i := bytes.IndexByte(rest[:limit], '\n'); i >= 0 {
		return trimLineEnd(rest[:i+1]), nil
	}
	if lineStart+int64(limit) >= size {
		return trimLineEnd(rest[:limit]), nil
	}
	window := rest[:limit]
	if window[limit-1] != '\r' {
		return window, nil
	}
	endsLine, err := onlyCarriageReturnsUntilLineEnd(text, lineStart+int64(limit), size)
	if err != nil || !endsLine {
		return window, err
	}
	return trimLineEnd(window), nil
}

// onlyCarriageReturnsUntilLineEnd reports whether the text from offset
// on holds only \r bytes before a \n or the end of the text.
func onlyCarriageReturnsUntilLineEnd(text io.ReaderAt, offset, size int64) (bool, error) {
	buf := make([]byte, 4096)
	for offset < size {
		n, err := text.ReadAt(buf[:min(int64(len(buf)), size-offset)], offset)
		for _, c := range buf[:n] {
			if c != '\r' {
				return c == '\n', nil
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if n == 0 {
			break
		}
		offset += int64(n)
	}
	return true, nil
}

// trimLineEnd drops the trailing \r and \n bytes of b.
func trimLineEnd(b []byte) []byte { return b[:contentEndOf(b)] }
