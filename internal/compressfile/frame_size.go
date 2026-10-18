package compressfile

import "fmt"

// MinFrameSize is the smallest frame size a caller may ask for, in
// bytes. A frame holds at least one byte of text; a smaller "frame" has
// no meaning, and the encoder would replace it with its default without
// a word (Options.FrameSize: zero means the default). Both parsers of a
// frame size, `rx compress --frame-size` and the frame_size field of
// POST /v1/compress, refuse such a value before the encoder sees it.
const MinFrameSize = 1

// ErrFrameSizeBelowMinimum is the error for a frame size below
// MinFrameSize. A parser wraps it with the name of its flag or field
// (`--frame-size must be at least 1 byte`, `frame_size must be at least
// 1 byte`), and a caller tells it from a value that could not be read
// with errors.Is: the HTTP handler answers 422 for a size out of range
// and 400 for one it cannot read, as it does for every other field.
var ErrFrameSizeBelowMinimum = fmt.Errorf("must be at least %d byte", MinFrameSize)

// CheckFrameSize returns ErrFrameSizeBelowMinimum when bytes is below
// MinFrameSize, and nil otherwise.
func CheckFrameSize(bytes int64) error {
	if bytes < MinFrameSize {
		return ErrFrameSizeBelowMinimum
	}
	return nil
}
