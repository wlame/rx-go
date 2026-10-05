package samples

import (
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
	if !headMayAnswer(req, headBytes) {
		return nil, false, nil
	}
	req.IndexLoader = NoIndex
	resp, err := Resolve(context.WithValue(ctx, headLimitKey{}, headBytes), req)
	if errors.Is(err, errPastHead) {
		return nil, false, nil
	}
	return resp, true, err
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

// headLimitKey is the context key of the head limit: the number of
// bytes of text from the start that a Resolve may read. It travels in
// the context because every pass over the text already reads through
// withContext, which is where the limit is applied.
//
// Go note: a context value is looked up by key equality, and an
// unexported struct type as the key cannot collide with a key of any
// other package.
type headLimitKey struct{}

// headLimitOf returns the head limit ctx carries, and false when it
// carries none.
func headLimitOf(ctx context.Context) (int64, bool) {
	limit, ok := ctx.Value(headLimitKey{}).(int64)
	return limit, ok
}

// headReader serves a text's bytes up to the head limit and no further.
//
// A read that crosses the limit is cut at it, not refused: the line
// that ends there is whole in the head. A read at the limit looks at
// the underlying reader once: io.EOF there means the whole text lies in
// the head, and is passed on as the end of the text; a byte there means
// the reader was asked for text past the head, and it answers
// errPastHead from then on.
type headReader struct {
	r io.Reader
	// left is how many bytes the reader may still give.
	left int64
	// past is set once a read was asked past the head.
	past bool
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
	return n, err
}

// maxEmptyProbes bounds how often atLimit asks a reader that returns
// neither a byte nor an error, as bufio does for its own reads.
const maxEmptyProbes = 100

// atLimit looks one byte past the head and returns io.EOF when the text
// ends there, errPastHead when it goes on, or the read's error.
func (h *headReader) atLimit() error {
	var probe [1]byte
	for range maxEmptyProbes {
		n, err := h.r.Read(probe[:])
		if n > 0 {
			h.past = true
			return errPastHead
		}
		if err != nil {
			return err
		}
	}
	return io.ErrNoProgress
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
