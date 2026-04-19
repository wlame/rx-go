package seekable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
)

// DefaultFrameSize is the target decompressed-bytes size for each frame.
// Matches Python's DEFAULT_FRAME_SIZE_BYTES = 4 MiB.
//
// Larger frames compress better (bigger window) but reduce parallelism
// granularity; 4 MiB is Python's empirical sweet spot so we match it.
const DefaultFrameSize = 4 * 1024 * 1024

// DefaultCompressionLevel = 3 matches Python. zstd's "balanced" level.
const DefaultCompressionLevel = 3

// EncoderConfig tunes the encoder. Zero values mean "use the default".
type EncoderConfig struct {
	// FrameSize is the target decompressed bytes per frame. Frames may
	// exceed this by up to one line to respect newline alignment.
	FrameSize int

	// Level is the zstd compression level (1-22). 3 is balanced.
	Level int

	// Workers is the parallelism for frame compression. 0 = single-threaded.
	// More workers use more memory (one encoder + one buffer per worker)
	// but scale linearly with CPU cores. Use runtime.NumCPU() for max.
	Workers int
}

// applyDefaults fills zero fields with the defaults.
func (c *EncoderConfig) applyDefaults() {
	if c.FrameSize <= 0 {
		c.FrameSize = DefaultFrameSize
	}
	if c.Level <= 0 {
		c.Level = DefaultCompressionLevel
	}
	if c.Workers <= 0 {
		c.Workers = 1
	}
}

// Encoder creates seekable zstd files.
//
// Usage:
//
//	enc := seekable.NewEncoder(seekable.EncoderConfig{Workers: runtime.NumCPU()})
//	tbl, err := enc.Encode(ctx, src, srcSize, dst)
//
// Encode returns the seek table describing the frames it wrote.
type Encoder struct {
	cfg EncoderConfig
}

// NewEncoder constructs an encoder. The config is copied.
func NewEncoder(cfg EncoderConfig) *Encoder {
	cfg.applyDefaults()
	return &Encoder{cfg: cfg}
}

// chunk describes one slice of input destined to become a zstd frame.
type chunk struct {
	index        int
	decompressed []byte // owned; may be recycled via sync.Pool in future
	// compressed bytes are stored in a separate slice indexed by index
}

// Encode reads srcSize bytes from src and writes a seekable zstd file
// to dst. Newline-aligned frames: the last byte of every frame is '\n'
// (except possibly the final frame, which contains whatever trailing
// bytes have no terminator).
//
// Concurrency model (decision 5.4, Workers > 1):
//
//	reader goroutine        workers              writer goroutine
//	  |                       |                    |
//	  v                       v                    v
//	split into newline-   compress each       write in chunk-index
//	aligned chunks,       chunk independently  order to preserve the
//	emit to work channel  (Workers copies)    frame ordering
//
// With Workers=1 the reader and writer run in-line (no channels) to
// avoid goroutine overhead.
func (e *Encoder) Encode(ctx context.Context, src io.ReaderAt, srcSize int64, dst io.Writer) (*SeekTable, error) {
	if e.cfg.Workers <= 1 {
		return e.encodeSequential(ctx, src, srcSize, dst)
	}
	return e.encodeParallel(ctx, src, srcSize, dst)
}

// encodeSequential is the simple path: read a chunk, compress it, write it.
// Produces identical output to the parallel path but with less machinery.
func (e *Encoder) encodeSequential(ctx context.Context, src io.ReaderAt, srcSize int64, dst io.Writer) (*SeekTable, error) {
	zenc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstdEncoderLevel(e.cfg.Level)),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	defer func() {
		// Close on a nil-backing encoder is a no-op; error is
		// informational only and doesn't affect correctness here.
		_ = zenc.Close()
	}()

	var frames []FrameInfo
	var compressedOffset, decompressedOffset int64
	frameIndex := 0

	// We read forward through src, emitting one chunk per loop iteration.
	// readPos tracks the next byte to read.
	var readPos int64
	for readPos < srcSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunkBytes, err := readChunk(src, readPos, srcSize, int64(e.cfg.FrameSize))
		if err != nil {
			return nil, fmt.Errorf("read chunk at %d: %w", readPos, err)
		}
		if len(chunkBytes) == 0 {
			break
		}

		compressed := zenc.EncodeAll(chunkBytes, nil)

		if _, err := dst.Write(compressed); err != nil {
			return nil, fmt.Errorf("write compressed frame %d: %w", frameIndex, err)
		}

		frames = append(frames, FrameInfo{
			Index:              frameIndex,
			CompressedOffset:   compressedOffset,
			CompressedSize:     int64(len(compressed)),
			DecompressedOffset: decompressedOffset,
			DecompressedSize:   int64(len(chunkBytes)),
		})
		compressedOffset += int64(len(compressed))
		decompressedOffset += int64(len(chunkBytes))
		frameIndex++
		readPos += int64(len(chunkBytes))
	}

	if err := WriteSeekTable(dst, frames); err != nil {
		return nil, fmt.Errorf("write seek table: %w", err)
	}
	return &SeekTable{
		NumFrames: len(frames),
		Flags:     0,
		Frames:    frames,
	}, nil
}

// workerEncoder is the minimal interface encodeParallel needs from a
// worker encoder. *zstd.Encoder satisfies this naturally; tests inject
// spies to verify cleanup semantics. Keeping the interface internal
// means production code is unaffected (size / allocation / dispatch
// overhead is negligible for a handful of encoders allocated once per
// Encode() call).
type workerEncoder interface {
	EncodeAll(src, dst []byte) []byte
	Close() error
}

// newZstdEncoderForWorker constructs one zstd encoder for a worker slot.
// It is a package-level variable so tests can swap in a factory that
// returns spies or induces failures, exercising the init-error cleanup
// path (see TestEncodeParallel_EncoderClosedOnInitError). Production
// code must not reassign this variable; it exists solely as a test seam.
var newZstdEncoderForWorker = func(level int) (workerEncoder, error) {
	return zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstdEncoderLevel(level)),
		zstd.WithEncoderConcurrency(1),
	)
}

// encodeParallel uses errgroup to distribute frame compression across
// Workers goroutines while preserving output order.
//
// Why NOT use zstd's built-in concurrency:
//
//	The klauspost/compress zstd encoder CAN parallelise internally
//	via WithEncoderConcurrency, BUT that parallelism is WITHIN a
//	single frame (block-level). We need parallelism ACROSS frames
//	because each frame is an independent compression unit that the
//	decoder can pick individually. So we spin our own goroutines.
func (e *Encoder) encodeParallel(ctx context.Context, src io.ReaderAt, srcSize int64, dst io.Writer) (*SeekTable, error) {
	// Each worker gets its own zstd encoder — zstd.Encoder is NOT safe
	// for concurrent EncodeAll calls on the same instance.
	workers := e.cfg.Workers
	encoders := make([]workerEncoder, workers)

	// CRITICAL: register the cleanup defer BEFORE the construction loop.
	// Previously the defer was installed AFTER the loop; a failure at
	// iteration i leaked encoders[0..i-1] because the early return
	// bypassed the defer entirely. Each leaked *zstd.Encoder retains a
	// compression window and an internal worker goroutine that only
	// release on Close(), so cumulative leaks can grow quickly on a
	// long-running server that encounters init errors.
	//
	// By installing the defer first, the slice's nil entries (positions
	// that were never reached in the loop) are skipped via the explicit
	// nil check; positions that were populated get Close()d exactly
	// once on normal exit and exactly once on early exit.
	defer func() {
		for _, enc := range encoders {
			if enc == nil {
				// Construction bailed before reaching this slot — nothing
				// to clean up here.
				continue
			}
			_ = enc.Close()
		}
	}()

	for i := range encoders {
		enc, err := newZstdEncoderForWorker(e.cfg.Level)
		if err != nil {
			// encoders[0..i-1] are already populated and the defer above
			// will Close() them. encoders[i..] stay nil and are skipped.
			return nil, fmt.Errorf("create zstd encoder %d: %w", i, err)
		}
		encoders[i] = enc
	}

	// Phase 1: read the whole input into chunks (newline-aligned).
	// We buffer the entire chunk list so the workers can pick any index.
	// For very large files this holds srcSize bytes in memory — matching
	// Python's behavior — but keeps the encoder cache-friendly.
	//
	// A future optimization would be to stream chunks through a bounded
	// channel and rely on write-order reassembly, but that's an incremental
	// improvement (decision 5.4 doesn't require it at v1).
	chunks, err := splitIntoChunks(src, srcSize, int64(e.cfg.FrameSize))
	if err != nil {
		return nil, fmt.Errorf("split chunks: %w", err)
	}
	if len(chunks) == 0 {
		// Empty input: still produce a valid (empty) seek table so
		// readers can round-trip the file.
		if err := WriteSeekTable(dst, nil); err != nil {
			return nil, err
		}
		return &SeekTable{}, nil
	}

	// Phase 2: compress chunks in parallel. compressed[i] holds the
	// bytes for chunk[i] after its worker finishes.
	compressed := make([][]byte, len(chunks))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	// Round-robin encoders across workers. We use a mutex-protected
	// free list so workers grab whichever encoder is idle.
	var encMu sync.Mutex
	free := make([]workerEncoder, len(encoders))
	copy(free, encoders)

	for i, c := range chunks {
		i := i
		c := c
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			encMu.Lock()
			enc := free[len(free)-1]
			free = free[:len(free)-1]
			encMu.Unlock()

			out := enc.EncodeAll(c.decompressed, nil)

			encMu.Lock()
			free = append(free, enc)
			encMu.Unlock()

			compressed[i] = out
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Phase 3: write in order + compute seek table.
	frames := make([]FrameInfo, len(chunks))
	var compressedOffset, decompressedOffset int64
	for i, c := range chunks {
		out := compressed[i]
		if _, err := dst.Write(out); err != nil {
			return nil, fmt.Errorf("write frame %d: %w", i, err)
		}
		frames[i] = FrameInfo{
			Index:              i,
			CompressedOffset:   compressedOffset,
			CompressedSize:     int64(len(out)),
			DecompressedOffset: decompressedOffset,
			DecompressedSize:   int64(len(c.decompressed)),
		}
		compressedOffset += int64(len(out))
		decompressedOffset += int64(len(c.decompressed))
	}

	// Phase 4: seek table tail.
	if err := WriteSeekTable(dst, frames); err != nil {
		return nil, fmt.Errorf("write seek table: %w", err)
	}
	return &SeekTable{
		NumFrames: len(frames),
		Flags:     0,
		Frames:    frames,
	}, nil
}

// readChunk reads target bytes starting at offset, then extends to the
// next newline if the read didn't end on one. Returns the exact bytes
// of the chunk; the caller is responsible for advancing its read
// pointer by len(chunk).
func readChunk(src io.ReaderAt, offset, srcSize, target int64) ([]byte, error) {
	toRead := target
	if offset+toRead > srcSize {
		toRead = srcSize - offset
	}
	if toRead <= 0 {
		return nil, nil
	}
	buf := make([]byte, toRead)
	n, err := src.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:n]

	// Align to newline: if we didn't end on \n, keep reading in small
	// increments until we find one or hit EOF.
	if len(buf) == 0 || buf[len(buf)-1] == '\n' {
		return buf, nil
	}
	extraSize := 4096
	extraOffset := offset + int64(n)
	for extraOffset < srcSize {
		step := extraSize
		if extraOffset+int64(step) > srcSize {
			step = int(srcSize - extraOffset)
		}
		extra := make([]byte, step)
		en, err := src.ReadAt(extra, extraOffset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		extra = extra[:en]
		if idx := bytes.IndexByte(extra, '\n'); idx >= 0 {
			buf = append(buf, extra[:idx+1]...)
			return buf, nil
		}
		buf = append(buf, extra...)
		extraOffset += int64(en)
		if en == 0 {
			break
		}
	}
	return buf, nil
}

// splitIntoChunks reads the whole input from src into newline-aligned
// chunks. Used by the parallel encoder to hand off chunks to workers.
func splitIntoChunks(src io.ReaderAt, srcSize, target int64) ([]chunk, error) {
	var chunks []chunk
	var pos int64
	idx := 0
	for pos < srcSize {
		data, err := readChunk(src, pos, srcSize, target)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			break
		}
		chunks = append(chunks, chunk{
			index:        idx,
			decompressed: data,
		})
		pos += int64(len(data))
		idx++
	}
	return chunks, nil
}

// zstdEncoderLevel maps an integer 1..22 to klauspost/compress's named
// encoder levels. klauspost defines 5 discrete levels (Fastest, Default,
// BetterCompression, BestCompression, SpeedDefault) whereas upstream
// zstd accepts 1..22. We pick the closest match so the file sizes are
// comparable to Python's output without requiring byte-identity.
func zstdEncoderLevel(n int) zstd.EncoderLevel {
	switch {
	case n <= 1:
		return zstd.SpeedFastest
	case n <= 5:
		return zstd.SpeedDefault // klauspost's ~level 3
	case n <= 9:
		return zstd.SpeedBetterCompression // ~level 7
	default:
		return zstd.SpeedBestCompression // ~level 11+
	}
}
