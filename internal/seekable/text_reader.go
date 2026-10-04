package seekable

import (
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/wlame/rx-go/internal/compression"
)

// TextReader reads a seekable file's text from its first byte, one
// frame after another in file order, through the seek table. It is
// what a plain zstd decoder gives for an intact file, but it reads each
// frame where the table places it, checks it with DecodeFrame, and
// stops at a damaged frame with an error that wraps ErrDamagedFrame and
// names the frame, after the text of the frames before it.
//
// Memory holds one frame's text at a time. Close returns the pooled
// decoder; it does not close r.
type TextReader struct {
	r    io.ReaderAt
	tbl  *SeekTable
	zd   *zstd.Decoder
	next int    // index of the next frame to decode
	text []byte // what is left of the frame decoded last
	buf  []byte // the compressed bytes of a frame, reused
}

// NewTextReader returns a TextReader over the file r whose seek table,
// read and checked by ReadSeekTable, is tbl. r must be safe for ReadAt,
// as an *os.File is.
func NewTextReader(r io.ReaderAt, tbl *SeekTable) *TextReader {
	return &TextReader{r: r, tbl: tbl, zd: compression.AcquireDecoder()}
}

// Read copies the text into p. It decodes the next frame when the text
// of the last one is used up, and returns io.EOF after the last frame.
//
// Go note: having this method makes a *TextReader an io.Reader.
func (t *TextReader) Read(p []byte) (int, error) {
	for len(t.text) == 0 {
		if t.next >= t.tbl.NumFrames {
			return 0, io.EOF
		}
		if t.zd == nil {
			return 0, fmt.Errorf("seekable text reader: read after Close")
		}
		frame := t.tbl.Frames[t.next]
		if int64(cap(t.buf)) < frame.CompressedSize {
			t.buf = make([]byte, frame.CompressedSize)
		}
		t.buf = t.buf[:frame.CompressedSize]
		if _, err := t.r.ReadAt(t.buf, frame.CompressedOffset); err != nil {
			return 0, fmt.Errorf("read frame %d at %d: %w", frame.Index, frame.CompressedOffset, err)
		}
		text, err := DecodeFrame(t.zd, t.buf, frame)
		if err != nil {
			return 0, err
		}
		t.text = text
		t.next++
	}
	n := copy(p, t.text)
	t.text = t.text[n:]
	return n, nil
}

// Close returns the decoder to the pool. It always returns nil.
func (t *TextReader) Close() error {
	compression.ReleaseDecoder(t.zd)
	t.zd = nil
	return nil
}
