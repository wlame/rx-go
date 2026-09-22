package seekable

import (
	"errors"
	"fmt"
	"io"

	"github.com/wlame/rx-go/internal/compression"
)

// HeadReader reads the start of a seekable file's text, one frame after
// another in file order, each where the seek table places it. It is for
// a reader that needs only the first bytes of the text: the probe that
// decides whether a file is text, and the head its timestamp format is
// detected from.
//
// TextReader decodes each frame whole, so its memory follows the frame
// size the seek table declares, up to compression.WindowLimit a frame
// (ReadSeekTable refuses a table with a larger one). HeadReader streams
// each frame through a compression.HeadDecoder instead: memory holds
// one frame's window plus a block, whatever the frame's size, and a
// frame that declares a window above the limit is refused with an error
// wrapping compression.ErrWindowTooLarge before anything is reserved.
//
// Errors name the frame, as TextReader's do: a frame whose bytes do not
// decompress, or that ends before the length the seek table gives it,
// wraps ErrDamagedFrame; a failure to read the bytes at all is the
// read's own error. A frame read to its end must give exactly that
// length; a frame the caller stops reading part-way is checked only as
// far as it was read. The first error is returned by every later Read.
type HeadReader struct {
	r   io.ReaderAt
	tbl *SeekTable
	dec *compression.HeadDecoder

	next  int          // index of the next frame to start
	open  bool         // a frame is being read
	frame FrameInfo    // the frame being read
	left  int64        // its text not read yet, by the seek table
	src   *frameSource // its compressed bytes
	err   error        // the first error, returned by every later Read
}

// NewHeadReader returns a HeadReader over the file r whose seek table,
// read and checked by ReadSeekTable, is tbl. It refuses a frame whose
// window is above windowLimit bytes. r must be safe for ReadAt, as an
// *os.File is. The error is the decoder's (see
// compression.NewHeadDecoder).
func NewHeadReader(r io.ReaderAt, tbl *SeekTable, windowLimit uint64) (*HeadReader, error) {
	dec, err := compression.NewHeadDecoder(windowLimit)
	if err != nil {
		return nil, err
	}
	return &HeadReader{r: r, tbl: tbl, dec: dec}, nil
}

// Read copies the text into p. It starts the next frame when the text
// of the current one, by the seek table, is used up, and returns io.EOF
// after the last frame.
//
// Go note: having this method makes a *HeadReader an io.Reader.
func (h *HeadReader) Read(p []byte) (int, error) {
	if h.err != nil {
		return 0, h.err
	}
	if h.dec == nil {
		return 0, errors.New("seekable head reader: read after Close")
	}
	n, err := h.read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		h.err = err
	}
	return n, err
}

// read is Read without the sticky error.
func (h *HeadReader) read(p []byte) (int, error) {
	// Each pass either returns or moves to the next frame, so the loop
	// runs at most once per frame of the table.
	for {
		if h.open && h.left > 0 {
			if len(p) == 0 {
				return 0, nil
			}
			// Never ask for more than the frame's text, so the bytes
			// that come back are this frame's and its length is checked
			// at its end.
			n, err := h.dec.Read(p[:min(int64(len(p)), h.left)])
			h.left -= int64(n)
			frameEnded := errors.Is(err, io.EOF) && h.left == 0
			if err != nil && !frameEnded {
				return n, h.frameError(err)
			}
			if n > 0 || !frameEnded {
				return n, nil
			}
		}
		if h.open {
			if err := h.finishFrame(); err != nil {
				return 0, err
			}
			h.open = false
		}
		if h.next >= h.tbl.NumFrames {
			return 0, io.EOF
		}
		h.startFrame(h.tbl.Frames[h.next])
		h.next++
	}
}

// startFrame points the decoder at frame's compressed bytes. The
// decoder reads the frame's header on the next Read, which is where a
// frame whose window is above the limit is refused (see frameError).
func (h *HeadReader) startFrame(frame FrameInfo) {
	h.frame = frame
	h.left = frame.DecompressedSize
	h.src = &frameSource{r: io.NewSectionReader(h.r, frame.CompressedOffset, frame.CompressedSize)}
	h.open = true
	h.dec.Reset(h.src)
}

// finishFrame checks that the frame whose text was read to the length
// the seek table gives it ends there: one more read must report the end
// of the stream. The decoder checks the frame's checksum, when it has
// one, at that point too.
func (h *HeadReader) finishFrame() error {
	var extra [1]byte
	n, err := h.dec.Read(extra[:])
	switch {
	case n > 0:
		return fmt.Errorf("%w: frame %d: decompressed to more than the %d bytes the seek table says",
			ErrDamagedFrame, h.frame.Index, h.frame.DecompressedSize)
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return h.frameError(err)
	default:
		return fmt.Errorf("%w: frame %d: the decoder did not end the frame after %d bytes",
			ErrDamagedFrame, h.frame.Index, h.frame.DecompressedSize)
	}
}

// frameError names the frame in an error the decoder gave for it: the
// read's own error when reading the frame's bytes failed, the window
// refusal as it is, and ErrDamagedFrame for anything else, including a
// frame that ends early.
func (h *HeadReader) frameError(err error) error {
	frame := h.frame
	switch {
	case h.src.err != nil:
		return fmt.Errorf("read frame %d at %d: %w", frame.Index, frame.CompressedOffset, h.src.err)
	case errors.Is(err, compression.ErrWindowTooLarge):
		return fmt.Errorf("frame %d: %w", frame.Index, err)
	case errors.Is(err, io.EOF):
		return fmt.Errorf("%w: frame %d: decompressed to %d bytes, the seek table says %d",
			ErrDamagedFrame, frame.Index, frame.DecompressedSize-h.left, frame.DecompressedSize)
	default:
		return fmt.Errorf("%w: frame %d: %w", ErrDamagedFrame, frame.Index, err)
	}
}

// Close releases the decoder. It always returns nil; it does not close
// r.
func (h *HeadReader) Close() error {
	if h.dec != nil {
		_ = h.dec.Close()
		h.dec = nil
	}
	return nil
}

// frameSource reads a frame's compressed bytes and keeps the first read
// error other than the end of the frame. The decoder reports a failed
// read and a damaged frame alike, as its own error; this is how the
// reader tells them apart.
type frameSource struct {
	r   io.Reader
	err error
}

func (s *frameSource) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && s.err == nil {
		s.err = err
	}
	return n, err
}
