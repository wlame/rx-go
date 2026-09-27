package samples

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
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
	// budget counts the lines the answer holds, and their bytes,
	// against the request's MaxLines and MaxBytes; every path that
	// files a line takes it from here first.
	budget *lineBudget
}

// newCollected wraps resp, whose maps are already made, for the paths
// that read lines into it, holding at most req's MaxLines lines and
// MaxBytes bytes of text (0: no limit).
func newCollected(resp *rxtypes.SamplesResponse, req Request) *collected {
	budget := &lineBudget{lineLimit: req.MaxLines, byteLimit: req.MaxBytes}
	return &collected{SamplesResponse: resp, starts: map[string][]int64{}, budget: budget}
}

// ErrTooManyLines is returned by Resolve when the answer would hold
// more lines than Request.MaxLines.
var ErrTooManyLines = errors.New("too many lines for one samples answer")

// ErrTooManyBytes is returned by Resolve when the answer would hold
// more bytes of line text than Request.MaxBytes.
var ErrTooManyBytes = errors.New("too many bytes for one samples answer")

// lineBudget counts the lines an answer holds, and the bytes of their
// text, against two limits; a limit of 0 is none.
type lineBudget struct {
	lineLimit, lines int
	byteLimit, bytes int64
}

// take counts one more line of textBytes bytes, and fails when it would
// pass either limit. It is called before the line is held, so a refused
// answer has held at most the limits.
func (b *lineBudget) take(textBytes int64) error {
	b.lines++
	b.bytes += textBytes
	if b.lineLimit > 0 && b.lines > b.lineLimit {
		return fmt.Errorf("%w: the answer reached %d lines, more than the %d allowed", ErrTooManyLines, b.lines, b.lineLimit)
	}
	if b.byteLimit > 0 && b.bytes > b.byteLimit {
		return fmt.Errorf("%w: the answer reached %d bytes of line text, more than the %d allowed", ErrTooManyBytes, b.bytes, b.byteLimit)
	}
	return nil
}

// keepPerLine is how much of one line a pass needs to hold to file it:
// the byte limit and a line break of up to two bytes (\r\n), or 0, all
// of it, without a byte limit. A longer line's text alone passes the
// limit, so it can only be refused, and holding more of it would only
// cost memory.
func (b *lineBudget) keepPerLine() int64 {
	if b.byteLimit <= 0 {
		return 0
	}
	return b.byteLimit + 2
}

// stampedLine is a line with a timestamp of its own: where it starts in
// the text and its timestamp in the file's frame. found is false for
// "no such line".
//
// A line of the text a log chain reads before the file (Request.Earlier)
// starts before the file's first byte, at a negative start, and carries
// its timestamp as a UTC instant already (isInstant), read in its own
// file's frame: instant, or none when instantOK is false.
type stampedLine struct {
	start int64
	ms    int64
	found bool

	isInstant bool
	instant   int64
	instantOK bool
}

// Earlier is the text a log chain reads before a file, for the read back
// of line_timestamps (Request.Earlier). In the chain read as one text, a
// line without a timestamp of its own at the start of a part (a
// traceback continued from the part before) carries the timestamp of a
// line near the end of the part before it; the read back within the file
// reaches the file's first byte and continues here.
//
// Go note: an interface lets this package call into the chain's reader
// without importing it; the log chain package implements it.
type Earlier interface {
	// LastStamp returns the nearest line before the file with a
	// timestamp of its own, when it starts at most within bytes before
	// the file's first byte in the text the files make together (a line
	// break counted after a file that ends without one), and found false
	// otherwise. It may return a stamp whose Start is one byte too late
	// (OneEarlier).
	LastStamp(ctx context.Context, within int64) (EarlierStamp, error)
	// Settle returns stamp, from LastStamp, with Start exact and
	// OneEarlier false: it reads whether the text before the file ends
	// with a line break. It is called only when a line's distance from
	// the stamp decides on that byte.
	Settle(ctx context.Context, stamp EarlierStamp) (EarlierStamp, error)
}

// EarlierStamp is a line before a file with a timestamp of its own, as
// Earlier finds it.
type EarlierStamp struct {
	// Found says that such a line starts within the distance asked.
	Found bool
	// Start is where the line starts, counted from the file's first byte
	// in the text the files make together: a negative number.
	Start int64
	// OneEarlier says that the line may start one byte before Start:
	// the text before the file may end without a line break, and the
	// text read as one adds one after it.
	OneEarlier bool
	// Instant is the line's own timestamp as a UTC instant, read in its
	// own file's frame; InstantOK is false when it has none in the years
	// 1 to 9999.
	Instant   int64
	InstantOK bool
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
	before, reachedStart, err := t.stampsBefore(req, kind, asks, lookback)
	if err != nil {
		return nil, err
	}
	earlier := &earlierCarry{source: req.Earlier, ctx: withoutHeadLimit(req.context())}
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
			if !carried.found && reachedStart[starts[0]] {
				// The read back within the file reached its first byte
				// with distance left and found nothing: it goes on into
				// the text before the file, if there is one.
				if carried, err = earlier.carry(t, sample, starts, lookback); err != nil {
					return nil, err
				}
			}
		}
		out[key] = t.effectiveStamps(sample, starts, carried, lookback)
	}
	return out, nil
}

// earlierCarry fetches the stamp of the text before a file (Earlier) at
// most once per answer, and settles it at most once: every sample whose
// read back reaches the file's first byte carries the same line.
type earlierCarry struct {
	source Earlier
	// ctx is the answer's context without its head limit: the earlier
	// text is another file, which the limit on this file's head does
	// not bound.
	ctx context.Context

	fetched, settled bool
	stamp            EarlierStamp
}

// carry returns the stamped line a sample whose first lines have no
// timestamp of their own carries from the text before the file: the
// nearest earlier line with one, when it starts at most lookback bytes
// before the sample's first line; none otherwise, or without an
// earlier text.
//
// When the stamp's start may be one byte earlier than it says, and a
// line of the sample's leading run (the lines before its first own
// timestamp) lies exactly lookback bytes after the said start, that
// byte decides whether the line carries the stamp: the stamp is settled
// first, which reads the end of the earlier text. Any other line is
// decided either way by the said start.
func (e *earlierCarry) carry(t *fileTimes, sample []string, starts []int64, lookback int64) (stampedLine, error) {
	if e.source == nil {
		return stampedLine{}, nil
	}
	if !e.fetched {
		stamp, err := e.source.LastStamp(e.ctx, lookback)
		if err != nil {
			return stampedLine{}, err
		}
		e.stamp, e.fetched = stamp, true
	}
	if !e.stamp.Found {
		return stampedLine{}, nil
	}
	if e.stamp.OneEarlier && !e.settled && t.leadingRunMeets(sample, starts, e.stamp.Start+lookback) {
		stamp, err := e.source.Settle(e.ctx, e.stamp)
		if err != nil {
			return stampedLine{}, err
		}
		e.stamp, e.settled = stamp, true
	}
	if starts[0]-e.stamp.Start > lookback {
		return stampedLine{}, nil
	}
	return stampedLine{
		start: e.stamp.Start, found: true,
		isInstant: true, instant: e.stamp.Instant, instantOK: e.stamp.InstantOK,
	}, nil
}

// leadingRunMeets reports whether a line of the sample's leading run,
// the lines before its first line with a timestamp of its own, starts
// exactly at offset.
func (t *fileTimes) leadingRunMeets(sample []string, starts []int64, offset int64) bool {
	for i, text := range sample {
		if starts[i] > offset {
			return false
		}
		if _, ok := index.LineStamp(t.parser, []byte(text)); ok {
			return false
		}
		if starts[i] == offset {
			return true
		}
	}
	return false
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
	last := carried
	for i, text := range lines {
		if stamp, ok := index.LineStamp(t.parser, []byte(text)); ok {
			last = stampedLine{start: starts[i], ms: stamp.Ms, found: true}
		}
		if !last.found || starts[i]-last.start > lookback {
			continue
		}
		// A value whose instant falls outside the years 1 to 9999
		// (a wall clock at either end, read in its zone) has none.
		if instant, ok := last.instantIn(t.frame); ok {
			values[i] = instant
			out[i] = &values[i]
		}
	}
	return out
}

// instantIn returns the line's timestamp as a UTC instant: its value
// read in frame, or the instant it carries from the text before the
// file. ok is false when it has none in the years 1 to 9999.
func (s stampedLine) instantIn(frame timeFrame) (int64, bool) {
	if s.isInstant {
		return s.instant, s.instantOK
	}
	return frame.instant(s.ms)
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
//
// reachedStart holds the asked starts less than lookback bytes into the
// file whose read found no timestamped line: their look back reaches
// before the file's first byte, where a log chain's earlier parts go on
// (Request.Earlier).
//
// A read that fails (a damaged frame of a seekable file that the lines
// themselves did not need) leaves its start absent rather than failing
// the answer: that sample's lines without a timestamp of their own get
// none, which line_timestamps spells null, "not known". The failure is
// logged once per answer. A canceled request is the answer's error.
func (t *fileTimes) stampsBefore(req Request, kind filekind.Kind, asks []int64, lookback int64) (found map[int64]stampedLine, reachedStart map[int64]bool, err error) {
	if len(asks) == 0 {
		return nil, nil, nil
	}
	asks = slices.Clone(asks)
	slices.Sort(asks)
	asks = slices.Compact(asks)

	text, closeText, err := lookbackTextOf(req, kind, noDecodeLimit)
	if err != nil {
		return nil, nil, readBackFailed(req, len(asks), err)
	}
	defer func() { _ = closeText() }()

	found = make(map[int64]stampedLine, len(asks))
	reachedStart = map[int64]bool{}
	var (
		failures  int
		firstFail error
	)
	var carried stampedLine
	// covered is where the last read ended: a line start, or -1 before
	// the first read.
	covered := int64(-1)
	var buf []byte
	for _, start := range asks {
		if err := req.context().Err(); err != nil {
			return nil, nil, err
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
				if ctxErr := req.context().Err(); ctxErr != nil {
					return nil, nil, ctxErr
				}
				if errors.Is(err, errPastHead) {
					// Under a head limit a read past the head is not a
					// damaged frame: the answer cannot be given from
					// the head, so the attempt ends (ResolveFromHead).
					return nil, nil, err
				}
				if failures++; firstFail == nil {
					firstFail = fmt.Errorf("read back from line start %d: %w", start, err)
				}
				// Nothing is known to have been read: the next start
				// reads its own lookback whole.
				carried, covered = stampedLine{}, -1
				continue
			}
			carried = t.lastStampedIn(buf, readFrom, atLineStart, carried)
			covered = start
		}
		switch {
		case carried.found && start-carried.start <= lookback:
			found[start] = carried
		case start < lookback:
			// The look back reaches before the file's first byte, and
			// the text from there to start was read whole (a failed
			// read continues above) without a timestamped line.
			reachedStart[start] = true
		}
	}
	if failures > 0 {
		_ = readBackFailed(req, failures, firstFail)
	}
	return found, reachedStart, nil
}

// readBackFailed logs that the read back for line_timestamps failed for
// samples of the answer, and returns the request's error when it was
// canceled (the answer fails with it), else nil (those samples carry no
// earlier timestamp).
func readBackFailed(req Request, samples int, err error) error {
	if ctxErr := req.context().Err(); ctxErr != nil {
		return ctxErr
	}
	slog.Default().Warn("line_timestamps_read_back_failed",
		"path", req.Path, "samples", samples, "error", err.Error())
	return nil
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
//
// decodeLimit bounds the text a seekable file's frames decode to, in
// all (textByPosition); noDecodeLimit decodes what the reads need.
func lookbackTextOf(req Request, kind filekind.Kind, decodeLimit int64) (io.ReaderAt, func() error, error) {
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
	text, _, err := textByPosition(req.context(), file, kind, decodeLimit)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return text, f.Close, nil
}

// ErrReadLimit is returned by EndsWithLineBreak when the last byte of
// the text lies past what its read limit lets it decode: a
// stream-compressed text longer than the limit, or a seekable file whose
// frame that holds the byte would take the text decoded past it. Nothing
// of that text was decoded.
var ErrReadLimit = errors.New("the end of the text lies past what the read may decode")

// EndsWithLineBreak reports whether the text of req's file, textLen
// bytes long (its decompressed stream for a compressed file), ends with
// a line break: a log chain reads its parts as one text and adds one
// after a part that ends without one. An empty text ends with none. It
// reads the last byte through the read back's text (lookbackTextOf),
// through req's pin: by position in a plain or seekable file, and a
// stream-compressed file decompressed to its end.
//
// SECURITY: readLimit, when it is above 0, bounds the text the read may
// decode. A plain file is read one byte. A stream-compressed text longer
// than the limit is not opened at all, and a seekable file's frame is
// not decoded when its text would take what was decoded past the limit
// (textByPosition's decode limit). Either refusal is ErrReadLimit. 0 is
// no limit: `rx logs samples`, which runs as the user's own process,
// reads what the answer needs.
func EndsWithLineBreak(ctx context.Context, req Request, textLen, readLimit int64) (bool, error) {
	if textLen <= 0 {
		return false, nil
	}
	req, kind, err := prepare(ctx, req)
	if err != nil {
		return false, err
	}
	if readLimit > 0 && !readsByPosition(kind) && textLen > readLimit {
		return false, fmt.Errorf("%w: %s is stream-compressed and its text of %d bytes is longer than the limit of %d",
			ErrReadLimit, req.Path, textLen, readLimit)
	}
	text, closeText, err := lookbackTextOf(req, kind, readLimit)
	if err != nil {
		return false, err
	}
	defer func() { _ = closeText() }()
	var last [1]byte
	if err := readTextAt(text, last[:], textLen-1); err != nil {
		if errors.Is(err, errDecodeLimit) {
			return false, fmt.Errorf("%w: %w", ErrReadLimit, err)
		}
		return false, err
	}
	return last[0] == '\n', nil
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
