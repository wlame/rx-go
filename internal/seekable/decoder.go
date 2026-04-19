package seekable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/wlame/rx-go/internal/compression"
)

// ErrFrameIndexOutOfRange is returned when caller asks for a frame
// that doesn't exist in the seek table.
var ErrFrameIndexOutOfRange = errors.New("frame index out of range")

// Decoder decompresses frames from seekable zstd files.
//
// Reusable across calls: create once, call DecompressFrame / DecompressFrames
// many times. Safe for concurrent use — each decompressed frame uses a
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
// goroutines that aren't sharing file descriptors. For hot paths that
// decompress many frames from one file, use DecompressFrames instead.
func (d *Decoder) DecompressFrame(path string, idx int, tbl *SeekTable) ([]byte, error) {
	if idx < 0 || idx >= tbl.NumFrames {
		return nil, fmt.Errorf("%w: idx=%d numFrames=%d", ErrFrameIndexOutOfRange, idx, tbl.NumFrames)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only: close-error is informational
	return d.decompressFrameFromReaderAt(f, tbl.Frames[idx])
}

// DecompressFrames decodes a set of frames in parallel and returns a
// map from frame index to decompressed bytes. Parallelism is capped at
// runtime.NumCPU() by default (via errgroup default; caller can cap via
// context with a cancel).
//
// Reads are lock-free: os.File's ReadAt is safe for concurrent use on
// Linux/macOS (it uses pread(2) under the hood).
func (d *Decoder) DecompressFrames(ctx context.Context, path string, frameIndices []int, tbl *SeekTable) (map[int][]byte, error) {
	if len(frameIndices) == 0 {
		return map[int][]byte{}, nil
	}
	// Validate indices up front — cheaper than failing mid-goroutine.
	for _, idx := range frameIndices {
		if idx < 0 || idx >= tbl.NumFrames {
			return nil, fmt.Errorf("%w: idx=%d numFrames=%d", ErrFrameIndexOutOfRange, idx, tbl.NumFrames)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only: close-error is informational

	result := make(map[int][]byte, len(frameIndices))
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	for _, idx := range frameIndices {
		idx := idx
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			out, err := d.decompressFrameFromReaderAt(f, tbl.Frames[idx])
			if err != nil {
				return fmt.Errorf("frame %d: %w", idx, err)
			}
			mu.Lock()
			result[idx] = out
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return result, nil
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

	out, err := zd.DecodeAll(buf, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decode: %w", err)
	}
	return out, nil
}

// DecompressRange returns decompressed bytes [startOffset, startOffset+length)
// from the underlying file, decompressing only the frames that overlap
// the range. Convenience for implementing random access on the logical
// (decompressed) stream.
func (d *Decoder) DecompressRange(ctx context.Context, path string, tbl *SeekTable, startOffset, length int64) ([]byte, error) {
	if length <= 0 {
		return []byte{}, nil
	}
	endOffset := startOffset + length
	var need []int
	for _, f := range tbl.Frames {
		if f.DecompressedOffset < endOffset && f.DecompressedEnd() > startOffset {
			need = append(need, f.Index)
		}
	}
	if len(need) == 0 {
		return []byte{}, nil
	}
	frames, err := d.DecompressFrames(ctx, path, need, tbl)
	if err != nil {
		return nil, err
	}
	// Assemble in order.
	result := make([]byte, 0, length)
	for _, idx := range need {
		frame := tbl.Frames[idx]
		fdata := frames[idx]
		fStart := int64(0)
		if startOffset > frame.DecompressedOffset {
			fStart = startOffset - frame.DecompressedOffset
		}
		fEnd := int64(len(fdata))
		if endOffset < frame.DecompressedEnd() {
			fEnd = endOffset - frame.DecompressedOffset
		}
		if fStart < fEnd && fStart < int64(len(fdata)) {
			if fEnd > int64(len(fdata)) {
				fEnd = int64(len(fdata))
			}
			result = append(result, fdata[fStart:fEnd]...)
		}
	}
	if int64(len(result)) > length {
		result = result[:length]
	}
	return result, nil
}
