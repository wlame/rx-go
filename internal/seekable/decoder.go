package seekable

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"

	"github.com/wlame/rx-go/internal/compression"
)

// ErrFrameIndexOutOfRange is returned when caller asks for a frame
// that doesn't exist in the seek table.
var ErrFrameIndexOutOfRange = errors.New("frame index out of range")

// ErrDamagedFrame reports that a frame's bytes were read but do not
// decompress into the text the seek table says the frame holds: the
// zstd decoder refused them, or they gave a different number of bytes.
// Archives get damaged by partial copies, bad sectors and interrupted
// uploads, usually in one place, and the frames around a damaged one
// still decompress, since every frame is independent.
//
// A failure to read the bytes at all is not this error: it comes back
// as the read's own error.
var ErrDamagedFrame = errors.New("seekable zstd frame is damaged")

// DecodeFrame decompresses compressed, the bytes of frame as the seek
// table places them, with zd, a decoder from compression.AcquireDecoder
// used through its stateless DecodeAll. The text must be exactly as
// long as the table says: a frame that gives another length would
// shift every offset after it. An error wraps ErrDamagedFrame and names
// the frame, except a refusal for size.
//
// SECURITY: the whole text is held, so it is held to
// compression.WindowLimit. A frame the table gives more than that is
// refused before anything is decoded (ReadSeekTable has refused such a
// table already), and so is a frame whose header declares a window
// above it, both with an error wrapping compression.ErrWindowTooLarge:
// the file is too large to read, not damaged, and must not be answered
// as if its other frames were all of it. A frame that would give more
// text than its entry is stopped by the decoder at the limit and is
// damaged.
func DecodeFrame(zd *zstd.Decoder, compressed []byte, frame FrameInfo) ([]byte, error) {
	if frameTooLarge(frame) {
		return nil, fmt.Errorf("frame %d: %w: the seek table gives it %d bytes of text in %d bytes",
			frame.Index, compression.ErrWindowTooLarge, frame.DecompressedSize, frame.CompressedSize)
	}
	out, err := zd.DecodeAll(compressed, nil)
	if errors.Is(err, zstd.ErrWindowSizeExceeded) {
		return nil, fmt.Errorf("frame %d: %w: %w", frame.Index, compression.ErrWindowTooLarge, err)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: frame %d: %w", ErrDamagedFrame, frame.Index, err)
	}
	if int64(len(out)) != frame.DecompressedSize {
		return nil, fmt.Errorf("%w: frame %d: decompressed to %d bytes, the seek table says %d",
			ErrDamagedFrame, frame.Index, len(out), frame.DecompressedSize)
	}
	return out, nil
}

// Decoder decompresses frames from seekable zstd files.
//
// Reusable across calls: create once, call DecompressFrameAt many
// times. Safe for concurrent use — each decompressed frame uses a
// decoder pulled from the package-level pool in internal/compression,
// which amortizes the ~2 MB decoding-table allocation across all
// callers in the process (seekable readers, webapi handlers, etc.).
//
// Go note: Decoder is a plain struct with no fields today. It is kept
// as a type (rather than a package-level function) so that future
// per-instance state (e.g. metrics, config) can be added without
// breaking callers. Constructing via NewDecoder() is idiomatic Go and
// gives us a seam to attach such state later.
type Decoder struct{}

// NewDecoder constructs a Decoder. Cheap — no allocation beyond the
// struct itself.
func NewDecoder() *Decoder {
	return &Decoder{}
}

// DecompressFrame reads frame[idx] from path and returns its decompressed
// bytes. idx is 0-based.
//
// Opens path fresh each call so the function is safe to call from
// goroutines that aren't sharing file descriptors. A caller that
// decompresses many frames from one file opens it once and calls
// DecompressFrameAt.
//
// SECURITY: a caller decodes one frame per call and holds what it
// keeps, so the memory a set of frames costs is the caller's choice.
// The package deliberately has no call that decodes a set of frames at
// once: it would hold the whole set, up to 128 MiB per frame.
func (d *Decoder) DecompressFrame(path string, idx int, tbl *SeekTable) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only: close-error is informational
	return d.DecompressFrameAt(f, idx, tbl)
}

// DecompressFrameAt is DecompressFrame for a file the caller has
// already opened, such as one opened through a pin that checked it is
// the file that was validated.
func (d *Decoder) DecompressFrameAt(r io.ReaderAt, idx int, tbl *SeekTable) ([]byte, error) {
	if idx < 0 || idx >= tbl.NumFrames {
		return nil, fmt.Errorf("%w: idx=%d numFrames=%d", ErrFrameIndexOutOfRange, idx, tbl.NumFrames)
	}
	return d.decompressFrameFromReaderAt(r, tbl.Frames[idx])
}

// decompressFrameFromReaderAt reads frame bytes then feeds them to the
// zstd decoder. Uses the package-level pool from internal/compression
// to amortize decoder construction cost (the zstd decoder allocates
// ~2 MB of internal decoding tables on creation — reusing is a ~5x
// speedup on multi-frame cold-index builds).
//
// Note: DecodeAll is stateless across calls, so pooled decoders do NOT
// need a Reset between uses. The returned byte slice is caller-owned.
func (d *Decoder) decompressFrameFromReaderAt(r io.ReaderAt, frame FrameInfo) ([]byte, error) {
	buf := make([]byte, frame.CompressedSize)
	if _, err := r.ReadAt(buf, frame.CompressedOffset); err != nil {
		return nil, fmt.Errorf("read compressed frame bytes: %w", err)
	}
	zd := compression.AcquireDecoder()
	defer compression.ReleaseDecoder(zd)
	return DecodeFrame(zd, buf, frame)
}
