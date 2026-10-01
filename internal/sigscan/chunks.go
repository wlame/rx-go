package sigscan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/klauspost/compress/zstd"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/seekable"
)

// A chunk of a plain or seekable file is a nominal byte range [from, to)
// of the text. The chunk owns the lines that start in
// [aligned(from), to], where aligned(0) = 0 and otherwise aligned(from)
// is the position after the first '\n' at or after from. Two chunks next
// to each other therefore meet at one line start, wherever the nominal
// boundary falls, and neither waits for the other: the chunk before
// reads on past to until its last line ends, and the chunk after skips
// to that same line end. A line longer than a chunk swallows the chunk
// starts inside it; those chunks own no line.

// chunkPlan is one chunk of the text: its nominal range and, for a
// seekable file, the frames that hold it.
type chunkPlan struct {
	seq      int
	from, to int64
	// frame and endFrame bound the frames of a seekable chunk: frame is
	// its first, endFrame the first of the next chunk.
	frame, endFrame int
}

// textCursor yields the text from some offset on, a piece at a time,
// and io.EOF after the last byte of the text.
type textCursor interface {
	next() ([]byte, error)
}

// walkChunk feeds the lines a chunk owns to fn. It returns where the
// chunk's lines start, and final when the walk reached the end of the
// text.
func walkChunk(ctx context.Context, cur textCursor, c chunkPlan, lineBytes int, fn lineFunc) (start int64, final bool, err error) {
	pos, start := c.from, c.from
	var rest []byte
	if c.from > 0 {
		// Skip to the first line that starts after from.
		for {
			piece, err := cur.next()
			if errors.Is(err, io.EOF) {
				return pos, true, nil
			}
			if err != nil {
				return 0, false, err
			}
			if i := bytes.IndexByte(piece, '\n'); i >= 0 {
				start = pos + int64(i) + 1
				rest = piece[i+1:]
				break
			}
			pos += int64(len(piece))
			if pos >= c.to {
				// No line break before to: the chunk owns no line.
				return pos, false, nil
			}
		}
		// A start past to (the line break was at to itself) owns no
		// line either: the splitter stops before it.
	}
	sp := newSplitter(lineBytes, start, c.to, fn)
	if !sp.push(rest) {
		return start, false, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		piece, err := cur.next()
		if errors.Is(err, io.EOF) {
			sp.finish()
			return start, !sp.stopped, nil
		}
		if err != nil {
			return 0, false, err
		}
		if !sp.push(piece) {
			return start, false, nil
		}
	}
}

// walkFrom feeds the lines that start at start (a line start) and at or
// before limit to fn, until fn returns false. The merge uses it to walk
// a chunk again.
func walkFrom(cur textCursor, start, limit int64, lineBytes int, fn lineFunc) error {
	sp := newSplitter(lineBytes, start, limit, fn)
	for {
		piece, err := cur.next()
		if errors.Is(err, io.EOF) {
			sp.finish()
			return nil
		}
		if err != nil {
			return err
		}
		if !sp.push(piece) {
			return nil
		}
	}
}

// ErrFileChanged is wrapped by the error of a scan that found the file
// shorter than when the scan began: it was truncated or replaced while
// it was read.
var ErrFileChanged = errors.New("the file changed while it was scanned")

// plainCursor reads a plain file's text with ReadAt from pos on. Reads
// are readBytes long up to edge, and edgeReadBytes after it, where only
// the end of one line is still needed.
type plainCursor struct {
	r                        io.ReaderAt
	pos, size, edge          int64
	buf                      []byte
	readBytes, edgeReadBytes int
	// progressTo bounds the bytes reported as progress: those before the
	// chunk's nominal end.
	progressTo int64
	progress   *progress
}

func (c *plainCursor) next() ([]byte, error) {
	if c.pos >= c.size {
		return nil, io.EOF
	}
	n := c.readBytes
	if c.pos >= c.edge {
		n = c.edgeReadBytes
	}
	n = int(min(int64(n), c.size-c.pos))
	if cap(c.buf) < n {
		c.buf = make([]byte, n)
	}
	buf := c.buf[:n]
	got, err := c.r.ReadAt(buf, c.pos)
	if got < n {
		if err == nil || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: it ended at byte %d, and had %d bytes when the scan began", ErrFileChanged, c.pos+int64(got), c.size)
		}
		return nil, fmt.Errorf("read at %d: %w", c.pos, err)
	}
	if c.pos < c.progressTo {
		c.progress.add(min(c.pos+int64(n), c.progressTo) - c.pos)
	}
	c.pos += int64(n)
	return buf, nil
}

// seekableCursor reads a seekable zstd file's text frame by frame, from
// a position inside frame on.
type seekableCursor struct {
	r     io.ReaderAt
	table *seekable.SeekTable
	frame int
	// skip is how many bytes of the first frame come before the start.
	skip int64
	zd   *zstd.Decoder
	comp []byte
	// progressEnd is the first frame not reported as progress: frames
	// before it belong to the chunk.
	progressEnd int
	progress    *progress
}

func newSeekableCursor(r io.ReaderAt, table *seekable.SeekTable, pos int64, progressEnd int, p *progress) *seekableCursor {
	frame := frameHolding(table, pos)
	c := &seekableCursor{r: r, table: table, frame: frame, progressEnd: progressEnd, progress: p, zd: compression.AcquireDecoder()}
	if frame < table.NumFrames {
		c.skip = pos - table.Frames[frame].DecompressedOffset
	}
	return c
}

// frameHolding returns the frame whose text holds pos, or NumFrames for
// a position at or after the end of the text. Empty frames are passed
// over.
func frameHolding(table *seekable.SeekTable, pos int64) int {
	return sort.Search(table.NumFrames, func(i int) bool {
		return table.Frames[i].DecompressedEnd() > pos
	})
}

func (c *seekableCursor) next() ([]byte, error) {
	for c.frame < c.table.NumFrames {
		frame := c.table.Frames[c.frame]
		if int64(cap(c.comp)) < frame.CompressedSize {
			c.comp = make([]byte, frame.CompressedSize)
		}
		comp := c.comp[:frame.CompressedSize]
		if n, err := c.r.ReadAt(comp, frame.CompressedOffset); n < len(comp) {
			if err == nil || errors.Is(err, io.EOF) {
				err = fmt.Errorf("%w: frame %d ends past the end of the file", ErrFileChanged, frame.Index)
			}
			return nil, fmt.Errorf("read frame %d: %w", frame.Index, err)
		}
		text, err := seekable.DecodeFrame(c.zd, comp, frame)
		if err != nil {
			return nil, err
		}
		if c.frame < c.progressEnd {
			c.progress.add(frame.CompressedSize)
		}
		c.frame++
		text = text[min(c.skip, int64(len(text))):]
		c.skip = 0
		if len(text) > 0 {
			return text, nil
		}
	}
	return nil, io.EOF
}

// close returns the pooled decoder.
func (c *seekableCursor) close() {
	compression.ReleaseDecoder(c.zd)
	c.zd = nil
}
