package samples

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// collected is a samples answer while the lines are being read: the
// response, and for each key of its Samples the offset in the file's
// text where each line of that sample starts, in the same order.
//
// line_timestamps needs the starts. A line without a timestamp of its
// own carries the timestamp of an earlier line only within a distance
// in bytes (RX_TIMESTAMP_LOOKBACK_KB), and a sample line has lost its
// line break, which may have been \n or \r\n, so its length in the file
// cannot be told from its text.
//
// Go note: embedding the *rxtypes.SamplesResponse pointer promotes its
// fields, so the code that fills an answer writes resp.Samples[key] on
// a *collected as it would on the response itself.
type collected struct {
	*rxtypes.SamplesResponse
	starts map[string][]int64
	// budget counts the lines the answer holds against the request's
	// MaxLines; every path that files a line takes it from here first.
	budget *lineBudget
}

// newCollected wraps resp, whose maps are already made, for the paths
// that read lines into it, holding at most maxLines lines (0: no
// limit).
func newCollected(resp *rxtypes.SamplesResponse, maxLines int) *collected {
	return &collected{SamplesResponse: resp, starts: map[string][]int64{}, budget: &lineBudget{limit: maxLines}}
}

// ErrTooManyLines is returned by Resolve when the answer would hold
// more lines than Request.MaxLines.
var ErrTooManyLines = errors.New("too many lines for one samples answer")

// lineBudget counts the lines an answer holds against a limit; a limit
// of 0 is none.
type lineBudget struct {
	limit, held int
}

// take counts one more line, and fails when it would pass the limit.
// It is called before the line is held, so a refused answer has held
// at most the limit.
func (b *lineBudget) take() error {
	b.held++
	if b.limit > 0 && b.held > b.limit {
		return fmt.Errorf("%w: the answer reached %d lines, more than the %d allowed", ErrTooManyLines, b.held, b.limit)
	}
	return nil
}

// stampedLine is a line with a timestamp of its own: where it starts in
// the text and its timestamp in the file's frame. found is false for
// "no such line".
type stampedLine struct {
	start int64
	ms    int64
	found bool
}

// lineTimestamps returns the line_timestamps member of the answer: for
// each key of answer.Samples, the effective timestamp of each line of
// its sample, as a UTC instant in milliseconds, or nil for a line that
// has none. A key whose sample is nil maps to nil. The whole map is nil
// when the file has no timestamp format (t is nil).
//
// A line's effective timestamp is its own (index.LineStamp, as an index
// build reads it), or else the own timestamp of the nearest earlier
// line that has one, when that line starts at most
// RX_TIMESTAMP_LOOKBACK_KB KiB before this line's start. Within a
// sample the earlier lines are in hand. For a sample whose first line
// has no timestamp, the text before it is read back (stampsBefore);
// that read never uses the index, so the answer is the same with one
// and without.
func (t *fileTimes) lineTimestamps(req Request, kind filekind.Kind, answer *collected) (map[string][]*int64, error) {
	if t == nil {
		return nil, nil
	}
	lookback := config.TimestampLookbackBytes()
	var asks []int64
	for key, sample := range answer.Samples {
		if len(answer.starts[key]) != len(sample) {
			// Every path that reads a line records where it starts; a
			// sample without them is a defect, and annotating it would
			// measure distances from the wrong places.
			return nil, fmt.Errorf("samples of %s: %d line starts for the %d lines of %q",
				req.Path, len(answer.starts[key]), len(sample), key)
		}
		if len(sample) == 0 || lookback == 0 {
			continue
		}
		if _, ok := index.LineStamp(t.parser, []byte(sample[0])); !ok {
			asks = append(asks, answer.starts[key][0])
		}
	}
	before, err := t.stampsBefore(req, kind, asks, lookback)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]*int64, len(answer.Samples))
	for key, sample := range answer.Samples {
		if sample == nil {
			out[key] = nil
			continue
		}
		starts := answer.starts[key]
		var carried stampedLine
		if len(starts) > 0 {
			carried = before[starts[0]]
		}
		out[key] = t.effectiveStamps(sample, starts, carried, lookback)
	}
	return out, nil
}

// effectiveStamps returns the effective timestamp of each line of a
// sample whose lines start at starts. carried is the nearest line
// before the sample with a timestamp of its own, if it is within the
// lookback of the sample's first line.
//
// The values point into one array, so a sample costs two allocations
// however many lines it has.
func (t *fileTimes) effectiveStamps(lines []string, starts []int64, carried stampedLine, lookback int64) []*int64 {
	out := make([]*int64, len(lines))
	values := make([]int64, len(lines))
	hasZone := t.parser.Format().HasZone
	last := carried
	for i, text := range lines {
		if stamp, ok := index.LineStamp(t.parser, []byte(text)); ok {
			last = stampedLine{start: starts[i], ms: stamp.Ms, found: true}
		}
		if !last.found || starts[i]-last.start > lookback {
			continue
		}
		// A value whose instant falls outside the years 1 to 9999
		// (a wall clock at either end, read in RX_LOG_TZ) has none.
		if instant, ok := timestamps.InstantOf(last.ms, hasZone, t.logZone.Location); ok {
			values[i] = instant
			out[i] = &values[i]
		}
	}
	return out
}

// stampsBefore answers, for each line start in asks, the nearest line
// before it that has a timestamp of its own and starts at most lookback
// bytes before it, keyed by the asked start; a start with no such line
// is absent (its zero value has found false).
//
// The starts are answered in ascending order by one sweep over the
// text. Each reads the bytes from lookback before its start (one byte
// more, to tell whether that byte begins a line) up to the start, but
// never a byte an earlier start's read already covered: the stamped
// line found there is carried forward. So the sweep reads at most
// len(asks)·(lookback+1) bytes and at most the text once, however many
// keys a request names and however close together they are.
//
// Every line it reads ends before the start it reads for, which is a
// line start, so each line is in hand whole.
func (t *fileTimes) stampsBefore(req Request, kind filekind.Kind, asks []int64, lookback int64) (map[int64]stampedLine, error) {
	if len(asks) == 0 {
		return nil, nil
	}
	asks = slices.Clone(asks)
	slices.Sort(asks)
	asks = slices.Compact(asks)

	text, closeText, err := lookbackTextOf(req, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeText() }()

	found := make(map[int64]stampedLine, len(asks))
	var carried stampedLine
	// covered is where the last read ended: a line start, or -1 before
	// the first read.
	covered := int64(-1)
	var buf []byte
	for _, start := range asks {
		if err := req.context().Err(); err != nil {
			return nil, err
		}
		from := max(0, start-lookback)
		if from <= covered {
			from = covered
		} else {
			// Nothing read so far is close enough to start to matter.
			carried = stampedLine{}
		}
		if from < start {
			// atLineStart says whether from is known to begin a line;
			// when it is not, the byte before it is read too and tells.
			atLineStart := from == 0 || from == covered
			readFrom := from
			if !atLineStart {
				readFrom--
			}
			buf = slices.Grow(buf[:0], int(start-readFrom))[:start-readFrom]
			if err := readTextAt(text, buf, readFrom); err != nil {
				return nil, fmt.Errorf("read back from line start %d of %s: %w", start, req.Path, err)
			}
			carried = t.lastStampedIn(buf, readFrom, atLineStart, carried)
			covered = start
		}
		if carried.found && start-carried.start <= lookback {
			found[start] = carried
		}
	}
	return found, nil
}

// lastStampedIn returns the last line with a timestamp of its own among
// the lines that start in buf, which holds the text from offset on and
// ends at a line start; latest is returned when none has one.
//
// buf's first byte begins a line when atLineStart is true. When it is
// false, buf's first byte is the one before the first position that may
// begin a line, so the first line considered is the one after buf's
// first line break.
func (t *fileTimes) lastStampedIn(buf []byte, offset int64, atLineStart bool, latest stampedLine) stampedLine {
	pos := 0
	if !atLineStart {
		lineBreak := bytes.IndexByte(buf, '\n')
		if lineBreak < 0 {
			return latest
		}
		pos = lineBreak + 1
	}
	for pos < len(buf) {
		length := bytes.IndexByte(buf[pos:], '\n') + 1
		if length == 0 {
			// buf ends at a line start, so a line without a break here
			// means the caller's start was not one: no line to read.
			break
		}
		if stamp, ok := index.LineStamp(t.parser, buf[pos:pos+length]); ok {
			latest = stampedLine{start: offset + int64(pos), ms: stamp.Ms, found: true}
		}
		pos += length
	}
	return latest
}

// lookbackTextOf returns the text of req.Source for the read back, as a
// reader by position, and what closing it takes. It is opened through
// the samples seam, so through the pin, and it never uses the index.
//
// A plain file and a seekable zstd file with a seek table are read by
// position, so each read costs what it asks for. A stream-compressed
// file (gzip, bzip2, xz, plain zstd) cannot be entered in the middle:
// it is decompressed from its first byte, once for the whole sweep,
// dropping the bytes between the regions the sweep reads.
func lookbackTextOf(req Request, kind filekind.Kind) (io.ReaderAt, func() error, error) {
	if !readsByPosition(kind) {
		cursor, err := streamedText{src: req.Source, format: kind.Format}.openAt(0)
		if err != nil {
			return nil, nil, err
		}
		return &forwardTextAt{r: withContext(req.context(), cursor)}, cursor.close, nil
	}
	f, err := openFileForSamples(req.Source)
	if err != nil {
		return nil, nil, err
	}
	file, ok := f.(positionalFile)
	if !ok {
		_ = f.Close()
		return nil, nil, fmt.Errorf("read back in %s: the file cannot be read by position", req.Source.Path())
	}
	text, _, err := textByPosition(req.context(), file, kind)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return text, f.Close, nil
}

// readsByPosition reports whether a file of this kind can be read at any
// position of its text without decompressing what comes before: a plain
// file, or a seekable zstd file with its seek table.
func readsByPosition(kind filekind.Kind) bool {
	return !kind.IsCompressed() || (kind.IsSeekable() && kind.Table != nil)
}

// readTextAt fills buf from offset of text. A reader by position may
// report io.EOF with a full buffer at the end of the text; only a short
// read is a failure.
func readTextAt(text io.ReaderAt, buf []byte, offset int64) error {
	n, err := text.ReadAt(buf, offset)
	switch {
	case err == nil || (errors.Is(err, io.EOF) && n == len(buf)):
		return nil
	case errors.Is(err, io.EOF):
		return io.ErrUnexpectedEOF
	default:
		return err
	}
}

// errReadBehind is returned by forwardTextAt for a read before what the
// stream has already given.
var errReadBehind = errors.New("read before the position a stream has reached")

// forwardTextAt reads a stream by position, for reads at ascending
// offsets: the bytes between one read and the next are read and
// dropped. A read before the stream's position fails with errReadBehind
// rather than answering from the wrong place.
type forwardTextAt struct {
	r   io.Reader
	pos int64
}

// ReadAt implements io.ReaderAt for offsets at or after the bytes
// already read.
func (f *forwardTextAt) ReadAt(p []byte, offset int64) (int, error) {
	if offset < f.pos {
		return 0, errReadBehind
	}
	if skip := offset - f.pos; skip > 0 {
		skipped, err := io.CopyN(io.Discard, f.r, skip)
		f.pos += skipped
		if err != nil {
			return 0, err
		}
	}
	n, err := io.ReadFull(f.r, p)
	f.pos += int64(n)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}
