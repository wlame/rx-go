package samples

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Where a time range came from: the values of TimeRangeResponse.Source.
const (
	// TimeRangeFromIndex: the file's line index, whose time section
	// holds the first and the last timestamp. Nothing of the file is
	// read.
	TimeRangeFromIndex = "index"
	// TimeRangeFromScan: the head of the text and a read back from its
	// end, read for this answer.
	TimeRangeFromScan = "scan"
	// TimeRangeNone: the range is not known. A gzip, bzip2, xz or plain
	// zstd file cannot be entered at its end, and without an index its
	// last timestamp needs its whole text decompressed.
	TimeRangeNone = "none"
)

// TimeRangeTailBytes is how far back from the end of a file's text the
// read for its last timestamp goes: 16 steps of tailStepBytes. A file
// whose last timestamped line starts further back answers last_ms null.
const TimeRangeTailBytes = 16 * tailStepBytes

// TimeRangeDecodeBytes is the most text the read back from the end of a
// seekable zstd file decodes: twice TimeRangeTailBytes. A frame decodes
// whole, and a frame may hold up to 128 MiB of text, so a read of a
// mebibyte of text can cost a hundred times that. A file whose frames
// at its end are too large for the limit answers last_ms null. Frames
// of 16 MiB fit: the TimeRangeTailBytes of text the read examines lie
// in at most two of them.
const TimeRangeDecodeBytes = 2 * TimeRangeTailBytes

// TimeRange returns the time range of one file: its timestamp format,
// its first and last timestamp as UTC instants, the zone its lines show
// times in, and its first timestamp as written. It reads req.Path
// through req.Source (pinning it when that is the zero value), takes
// req.Kind when the caller classified the file already, and consults
// req.IndexLoader; the position and context fields of req are not used.
//
// The answer comes from:
//
//   - a line index that still describes the file: its time section
//     (source "index"). Nothing of the file is read.
//   - without one, a plain or seekable zstd file: the head of its text,
//     at most timestamps.SampleBytes, gives the format and the first
//     timestamp, and a read back from the end in steps of a mebibyte,
//     at most TimeRangeTailBytes, the last (source "scan"). The read
//     back decodes at most TimeRangeDecodeBytes of a seekable file's
//     frames; past either limit last_ms is null.
//   - without one, a gzip, bzip2, xz or plain zstd file: the head of its
//     text gives the format; first_ms and last_ms stay null (source
//     "none"). The answer never decompresses a whole file: an index
//     build does that once and keeps what it finds.
//
// The first and last timestamps are those of the first and the last
// line, in file order, with a timestamp of their own: the lines an
// index's time_index.first and .last name. Each is turned from the
// file's frame into a UTC instant as line_timestamps turns it (a file
// whose timestamps carry no zone has its wall clock read in RX_LOG_TZ),
// so a samples query for first_ms finds the first timestamped line.
//
// Under req.FileZone each of them is the wall clock its line writes,
// read in that zone, and display_zone names the zone. The index of a
// file whose timestamps carry zones holds instants, which cannot give
// back a written wall clock: its first one is the stored instant plus
// the stored offset of the first timestamp, and its last line is read
// again at the byte offset the index stores (stampOfLineAt), a window of
// one line. A gzip, bzip2, xz or plain zstd file cannot be read at an
// offset, so such a file answers source "none" under a file zone, with
// an index too.
func TimeRange(ctx context.Context, req Request) (*rxtypes.TimeRangeResponse, error) {
	req.ctx = ctx
	if req.Source.IsZero() {
		src, err := paths.Pin(req.Path)
		if err != nil {
			return nil, err
		}
		req.Source = src
	}
	req.IndexLoader = loadOnce(onlyIndexesOf(req.Source, req.IndexLoader))
	kind, err := Classify(req)
	if err != nil {
		return nil, err
	}
	if idx := loadIndexOrNone(req); idx != nil {
		return rangeOfIndex(req, kind, idx.TimeIndex)
	}
	return scanTimeRange(req, kind)
}

// fileRange is what a time range is made of, in the file's frame,
// whichever way it was found.
type fileRange struct {
	// format is the file's format as detected; frame is the frame first
	// and last are in.
	format timestamps.Format
	frame  timeFrame
	// first and last are the first and the last own timestamp; nil when
	// not known.
	first, last *timestamps.Stamp
	// example is the first timestamp as written; nil with first.
	example *string
}

// rangeOfIndex is the answer from a line index's time section, or the
// answer for a file without timestamps when the section is nil.
func rangeOfIndex(req Request, kind filekind.Kind, ti *rxtypes.TimeIndex) (*rxtypes.TimeRangeResponse, error) {
	resp := &rxtypes.TimeRangeResponse{Path: req.Path, Source: TimeRangeFromIndex}
	if ti == nil {
		return resp, nil
	}
	detected := formatOfSection(ti)
	r := fileRange{format: detected, frame: req.frameFor(detected), example: ti.FirstText}
	offset := 0
	if ti.FirstZoneOffsetMinutes != nil {
		offset = *ti.FirstZoneOffsetMinutes
	}
	if ti.First != nil {
		r.first = &timestamps.Stamp{Ms: ti.First.Ms, OffsetMinutes: offset}
	}
	if ti.Last != nil {
		r.last = &timestamps.Stamp{Ms: ti.Last.Ms}
	}
	if !r.frame.readsStoredFrame(detected) {
		if err := r.wallClocksOfStoredLines(req, kind, ti, offset); err != nil {
			return nil, err
		}
		if r.last == nil {
			// The range is unknown without the last timestamp, as for a
			// stream without an index.
			resp.Source, r.first = TimeRangeNone, nil
		}
	}
	r.describe(resp)
	return resp, nil
}

// wallClocksOfStoredLines turns r's first and last, the instants an
// index of a file whose timestamps carry zones stores, into the wall
// clocks their lines write. The first is its instant plus the offset
// written with it (firstOffset, which the index stores). The last line's
// offset is not stored, so the line is read again at the byte offset the
// index gives; r.last is nil when it cannot be: the file is a stream
// that cannot be read at an offset, the frame that holds the line would
// decode more than TimeRangeDecodeBytes, or the line is not read as the
// index read it.
func (r *fileRange) wallClocksOfStoredLines(req Request, kind filekind.Kind, ti *rxtypes.TimeIndex, firstOffset int) error {
	if r.first != nil {
		r.first = &timestamps.Stamp{Ms: r.first.Ms + int64(firstOffset)*msPerMinute}
	}
	if r.last == nil {
		return nil
	}
	r.last = nil
	if !readsByPosition(kind) {
		return nil
	}
	parser, err := timestamps.NewParser(r.frame.parserFormat(r.format), req.Source.Info().ModTime().UnixNano())
	if err != nil {
		return err
	}
	f, err := openFileForSamples(req.Source)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	file, ok := f.(positionalFile)
	if !ok {
		return fmt.Errorf("read the time range of %s: the file cannot be read by position", req.Path)
	}
	text, size, err := textByPosition(req.context(), file, kind, TimeRangeDecodeBytes)
	if err != nil {
		return err
	}
	last, found, err := stampOfLineAt(text, size, ti.Last.Offset, parser)
	if errors.Is(err, errDecodeLimit) {
		return nil
	}
	if err != nil || !found {
		return err
	}
	r.last = &last
	return nil
}

// msPerMinute converts a zone offset in minutes to milliseconds.
const msPerMinute = 60 * 1000

// maxLineReadBytes is the most text stampOfLineAt reads past a line's
// window to learn whether a run of \r bytes ends the line.
const maxLineReadBytes = tailStepBytes

// lineStepBytes is how much text stampOfLineAt reads at a time past a
// line's window.
const lineStepBytes = 4 * 1024

// stampOfLineAt returns the own timestamp of the line that starts at
// byte offset lineStart of text, size bytes long, as index.LineStamp
// reads it, and false when the line has none or when telling where its
// content ends would read more than maxLineReadBytes past its window.
//
// It reads the line's first timestamps.WindowBytes bytes, which is all
// the parser looks at. Only a window that ends in \r bytes needs more:
// whether those \r bytes run on to the line break (and are dropped) or
// to more text (and are kept), which the bytes after the window say.
// Those are read lineStepBytes at a time up to the first byte that is
// not \r, so a line of any length costs its window and, for a run of
// \r bytes, at most maxLineReadBytes more.
func stampOfLineAt(text io.ReaderAt, size, lineStart int64, parser *timestamps.Parser) (timestamps.Stamp, bool, error) {
	if lineStart < 0 || lineStart >= size {
		return timestamps.Stamp{}, false, nil
	}
	rest := make([]byte, min(size-lineStart, timestamps.WindowBytes))
	if err := readFullAt(text, rest, lineStart); err != nil {
		return timestamps.Stamp{}, false, err
	}
	endsLine, known, err := runOfCarriageReturnsEndsLine(text, size, lineStart+int64(len(rest)), rest)
	if err != nil || !known {
		return timestamps.Stamp{}, false, err
	}
	stamp, ok := parser.Own(lineWindow(rest, endsLine))
	return stamp, ok, nil
}

// runOfCarriageReturnsEndsLine returns the endsLine lineWindow needs for
// rest, the text of a line from its start up to byte offset end of text:
// whether the text from end on holds only \r bytes before a \n or the
// end of the text. It reads nothing when the answer cannot change the
// window: rest holds the line break, reaches the end of the text, or
// does not end with \r. known is false when the run of \r bytes goes
// on for more than maxLineReadBytes.
func runOfCarriageReturnsEndsLine(text io.ReaderAt, size, end int64, rest []byte) (endsLine, known bool, err error) {
	if bytes.IndexByte(rest, '\n') >= 0 || end >= size || len(rest) == 0 || rest[len(rest)-1] != '\r' {
		return true, true, nil
	}
	buf := make([]byte, lineStepBytes)
	for from := end; from < size && from-end < maxLineReadBytes; from += lineStepBytes {
		chunk := buf[:min(int64(lineStepBytes), size-from)]
		if err := readFullAt(text, chunk, from); err != nil {
			return false, false, err
		}
		for _, c := range chunk {
			if c != '\r' {
				return c == '\n', true, nil
			}
		}
	}
	// Only \r bytes to the end of the text end the line; a run past the
	// bound leaves it unknown.
	return true, end+maxLineReadBytes >= size, nil
}

// scanTimeRange is the answer for a file without an index: its head,
// and for a file read by position, its tail, read now.
func scanTimeRange(req Request, kind filekind.Kind) (*rxtypes.TimeRangeResponse, error) {
	byPosition := readsByPosition(kind)
	resp := &rxtypes.TimeRangeResponse{Path: req.Path, Source: TimeRangeFromScan}
	if !byPosition {
		resp.Source = TimeRangeNone
	}

	f, err := openFileForSamples(req.Source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	file, ok := f.(positionalFile)
	if !ok {
		return nil, fmt.Errorf("read the time range of %s: the file cannot be read by position", req.Path)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	// One read of the head serves the format, the first timestamp and
	// its text.
	format, found, head, err := index.DetectTimeFormatAndHead(file, info.Size(), kind)
	if err != nil || !found {
		return resp, err
	}
	frame := req.frameFor(format)
	parser, err := timestamps.NewParser(frame.parserFormat(format), req.Source.Info().ModTime().UnixNano())
	if err != nil {
		return nil, err
	}
	r := fileRange{format: format, frame: frame}
	if first, text, ok := firstStampOfHead(head, headIsWholeText(head, info.Size(), kind), parser); ok {
		r.first, r.example = &first, &text
	}
	if byPosition && r.first != nil {
		last, ok, err := lastStampOfTail(req.context(), file, kind, parser)
		if err != nil {
			return nil, err
		}
		if ok {
			r.last = &last
		}
	}
	r.describe(resp)
	if !byPosition {
		// The range is unknown without the last timestamp; the first
		// alone is no range, and a client waits for the index.
		resp.FirstMs = nil
	}
	return resp, nil
}

// lastStampOfTail returns the own timestamp of the last timestamped line
// that starts within TimeRangeTailBytes of the end of file's text, a
// plain or seekable zstd file, and false when there is none or when
// finding it would decode more than TimeRangeDecodeBytes of a seekable
// file's frames. The answer is then unknown rather than an error: the
// first timestamp still makes a useful answer.
func lastStampOfTail(ctx context.Context, file positionalFile, kind filekind.Kind, parser *timestamps.Parser) (timestamps.Stamp, bool, error) {
	text, size, err := textByPosition(ctx, file, kind, TimeRangeDecodeBytes)
	if err != nil {
		return timestamps.Stamp{}, false, err
	}
	last, ok, err := lastStampFromEnd(ctx, text, size, parser, TimeRangeTailBytes)
	if errors.Is(err, errDecodeLimit) {
		return timestamps.Stamp{}, false, nil
	}
	return last, ok, err
}

// describe fills the format, zone, example and instants of resp.
// has_zone is the file's format's; the zone and the instants follow
// r.frame.
func (r fileRange) describe(resp *rxtypes.TimeRangeResponse) {
	family := string(r.format.Family)
	hasZone := r.format.HasZone
	resp.Format, resp.HasZone, resp.Example = &family, &hasZone, r.example
	if r.format.DayFirst != nil {
		dayFirst := *r.format.DayFirst
		resp.DayFirst = &dayFirst
	}
	resp.DisplayZone = displayZone(r.frame, r.first)
	if r.first != nil {
		resp.FirstMs = instantOf(r.first.Ms, r.frame)
	}
	if r.last != nil {
		resp.LastMs = instantOf(r.last.Ms, r.frame)
	}
}

// instantOf is the UTC instant of fileMs, a value in frame, or nil when
// it falls outside the years 1 to 9999.
func instantOf(fileMs int64, frame timeFrame) *int64 {
	instant, ok := frame.instant(fileMs)
	if !ok {
		return nil
	}
	return &instant
}

// displayZone is the zone a file's lines show their times in: the
// frame's zone (RX_LOG_TZ, or the file zone) when its values are wall
// clocks, and the offset written with the first timestamp when they are
// instants (nil when there is none).
func displayZone(frame timeFrame, first *timestamps.Stamp) *string {
	if !frame.hasZone {
		name := frame.zone.Name
		if name == "" {
			name = "UTC"
		}
		return &name
	}
	if first == nil {
		return nil
	}
	offset := formatOffset(first.OffsetMinutes)
	return &offset
}

// formatOffset writes a zone offset east of UTC as ±HH:MM.
func formatOffset(minutes int) string {
	sign := '+'
	if minutes < 0 {
		sign, minutes = '-', -minutes
	}
	return fmt.Sprintf("%c%02d:%02d", sign, minutes/60, minutes%60)
}

// firstStampOfHead returns the own timestamp of the first line of head
// that has one, with its text as written (timestamps.Parser.Text), and
// false when none has. Only whole lines count: the last line of a head
// that is not the whole text may be cut, and is left out.
//
// Detection decided the format from the whole lines of this head, and
// only from lines that carry a timestamp of it, so a format found means
// the first timestamped line is here: the index build, which reads the
// same lines with the same parser, records the same line as its first.
func firstStampOfHead(head []byte, wholeText bool, parser *timestamps.Parser) (timestamps.Stamp, string, bool) {
	for rest := head; len(rest) > 0; {
		line := rest
		if end := bytes.IndexByte(rest, '\n'); end >= 0 {
			line, rest = rest[:end+1], rest[end+1:]
		} else if !wholeText {
			break
		} else {
			rest = nil
		}
		content := trimLineEnd(line)
		if stamp, ok := parser.Own(content); ok {
			text, _ := parser.Text(content)
			return stamp, text, true
		}
	}
	return timestamps.Stamp{}, "", false
}

// headIsWholeText reports whether head, read by
// index.DetectTimeFormatAndHead from a file size bytes long, holds the
// whole text: it is shorter than the most detection reads, or the file
// is plain and head holds all of it.
func headIsWholeText(head []byte, size int64, kind filekind.Kind) bool {
	if len(head) < timestamps.SampleBytes {
		return true
	}
	return !kind.IsCompressed() && int64(len(head)) >= size
}
