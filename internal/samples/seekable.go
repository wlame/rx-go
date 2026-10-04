package samples

import (
	"errors"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
)

// errNoFrameIndex says the file has no usable frame table, so the caller
// falls back to streaming the whole archive. It never reaches a user:
// the answer is the same either way, only slower.
var errNoFrameIndex = errors.New("samples: no frame index for this file")

// decodeSeekableFrame decompresses one frame of a seekable file, read
// through its pin. It is a variable so a test can count the frames a
// request decodes.
var decodeSeekableFrame = func(d *seekable.Decoder, src paths.Pinned, frame int, table *seekable.SeekTable) ([]byte, error) {
	f, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return d.DecompressFrameAt(f, frame, table)
}
