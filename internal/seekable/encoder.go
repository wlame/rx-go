package seekable

import (
	"context"
	"io"

	"github.com/klauspost/compress/zstd"
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

// Encode reads srcSize bytes from src and writes a seekable zstd file
// to dst. Newline-aligned frames: the last byte of every frame is '\n'
// (except possibly the final frame, which contains whatever trailing
// bytes have no terminator).
//
// srcSize is read once, so bytes appended to a growing file after the
// call starts are not encoded. The work is done by EncodeStream, which
// holds one batch of Workers frames in memory at a time.
func (e *Encoder) Encode(ctx context.Context, src io.ReaderAt, srcSize int64, dst io.Writer) (*SeekTable, error) {
	return e.EncodeStream(ctx, io.NewSectionReader(src, 0, srcSize), dst)
}

// workerEncoder is the minimal interface the encoder needs from a
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
//
// Why NOT use zstd's built-in concurrency: the klauspost/compress zstd
// encoder CAN parallelize internally via WithEncoderConcurrency, BUT
// that parallelism is WITHIN a single frame (block-level). Seekable
// frames are independent compression units that a decoder picks one
// at a time, so the parallelism has to be ACROSS frames: one encoder
// per worker, each running EncodeAll on its own frame.
var newZstdEncoderForWorker = func(level int) (workerEncoder, error) {
	return zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstdEncoderLevel(level)),
		zstd.WithEncoderConcurrency(1),
	)
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
