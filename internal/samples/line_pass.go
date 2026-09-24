package samples

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// wantedLines is one window of lines a lines-mode request asks for, and
// what the pass collected for it.
type wantedLines struct {
	// key is the window's key in the answer.
	key string
	// first and last bound the lines the sample holds, both included.
	first, last int64
	// target is the line whose start the answer reports under key in
	// Lines; 0 for a range, which reports -1.
	target int64
	// from and to are the lines the pass must read for the window:
	// first..last, widened to take in target, which a negative context
	// can leave outside it.
	from, to int64

	lines        []string
	starts       []int64
	targetOffset int64
}

// lineSource is a file's text as the lines pass reaches it: a plain
// file, a compressed stream, or a seekable zstd file with a frame table.
//
// Go note: an interface is a set of methods; plainLines, streamLines and
// frameLines below each have them, so each is a lineSource.
type lineSource interface {
	// originOf returns the number of the line openAt(line) starts at,
	// which is never after line, and whether opening there skips the
	// text before it without reading it: a seek in a plain file, a frame
	// of a seekable one. A compressed stream is decompressed from its
	// first byte whatever the line, so opening it again would read again
	// what the pass has already read.
	originOf(line int64) (origin int64, skipsUnread bool)
	// openAt returns the text from the start of line originOf(line).
	openAt(line int64) (*textCursor, error)
	// lineCount returns the number of lines of the text, for a position
	// counted back from the last line.
	lineCount(ctx context.Context) (int64, error)
	// emptyRange is the sample of a range with no line in the file. It
	// differs by format: null for a plain file (an empty list for 0-0)
	// and a seekable file read through its frame table, an empty list
	// for a compressed stream.
	emptyRange(last int64) []string
	// bufferBytes is the size of the pass's read buffer.
	bufferBytes() int
}

// lineSourceFor picks how the lines pass reads req.Source, a file of
// the given kind. A seekable zstd file whose index carries no frame
// table that describes it is streamed from its first byte: its index's
// checkpoints sit at frame starts, which are not always line starts.
func lineSourceFor(req Request, kind filekind.Kind) (lineSource, error) {
	idx := loadIndexOrNone(req)
	switch {
	case !kind.IsCompressed():
		return plainLines{src: req.Source, idx: idx}, nil
	case kind.IsSeekable():
		text, err := seekableTextFor(req.Source, idx)
		if err == nil {
			return frameLines{text: text, lines: *idx.LineCount}, nil
		}
		if !errors.Is(err, errNoFrameIndex) {
			return nil, err
		}
		return streamLines{src: req.Source, format: compression.FormatSeekableZstd}, nil
	default:
		return streamLines{src: req.Source, format: kind.Format, idx: idx}, nil
	}
}

// resolveLineWindows answers req.Lines, the lines-mode request, for a
// file of the given kind. Timestamps mode hands it the lines it found,
// so a time query reads its line and context exactly as --lines does.
//
// Every window is answered by one pass over the text, in order of the
// windows' first lines: a line before the first window is never read
// twice, however many windows the request names. Where the source can
// skip text unread (a plain file with a line index, a seekable file
// with a frame table) the pass jumps over a gap between windows to the
// checkpoint or frame before the next one; a compressed stream reads
// through the gap, which costs less than decompressing it again.
func resolveLineWindows(req Request, kind filekind.Kind, resp *collected) error {
	source, err := lineSourceFor(req, kind)
	if err != nil {
		return err
	}
	wants, err := wantedLinesOf(req, source, resp)
	if err != nil {
		return err
	}
	if err := readWantedLines(req.context(), source, wants, resp.budget); err != nil {
		return err
	}
	for _, w := range wants {
		if len(w.lines) == 0 {
			// No line of the window is in the file: the answer keeps
			// what wantedLinesOf set, -1 and the empty sample.
			continue
		}
		resp.Samples[w.key] = w.lines
		resp.starts[w.key] = w.starts
		if w.target > 0 {
			resp.Lines[w.key] = w.targetOffset
		}
	}
	return nil
}

// wantedLinesOf turns req.Lines into the windows the pass reads, keyed
// as the answer reports them, and sets each key's answer to the one it
// has when the file holds none of its lines: -1, and a null sample (or
// the source's empty range).
//
// A line counted back from the last one (-N) is resolved against the
// file's line count and keyed by the line it resolves to; line 0 is not
// a line. Two positions with the same key ask for the same window,
// which is read once.
func wantedLinesOf(req Request, source lineSource, resp *collected) ([]*wantedLines, error) {
	wants := make([]*wantedLines, 0, len(req.Lines))
	seen := make(map[string]bool, len(req.Lines))
	totalLines := int64(-1)
	for _, v := range req.Lines {
		w := &wantedLines{targetOffset: -1}
		switch {
		case v.IsRange():
			w.key, w.first, w.last = v.Key(), v.Start, *v.End
			resp.Samples[w.key] = source.emptyRange(w.last)
		case v.Start == 0:
			resp.Samples["0"] = nil
			resp.Lines["0"] = -1
			continue
		default:
			line := v.Start
			if line < 0 {
				if totalLines < 0 {
					n, err := source.lineCount(req.context())
					if err != nil {
						return nil, err
					}
					totalLines = n
				}
				line = max(1, totalLines+line+1)
			}
			w.key, w.target = strconv.FormatInt(line, 10), line
			w.first, w.last = contextWindow(line, req)
			resp.Samples[w.key] = nil
		}
		resp.Lines[w.key] = -1
		if seen[w.key] {
			continue
		}
		seen[w.key] = true
		w.first = max(1, w.first)
		if w.last < w.first {
			// A window with no line (0-0, or a context that makes its
			// end come before its start) reads nothing.
			continue
		}
		w.from, w.to = w.first, w.last
		if w.target > 0 {
			w.from, w.to = min(w.from, w.target), max(w.to, w.target)
		}
		wants = append(wants, w)
	}
	return wants, nil
}

// contextWindow returns the first and last line of the window req's
// context puts around line: last is before first when the context
// leaves no line in it.
func contextWindow(line int64, req Request) (first, last int64) {
	return max(1, line-int64(req.BeforeContext)), addWithin(line, int64(req.AfterContext))
}

// addWithin returns a+b, or the largest int64 when the sum would
// overflow: a window's end past every line is the same window.
func addWithin(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// readWantedLines reads the lines of every window in wants from source
// in one pass, in order of the windows' starts.
//
// The pass keeps the windows that hold the current line in active: a
// line costs one comparison per window it belongs to, plus one, however
// many windows the request names. A line no window holds is passed over
// without being kept (lineReader.skip), so reaching a window far into a
// file allocates nothing per line.
//
// INVARIANT: when no window is active, the cursor is at or before the
// start of the next window (originOf never returns a line after the one
// asked for), so every window sees all of its lines.
func readWantedLines(ctx context.Context, source lineSource, wants []*wantedLines, budget *lineBudget) error {
	sort.SliceStable(wants, func(i, j int) bool { return wants[i].from < wants[j].from })
	var (
		cursor  *textCursor
		lines   *lineReader
		lineNum int64 // number of the next line the cursor gives
		pos     int64 // where that line starts in the text
		next    int   // the first window not yet started
		active  []*wantedLines
	)
	defer func() {
		if cursor != nil {
			_ = cursor.close()
		}
	}()
	for next < len(wants) || len(active) > 0 {
		if len(active) == 0 {
			origin, skipsUnread := source.originOf(wants[next].from)
			if cursor == nil || (skipsUnread && origin > lineNum) {
				if cursor != nil {
					_ = cursor.close()
				}
				opened, err := source.openAt(wants[next].from)
				if err != nil {
					return err
				}
				cursor, lineNum, pos = opened, opened.line, opened.offset
				lines = newLineReader(withContext(ctx, cursor), source.bufferBytes())
			}
		}
		if len(active) == 0 && wants[next].from > lineNum {
			length, ended, err := lines.skip()
			if err != nil {
				return err
			}
			if length == 0 {
				return nil
			}
			pos, lineNum = pos+length, lineNum+1
			if ended {
				return nil
			}
			continue
		}

		raw, length, ended, err := lines.readUpTo(budget.keepPerLine())
		if err != nil {
			return err
		}
		if length == 0 {
			return nil
		}
		for next < len(wants) && wants[next].from <= lineNum {
			active = append(active, wants[next])
			next++
		}
		if err := collectLine(active, raw, length, lineNum, pos, budget); err != nil {
			return err
		}
		// Keep the windows that hold lines after this one. Filtering
		// into active[:0] reuses the slice's array: each kept window is
		// written at or before the place it is read from.
		kept := active[:0]
		for _, w := range active {
			if lineNum < w.to {
				kept = append(kept, w)
			}
		}
		active = kept
		pos, lineNum = pos+length, lineNum+1
		if ended {
			return nil
		}
	}
	return nil
}

// collectLine files one line, number lineNum starting at pos, in every
// active window that holds it, taking each from budget first. raw is
// the line as read, length bytes long, or cut shorter when the budget
// could not hold it (lineReader.readUpTo); then taking it fails before
// the cut text is used. Its text is made once and shared: a Go string
// never changes, so windows that overlap hold one copy.
func collectLine(active []*wantedLines, raw []byte, length, lineNum, pos int64, budget *lineBudget) error {
	var text string
	made := false
	textBytes := textLength(raw, length)
	for _, w := range active {
		if lineNum == w.target {
			w.targetOffset = pos
		}
		if lineNum < w.first || lineNum > w.last {
			continue
		}
		if err := budget.take(textBytes); err != nil {
			return err
		}
		if !made {
			text, made = string(trimOneLineBreak(raw)), true
		}
		w.lines = append(w.lines, text)
		w.starts = append(w.starts, pos)
	}
	return nil
}

// textLength is the length of a line's text without its line break: of
// raw when raw holds the whole line, else length, the whole line's,
// which is past any limit raw was cut to.
func textLength(raw []byte, length int64) int64 {
	if int64(len(raw)) < length {
		return length
	}
	return int64(len(trimOneLineBreak(raw)))
}

// trimOneLineBreak drops a trailing \n and one \r before it: a sample
// line is the line without its line break, \n or \r\n.
func trimOneLineBreak(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return b
}

// lineReader reads a text line by line through a buffer of fixed size.
type lineReader struct {
	br *bufio.Reader
	// long holds a line longer than br's buffer while it is read whole.
	long []byte
}

// newLineReader returns a lineReader over r with a read buffer of size
// bytes.
func newLineReader(r io.Reader, size int) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, size)}
}

// readUpTo returns the next line with its line break, its length, and
// whether the text ends with it. It holds at most keep bytes of the
// line (0: all of it): a longer line comes back cut to keep bytes, its
// rest passed over without being held, and its whole length tells the
// caller so. The bytes are valid until the next call. A length of 0
// with ended true means the text had no more lines: a text that ends
// with a line break gives a final empty read, and that is not a line.
func (l *lineReader) readUpTo(keep int64) (line []byte, length int64, ended bool, err error) {
	chunk, err := l.br.ReadSlice('\n')
	length = int64(len(chunk))
	if errors.Is(err, bufio.ErrBufferFull) {
		// ReadSlice hands out its buffer, which the next call reuses,
		// so a line longer than the buffer is gathered in l.long, up
		// to keep bytes of it.
		l.long = append(l.long[:0], chunk...)
		for errors.Is(err, bufio.ErrBufferFull) {
			chunk, err = l.br.ReadSlice('\n')
			length += int64(len(chunk))
			if room := keep - int64(len(l.long)); keep == 0 || room > 0 {
				if keep != 0 && int64(len(chunk)) > room {
					chunk = chunk[:room]
				}
				l.long = append(l.long, chunk...)
			}
		}
		chunk = l.long
	}
	if keep != 0 && int64(len(chunk)) > keep {
		chunk = chunk[:keep]
	}
	switch {
	case err == nil:
		return chunk, length, false, nil
	case errors.Is(err, io.EOF):
		return chunk, length, true, nil
	default:
		return nil, 0, false, err
	}
}

// skip passes over the next line without keeping it, and returns its
// length with its line break and whether the text ends with it. A
// length of 0 means the text had no more lines.
func (l *lineReader) skip() (length int64, ended bool, err error) {
	for {
		chunk, err := l.br.ReadSlice('\n')
		length += int64(len(chunk))
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == nil:
			return length, false, nil
		case errors.Is(err, io.EOF):
			return length, true, nil
		default:
			return 0, false, err
		}
	}
}

// ============================================================================
// The three line sources
// ============================================================================

// plainLines is a plain file's text. With a line index a window starts
// at the checkpoint at or before its first line, reached by a seek.
type plainLines struct {
	src paths.Pinned
	idx *rxtypes.UnifiedFileIndex
}

func (p plainLines) originOf(line int64) (int64, bool) {
	_, origin := chooseSeekOrigin(p.idx, line)
	return origin, true
}

func (p plainLines) openAt(line int64) (*textCursor, error) {
	offset, origin := chooseSeekOrigin(p.idx, line)
	f, err := openFileForSamples(p.src)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return &textCursor{Reader: f, offset: offset, line: origin, close: f.Close}, nil
}

func (p plainLines) lineCount(ctx context.Context) (int64, error) {
	if p.idx != nil && p.idx.LineCount != nil && *p.idx.LineCount > 0 {
		return *p.idx.LineCount, nil
	}
	return countLines(ctx, p.src)
}

func (plainLines) emptyRange(last int64) []string {
	if last == 0 {
		return []string{}
	}
	return nil
}

// bufferBytes is small: a window a few lines long should not read far
// past its last line, which the bounded-read tests measure.
func (plainLines) bufferBytes() int { return 4 * 1024 }

// streamLines is a compressed stream's text (gzip, bzip2, xz, zstd, or
// a seekable zstd file read without its frame table). It is
// decompressed from the first byte; with a line index the bytes before
// the checkpoint are dropped without being split into lines.
type streamLines struct {
	src    paths.Pinned
	format compression.Format
	idx    *rxtypes.UnifiedFileIndex
}

func (s streamLines) originOf(line int64) (int64, bool) {
	_, origin := chooseSeekOrigin(s.idx, line)
	return origin, false
}

func (s streamLines) openAt(line int64) (*textCursor, error) {
	return openStreamAtLine(s.src, s.format, s.idx, line)
}

func (s streamLines) lineCount(ctx context.Context) (int64, error) {
	if s.idx != nil && s.idx.LineCount != nil {
		return *s.idx.LineCount, nil
	}
	return streamCountLines(ctx, s.src, s.format)
}

func (streamLines) emptyRange(int64) []string { return []string{} }

func (streamLines) bufferBytes() int { return 64 * 1024 }

// frameLines is a seekable zstd file's text read through the frame
// table of its index: a window starts in a frame before the one holding
// its first line, so a line costs a frame or two whatever the file's
// size. lines is the file's line count, which the table agrees with
// (seekableTextFor).
type frameLines struct {
	text  *seekableText
	lines int64
}

func (f frameLines) originOf(line int64) (int64, bool) {
	return f.text.lineAfterFirstBreak(f.text.frameBeforeLine(line)), true
}

func (f frameLines) openAt(line int64) (*textCursor, error) {
	return f.text.cursorAtFrame(f.text.frameBeforeLine(line))
}

func (f frameLines) lineCount(context.Context) (int64, error) { return f.lines, nil }

func (frameLines) emptyRange(int64) []string { return nil }

func (frameLines) bufferBytes() int { return 64 * 1024 }

// contextReader is an io.Reader that stops once its context is done.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

// withContext returns r reading only while ctx is not done: each Read
// first checks ctx and returns its error once it is canceled. The
// passes read through a buffer of kilobytes, so the check costs nothing
// next to the read, and a canceled request stops at the next buffer.
//
// When ctx carries a head limit (ResolveFromHead), the reader also
// stops at the head: it gives the text up to the limit and answers
// errPastHead to a read past it (headReader). r is a pass's
// *textCursor, which says where in the text it starts, or a reader that
// starts at the text's first byte.
func withContext(ctx context.Context, r io.Reader) io.Reader {
	reader := contextReader{ctx: ctx, r: r}
	scope := headScopeOf(ctx)
	if scope == nil {
		return reader
	}
	var start int64
	if cursor, isCursor := r.(*textCursor); isCursor {
		start = cursor.offset
	}
	head := &headReader{r: reader, left: scope.limit - start}
	if start == 0 {
		// Only a reader from the first byte can number the lines it
		// gives, so only it measures the head's reach.
		head.scope = scope
	}
	return head
}

// Read implements io.Reader.
func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
