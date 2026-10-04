package compression

import (
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// ErrWindowTooLarge reports a zstd frame that needs more window than a
// head reader allows.
var ErrWindowTooLarge = errors.New("zstd frame needs a larger window than a head read allows")

// windowRefusals are the errors klauspost's decoder gives for a frame
// whose window is above its limits: the window a frame header declares
// (ErrWindowSizeExceeded), and the content size a single-segment frame
// declares, which is its window (ErrDecoderSizeExceeded).
var windowRefusals = []error{zstd.ErrWindowSizeExceeded, zstd.ErrDecoderSizeExceeded}

// HeadDecoder decodes the start of a zstd stream in bounded memory, for
// a reader that needs only the first bytes of a file's text: the probe
// that decides whether a file is text, and the head its timestamp
// format is detected from.
//
// A zstd decoder reserves a frame's whole window before it decodes the
// frame's first block. The size comes from the file: the window the
// frame header declares, or, for a single-segment frame, the content
// size it declares. A file of a few kilobytes can therefore ask for
// gigabytes. A HeadDecoder refuses a frame whose window is above its
// limit before it reserves anything, with an error wrapping
// ErrWindowTooLarge; for any other frame it holds one window plus a
// block, however much text the frame holds.
//
// It decodes on the calling goroutine, so it starts no goroutine that
// Close would have to stop. It is not safe for use by several
// goroutines.
type HeadDecoder struct {
	zd *zstd.Decoder
}

// NewHeadDecoder returns a HeadDecoder that refuses a frame whose
// window is above windowLimit bytes. Reset gives it the stream to read.
// The error is the decoder's, for a limit below zstd's 1 KiB minimum
// window.
func NewHeadDecoder(windowLimit uint64) (*HeadDecoder, error) {
	zd, err := zstd.NewReader(nil,
		// One goroutine: the caller's. A concurrent decoder would decode
		// blocks ahead of the reader, which a head read does not need.
		zstd.WithDecoderConcurrency(1),
		// Reserve the window plus half a block, not twice the window.
		zstd.WithDecoderLowmem(true),
		// The limit on a declared window...
		zstd.WithDecoderMaxWindow(windowLimit),
		// ...and the limit the decoder holds a single-segment frame's
		// declared content size to, which the window limit does not
		// cover. It does not limit how much text a frame with a
		// declared window gives.
		zstd.WithDecoderMaxMemory(windowLimit),
	)
	if err != nil {
		return nil, fmt.Errorf("create zstd head decoder: %w", err)
	}
	return &HeadDecoder{zd: zd}, nil
}

// Reset starts decoding the stream src, from its first frame. It reads
// the first frame's header, so a frame whose window is above the limit
// is refused here, with an error wrapping ErrWindowTooLarge.
func (h *HeadDecoder) Reset(src io.Reader) error {
	// The decoder decodes a *bytes.Buffer or another source that hands
	// out its bytes whole in one call, without the window limit's
	// stream checks. A wrapper that is only an io.Reader keeps every
	// source on the streaming path.
	return windowError(h.zd.Reset(streamOnly{src}))
}

// Read copies decoded text into p. It returns io.EOF at the end of the
// stream, and an error wrapping ErrWindowTooLarge at a later frame
// whose window is above the limit.
//
// Go note: having this method makes a *HeadDecoder an io.Reader.
func (h *HeadDecoder) Read(p []byte) (int, error) {
	n, err := h.zd.Read(p)
	return n, windowError(err)
}

// Close releases the decoder. It always returns nil, so a
// *HeadDecoder is an io.ReadCloser.
func (h *HeadDecoder) Close() error {
	h.zd.Close()
	return nil
}

// windowError wraps a window refusal of the decoder in
// ErrWindowTooLarge and returns any other error, io.EOF included, as
// it is.
func windowError(err error) error {
	for _, refusal := range windowRefusals {
		if errors.Is(err, refusal) {
			return fmt.Errorf("%w: %w", ErrWindowTooLarge, err)
		}
	}
	return err
}

// streamOnly hides every method of a reader except Read.
type streamOnly struct{ r io.Reader }

func (s streamOnly) Read(p []byte) (int, error) { return s.r.Read(p) }
