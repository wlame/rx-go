package samples

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ResolveFromHead answers req by reading at most the first headBytes
// bytes of the file's text (the decompressed stream for a compressed
// file), without a line index. It returns answered false, with a nil
// response and error, when the answer needs text past the head; the
// caller then takes its usual path (an index build, then Resolve).
//
// It is Resolve with no index: every pass over the text starts at its
// first byte, so the answer it gives is the cold answer, which an index
// never changes (the accelerator rule). Only the read is bounded.
//
// A request is ruled out before any read when headBytes is 0 or less
// (the early answer is switched off), when it holds a position counted
// back from the end (its line needs the whole text counted), and when
// it holds an offset, or an offset range's end, at or past headBytes.
// Every other request is tried: each reader of the text stops at the
// head, and a read that would go past it ends the attempt (errPastHead).
// Whatever else Resolve returns is the answer, an error included: a
// request refused for its limits or for a time it names wrongly is
// refused now, not after a whole index build.
//
// The format detection reads at most timestamps.SampleBytes (1 MiB) of
// text, as it does in Resolve; a non-zero head of whole mebibytes, as
// RX_SAMPLES_HEAD_MB gives, holds it.
func ResolveFromHead(ctx context.Context, req Request, headBytes int64) (*rxtypes.SamplesResponse, bool, error) {
	resp, answered, _, err := ResolveFromHeadWithReach(ctx, req, headBytes, nil)
	return resp, answered, err
}

// HeadReach is how far the lines of one file's text reach into a head
// of it: what an attempt to answer from the head learns when the text
// goes on past the head. A caller that keeps it for the file (for as
// long as the file keeps its identity) can rule out, before any read,
// a later request the head is known not to hold.
type HeadReach struct {
	// Head is the size in bytes of the head it describes.
	Head int64
	// Lines is the number of lines whose line break lies inside the
	// head: lines 1 to Lines are whole in it, and every later line
	// ends past it.
	Lines int64
	// End is the text offset just past the line break of line Lines,
	// 0 when Lines is 0: every offset from End on lies in a line that
	// ends past the head.
	End int64
}

// ResolveFromHeadWithReach is ResolveFromHead for a caller that keeps
// the reach of the file's head between requests. known is the reach
// an earlier attempt returned for the same file, or nil; a reach of
// another head size is ignored.
//
// A request that needs a line past the known reach, or an offset at or
// past its End, is not answered and reads nothing. Any other request is
// tried as ResolveFromHead tries it. The reach returned is the one an
// attempt that ran past the head measured, else known when it applies,
// else nil: a reach is measured only by a pass that reads the text
// from its first byte, and a read by position (an offset in a plain
// file) measures none.
func ResolveFromHeadWithReach(
	ctx context.Context, req Request, headBytes int64, known *HeadReach,
) (resp *rxtypes.SamplesResponse, answered bool, reach *HeadReach, err error) {
	if known != nil && known.Head != headBytes {
		known = nil
	}
	if !headMayAnswer(req, headBytes) || (known != nil && pastReach(req, *known)) {
		return nil, false, known, nil
	}
	req.IndexLoader = NoIndex
	scope := &headScope{limit: headBytes}
	resp, err = Resolve(context.WithValue(ctx, headLimitKey{}, scope), req)
	if errors.Is(err, errPastHead) {
		if scope.reach != nil {
			return nil, false, scope.reach, nil
		}
		return nil, false, known, nil
	}
	return resp, true, known, err
}

// pastReach reports whether req needs text past reach: a line window
// that ends after its last line, or an offset at or past its End, or
// an offset range that ends there. A window with no line reads nothing
// and is not counted.
func pastReach(req Request, reach HeadReach) bool {
	for _, v := range req.Lines {
		if lastLineRead(v, req) > reach.Lines {
			return true
		}
	}
	for _, v := range req.Offsets {
		if v.Start >= reach.End || (v.IsRange() && *v.End >= reach.End) {
			return true
		}
	}
	return false
}

// lastLineRead returns the last line the lines pass reads for v, a
// position of a lines request that counts from the start, or 0 when it
// reads none: the arithmetic of wantedLinesOf.
func lastLineRead(v OffsetOrRange, req Request) int64 {
	if v.IsRange() {
		if *v.End < max(1, v.Start) {
			return 0
		}
		return *v.End
	}
	if v.Start <= 0 {
		return 0
	}
	first, last := contextWindow(v.Start, req)
	if last < first {
		return 0
	}
	return max(last, v.Start)
}

// headMayAnswer reports whether req is worth reading the head for:
// headBytes is positive, no position counts back from the end, and no
// offset lies at or past the head. Lines and times cannot be placed
// before the text is read; the readers stop them at the head.
func headMayAnswer(req Request, headBytes int64) bool {
	if headBytes <= 0 {
		return false
	}
	for _, v := range req.Lines {
		if v.Start < 0 {
			return false
		}
	}
	for _, v := range req.Offsets {
		if v.Start < 0 || v.Start >= headBytes || (v.IsRange() && *v.End >= headBytes) {
			return false
		}
	}
	return true
}

// errPastHead ends a Resolve under a head limit whose answer needs text
// past the head. ResolveFromHead turns it into "not answered"; it never
// leaves this package.
var errPastHead = errors.New("the answer needs text past the head")

// headLimitKey is the context key of the head limit: a *headScope
// holding the number of bytes of text from the start that a Resolve may
// read. It travels in the context because every pass over the text
// already reads through withContext, which is where the limit is
// applied.
//
// Go note: a context value is looked up by key equality, and an
// unexported struct type as the key cannot collide with a key of any
// other package.
type headLimitKey struct{}

// headScope is the head limit of one ResolveFromHeadWithReach call, and
// where the pass that runs past the head writes the reach it measured.
// The passes of one Resolve run one after another on the caller's
// goroutine, so it needs no lock.
type headScope struct {
	limit int64
	// reach is nil until a pass reading from the text's first byte
	// finds text past the head.
	reach *HeadReach
}

// headScopeOf returns the head scope ctx carries, and nil when it
// carries none.
func headScopeOf(ctx context.Context) *headScope {
	scope, _ := ctx.Value(headLimitKey{}).(*headScope)
	return scope
}

// headLimitOf returns the head limit ctx carries, and false when it
// carries none.
func headLimitOf(ctx context.Context) (int64, bool) {
	if scope := headScopeOf(ctx); scope != nil {
		return scope.limit, true
	}
	return 0, false
}

// headReader serves a text's bytes up to the head limit and no further.
//
// A read that crosses the limit is cut at it, not refused: the line
// that ends there is whole in the head. A read at the limit looks at
// the underlying reader once: io.EOF there means the whole text lies in
// the head, and is passed on as the end of the text; a byte there means
// the reader was asked for text past the head, and it answers
// errPastHead from then on.
//
// A reader that starts at the text's first byte also counts the line
// breaks it gives, and when it finds text past the head it writes the
// head's reach into scope. The count is one bytes.Count per read, over
// a buffer of kilobytes, not a step per line.
type headReader struct {
	r io.Reader
	// left is how many bytes the reader may still give.
	left int64
	// past is set once a read was asked past the head.
	past bool

	// scope receives the reach; nil when the reader does not start at
	// the text's first byte, and so cannot number the lines it gives.
	scope *headScope
	// given is how many bytes the reader has given, lines how many line
	// breaks they hold, and end the offset just past the last of them.
	given, lines, end int64
}

// Read implements io.Reader.
func (h *headReader) Read(p []byte) (int, error) {
	if h.past {
		return 0, errPastHead
	}
	if h.left <= 0 {
		return 0, h.atLimit()
	}
	if int64(len(p)) > h.left {
		p = p[:h.left]
	}
	n, err := h.r.Read(p)
	h.left -= int64(n)
	if h.scope != nil {
		h.count(p[:n])
	}
	return n, err
}

// count adds the line breaks of given, the next bytes the reader gives,
// to its tally.
func (h *headReader) count(given []byte) {
	h.lines += int64(bytes.Count(given, lineBreak))
	if last := bytes.LastIndexByte(given, '\n'); last >= 0 {
		h.end = h.given + int64(last) + 1
	}
	h.given += int64(len(given))
}

// lineBreak is the byte that ends a line, as a slice for bytes.Count.
var lineBreak = []byte{'\n'}

// maxEmptyProbes bounds how often atLimit asks a reader that returns
// neither a byte nor an error, as bufio does for its own reads.
const maxEmptyProbes = 100

// atLimit looks one byte past the head and returns io.EOF when the text
// ends there, errPastHead when it goes on, or the read's error. When
// the text goes on, a counting reader has given the whole head, so its
// tally is the head's reach.
func (h *headReader) atLimit() error {
	var probe [1]byte
	for range maxEmptyProbes {
		n, err := h.r.Read(probe[:])
		if n > 0 {
			h.past = true
			h.recordReach()
			return errPastHead
		}
		if err != nil {
			return err
		}
	}
	return io.ErrNoProgress
}

// recordReach writes the head's reach into the scope, unless the reader
// does not count or an earlier pass has written it.
func (h *headReader) recordReach() {
	if h.scope == nil || h.scope.reach != nil {
		return
	}
	h.scope.reach = &HeadReach{Head: h.scope.limit, Lines: h.lines, End: h.end}
}

// headReaderAt is a text read by position under a head limit: a read
// that ends past the limit fails with errPastHead. It wraps the text
// only when the text is longer than the limit; a shorter text lies in
// the head whole.
type headReaderAt struct {
	r     io.ReaderAt
	limit int64
}

// ReadAt implements io.ReaderAt.
func (h headReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if offset+int64(len(p)) > h.limit {
		return 0, errPastHead
	}
	return h.r.ReadAt(p, offset)
}

// limitToHead returns text, size bytes long, limited to the head that
// ctx carries, or text itself when ctx carries none or the text fits in
// the head.
func limitToHead(ctx context.Context, text io.ReaderAt, size int64) io.ReaderAt {
	limit, ok := headLimitOf(ctx)
	if !ok || size <= limit {
		return text
	}
	return headReaderAt{r: text, limit: limit}
}
