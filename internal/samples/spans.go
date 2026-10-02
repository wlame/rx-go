package samples

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
)

// LineSpan is a run of whole lines of a file's text whose place the
// caller knows already: the numbers of its first and last line
// (1-based, inclusive) and the offset in the text where its first line
// starts. A finding of a signature scan is one.
type LineSpan struct {
	FirstLine, LastLine int64
	StartOffset         int64
}

// SpanLine is one line ReadLineSpans hands to its visitor.
type SpanLine struct {
	// Number is the line's 1-based number in the text.
	Number int64
	// Text is the line without its line ending (\n or \r\n), at most
	// SpansRequest.KeepBytes bytes of it. It is valid only during the
	// call: the next line reuses its bytes.
	Text []byte
	// Length is the whole line's length without its line ending, which
	// is more than len(Text) when the line was cut.
	Length int64
}

// SpanAction is what the visitor of ReadLineSpans wants after a line.
type SpanAction int

const (
	// SpanContinue says the span wants its next line, if it has one.
	SpanContinue SpanAction = iota
	// SpanDone says the span wants no more lines; the others go on.
	SpanDone
	// StopReading says no span wants another line: the pass ends.
	StopReading
)

// SpansRequest is the input of ReadLineSpans.
type SpansRequest struct {
	// Source is the file, pinned; every read goes through it.
	Source paths.Pinned
	// Kind is what Source is (filekind.OfPinnedForReading).
	Kind filekind.Kind
	// Spans are the runs of lines to read, in order of their first line.
	Spans []LineSpan
	// Before and After are how many lines before each span's first line
	// and after its last line the span gets too, as far as the text
	// has them.
	Before, After int
	// KeepBytes is the most bytes of each line's text handed over; a
	// longer line is cut there and its whole length reported. 0 or
	// less keeps nothing.
	KeepBytes int
	// TextLines and TextBytes, when TextLines is above 0, are the end of
	// the text the caller knows (its line count and its length in
	// bytes), for a file that may have grown since: no line past line
	// TextLines is read or handed over, and a line that runs past byte
	// TextBytes is cut there, with the length it had then. 0 reads to the
	// end of the file's text.
	TextLines, TextBytes int64
	// IndexLoader, when set, gives the file's line index: a stream then
	// starts at the checkpoint before the first span, and a seekable
	// file is entered through its frame table. Nil reads no index. The
	// lines handed over do not depend on it.
	IndexLoader IndexLoader
}

// ErrSpanPastEnd reports a span whose own lines run past the end of the
// text: the file is shorter than it was when the span was found.
var ErrSpanPastEnd = errors.New("the text ends before the span's last line")

// The sizes of a pass over a plain file: its read buffer, the block it
// reads back by to find the lines before a span, and the gap past which
// it seeks to the next span rather than read on to it.
const (
	spanPlainBuffer = 4 << 10
	spanBackBlock   = 4 << 10
	spanSeekGap     = 64 << 10
	// spanStreamBuffer is the read buffer of a compressed file.
	spanStreamBuffer = 64 << 10
)

// ReadLineSpans reads the lines of every span in one forward pass over
// the file's text and hands each line to visit with the index of the
// span it belongs to: for span i, every line of its window, from
// Before lines before its first line to After lines after its last, in
// order. A line in the windows of several spans is handed to each of
// them, in span order. visit's answer decides what follows
// (SpanAction).
//
// The reads are bounded by the spans, not by the file:
//
//   - a plain file is entered at each span's first line by a seek (the
//     offsets are known); the lines before it are found by reading back
//     from there, block by block, so they are read twice at most; a gap
//     of less than spanSeekGap is read through instead;
//   - a seekable zstd file whose index carries its frame table is
//     entered at a frame near each span;
//   - a compressed stream is decompressed from its first byte (or from
//     the index's checkpoint before the first span), and the pass stops
//     after the last span, as it does on every file.
//
// A line holds at most KeepBytes bytes in memory however long it is.
// When the text ends before a span's last line, the pass returns an
// error wrapping ErrSpanPastEnd that names the file; lines after a span
// that the end of the text cuts are simply not there. With
// req.TextLines, the end of the text is the one given: a span past it
// is the same error, and nothing past it is read or handed over. ctx
// stops the pass at its next read.
func ReadLineSpans(ctx context.Context, req SpansRequest, visit func(span int, line SpanLine) SpanAction) error {
	if len(req.Spans) == 0 {
		return nil
	}
	if req.TextLines > 0 {
		for _, s := range req.Spans {
			if s.LastLine > req.TextLines {
				return fmt.Errorf("%s: lines %d-%d: %w (the text read ends at line %d)",
					req.Source.Path(), s.FirstLine, s.LastLine, ErrSpanPastEnd, req.TextLines)
			}
		}
	}
	p := newSpanPass(ctx, req, visit)
	defer p.close()
	if err := p.run(); err != nil {
		return err
	}
	return p.checkEnds()
}

// spanWindow is one span as the pass follows it.
type spanWindow struct {
	index int
	span  LineSpan
	// first and last are the window's first and last line: the span
	// widened by Before and After, first clipped at line 1.
	first, last int64
	// next is the next line the window wants; done says it wants none.
	next int64
	done bool
}

// spanPass is the state of one ReadLineSpans call.
type spanPass struct {
	ctx     context.Context
	req     SpansRequest
	visit   func(int, SpanLine) SpanAction
	text    textSource
	plain   bool
	windows []*spanWindow
	// next is the first window the pass has not reached; active are the
	// windows it reached that still want lines.
	next   int
	active []*spanWindow
	// plainFile is a plain file's one handle, opened at the first
	// window and repositioned by seeks; nil for a compressed file.
	plainFile readSeekCloser
	// The cursor: the text from the start of line `line`, at `offset`,
	// read through lines. nil before the first window is reached.
	cursor *textCursor
	lines  *lineReader
	line   int64
	offset int64
	// buf holds the text kept of the current line.
	buf     []byte
	stopped bool
}

func newSpanPass(ctx context.Context, req SpansRequest, visit func(int, SpanLine) SpanAction) *spanPass {
	index := Request{Path: req.Source.Path(), Source: req.Source, ctx: ctx}
	index.IndexLoader = loadOnce(onlyIndexesOf(req.Source, req.IndexLoader))
	p := &spanPass{
		ctx: ctx, req: req, visit: visit,
		text:  textSourceFor(index, req.Kind),
		plain: !req.Kind.IsCompressed(),
	}
	for i, s := range req.Spans {
		first := max(1, s.FirstLine-int64(max(req.Before, 0)))
		last := s.LastLine + int64(max(req.After, 0))
		if req.TextLines > 0 {
			// SECURITY: no line past the text the caller knows: a file
			// that grew since holds lines its findings do not describe.
			last = min(last, req.TextLines)
		}
		p.windows = append(p.windows, &spanWindow{index: i, span: s, first: first, last: last, next: first})
	}
	return p
}

// close releases the cursor's file and decoder, and a plain file's
// handle.
func (p *spanPass) close() {
	p.closeCursor()
	if p.plainFile != nil {
		_ = p.plainFile.Close()
		p.plainFile = nil
	}
}

// closeCursor releases the cursor (a compressed file's decoder and its
// file; a plain file's handle stays open for the next window).
func (p *spanPass) closeCursor() {
	if p.cursor != nil {
		_ = p.cursor.close()
		p.cursor = nil
	}
}

// run reads lines until every window is done, the visitor stops the
// pass, or the text ends.
func (p *spanPass) run() error {
	for !p.stopped && (p.next < len(p.windows) || len(p.active) > 0) {
		if len(p.active) == 0 {
			if err := p.approach(p.windows[p.next]); err != nil {
				return err
			}
		}
		for p.next < len(p.windows) && p.windows[p.next].first <= p.line {
			p.active = append(p.active, p.windows[p.next])
			p.next++
		}
		if len(p.active) == 0 {
			// The next window starts further on: pass this line by.
			length, ended, err := p.lines.skip()
			if err != nil || length == 0 {
				return err
			}
			p.advance(length)
			if ended {
				return nil
			}
			continue
		}
		ended, err := p.readLine()
		if err != nil || ended {
			return err
		}
	}
	return nil
}

// readLine reads the current line, hands it to every active window that
// wants it, and moves on. ended says the text has no more lines.
func (p *spanPass) readLine() (ended bool, err error) {
	text, textLen, length, ended, err := p.readText()
	if err != nil {
		return false, err
	}
	if length == 0 {
		return true, nil
	}
	text, textLen = p.cutAtTextEnd(text, textLen)
	line := SpanLine{Number: p.line, Text: text, Length: textLen}
	kept := p.active[:0]
	for _, w := range p.active {
		if p.stopped || w.next != p.line {
			kept = append(kept, w)
			continue
		}
		switch p.visit(w.index, line) {
		case SpanDone:
			w.done = true
		case StopReading:
			w.done, p.stopped = true, true
		}
		w.next++
		if w.next > w.last {
			w.done = true
		}
		if !w.done {
			kept = append(kept, w)
		}
	}
	p.active = kept
	p.advance(length)
	return ended, nil
}

// cutAtTextEnd cuts the current line at req.TextBytes, when the caller
// gave the end of the text: the last line the caller saw, written on
// since (it had no line break then), shows the bytes the caller saw and
// the length it had.
func (p *spanPass) cutAtTextEnd(text []byte, textLen int64) ([]byte, int64) {
	if p.req.TextLines <= 0 {
		return text, textLen
	}
	if room := max(p.req.TextBytes-p.offset, 0); textLen > room {
		textLen = room
		text = text[:min(int64(len(text)), room)]
	}
	return text, textLen
}

// advance moves the cursor past a line of length bytes, its line break
// included.
func (p *spanPass) advance(length int64) {
	p.line++
	p.offset += length
}

// readText reads the current line: its text without the line ending,
// at most KeepBytes of it, the text's whole length, the line's length
// with its line break, and whether the text ends with it. A length of
// 0 means the text had no more lines.
//
// Go note: ReadSlice hands out bufio's own buffer, which the next call
// reuses, so a line longer than the buffer comes in pieces; they are
// gathered into p.buf, keeping at most KeepBytes + 2 bytes (room for a
// \r\n after a text of exactly KeepBytes), and the line's last two
// bytes are remembered across pieces to tell a \r\n ending from \n.
func (p *spanPass) readText() (text []byte, textLen, length int64, ended bool, err error) {
	keep := max(p.req.KeepBytes, 0)
	p.buf = p.buf[:0]
	var last, beforeLast byte
	for {
		chunk, rerr := p.lines.br.ReadSlice('\n')
		length += int64(len(chunk))
		if room := keep + 2 - len(p.buf); room > 0 {
			p.buf = append(p.buf, chunk[:min(len(chunk), room)]...)
		}
		for _, b := range chunk[max(0, len(chunk)-2):] {
			beforeLast, last = last, b
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(rerr, io.EOF) {
			ended = true
		} else if rerr != nil {
			return nil, 0, 0, false, rerr
		}
		break
	}
	// A \n ends the line, and a \r right before it is part of the
	// ending; a last line without a \n keeps a final \r, as a scan's
	// line view does.
	textLen = length
	if length > 0 && last == '\n' {
		textLen--
		if textLen > 0 && beforeLast == '\r' {
			textLen--
		}
	}
	return p.buf[:min(int64(len(p.buf)), int64(keep), textLen)], textLen, length, ended, nil
}

// approach brings the cursor to a line at or before w's first line,
// with no window active: it opens the text the first time, and later
// seeks forward when the source allows and the gap is wide; otherwise
// the pass reads on to w.
func (p *spanPass) approach(w *spanWindow) error {
	if p.cursor != nil && !p.worthSeeking(w) {
		return nil
	}
	cursor, err := p.openAt(w)
	if err != nil {
		return err
	}
	if !p.plain && p.cursor != nil && cursor.line <= p.line {
		// The frames cannot come closer than where the pass already is.
		// (A plain file's cursor is always taken: its handle has been
		// moved, and the line it starts at is exact.)
		_ = cursor.close()
		return nil
	}
	p.closeCursor()
	p.cursor, p.line, p.offset = cursor, cursor.line, cursor.offset
	size := spanStreamBuffer
	if p.plain {
		size = spanPlainBuffer
	}
	p.lines = newLineReader(withContext(p.ctx, cursor), size)
	return nil
}

// worthSeeking reports whether the pass should enter the text again
// near w rather than read on to it: a plain file when w's first line
// starts more than spanSeekGap bytes ahead; a seekable file read
// through its frame table when w lies more than two frames ahead, since
// entering it decodes the frame before the one holding w. A compressed
// stream can only be read on.
func (p *spanPass) worthSeeking(w *spanWindow) bool {
	if p.plain {
		return w.span.StartOffset-p.offset > spanSeekGap
	}
	frames, ok := p.text.(*seekableText)
	return ok && frames.frameHolding(w.span.StartOffset)-frames.frameHolding(p.offset) > 2
}

// openAt returns the text from a line at or before w's first line. A
// plain file is opened through its pin once per pass, and each later
// window seeks the same handle.
func (p *spanPass) openAt(w *spanWindow) (*textCursor, error) {
	if !p.plain {
		return p.text.openNear(w.span.StartOffset, int(w.span.FirstLine-w.first))
	}
	if p.plainFile == nil {
		f, err := openFileForSamples(p.req.Source)
		if err != nil {
			return nil, err
		}
		p.plainFile = f
	}
	f := p.plainFile
	offset, line := w.span.StartOffset, w.span.FirstLine
	if back := w.span.FirstLine - w.first; back > 0 {
		var err error
		if offset, line, err = lineStartBack(f, w.span.StartOffset, w.span.FirstLine, back); err != nil {
			return nil, err
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return &textCursor{Reader: f, offset: offset, line: line, close: func() error { return nil }}, nil
}

// lineStartBack finds, in a plain file, where the line `back` lines
// before line firstLine starts, reading back from start (where
// firstLine starts) by blocks of spanBackBlock: the line break at
// start-1 ends the line before firstLine, and the (back+1)-th break
// before start ends the line before the one wanted. When the file has
// fewer, the answer is its first line. It reads the bytes of those
// lines and less than one block more.
func lineStartBack(f readSeekCloser, start, firstLine, back int64) (offset, line int64, err error) {
	block := make([]byte, spanBackBlock)
	breaks := int64(0)
	for end := start; end > 0; {
		from := max(0, end-spanBackBlock)
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return 0, 0, err
		}
		piece := block[:end-from]
		if _, err := io.ReadFull(f, piece); err != nil {
			return 0, 0, fmt.Errorf("read back from byte %d: %w", end, err)
		}
		for i := len(piece) - 1; i >= 0; i-- {
			if piece[i] != '\n' {
				continue
			}
			breaks++
			if breaks == back+1 {
				return from + int64(i) + 1, firstLine - back, nil
			}
		}
		end = from
	}
	return 0, 1, nil
}

// checkEnds fails when the text ended before a span's last line: a
// window that is not done and never got that line.
func (p *spanPass) checkEnds() error {
	if p.stopped {
		return nil
	}
	for _, w := range p.windows {
		if !w.done && w.next <= w.span.LastLine {
			return fmt.Errorf("%s: lines %d-%d: %w (the file changed since it was scanned)",
				p.req.Source.Path(), w.span.FirstLine, w.span.LastLine, ErrSpanPastEnd)
		}
	}
	return nil
}
