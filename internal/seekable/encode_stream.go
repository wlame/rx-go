package seekable

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// streamReadBufferSize is the read-ahead buffer between the source and
// the frame splitter. A decompressor source hands out small reads; the
// buffer turns them into a few large ones.
const streamReadBufferSize = 64 * 1024

// EncodeStream reads src to its end and writes a seekable zstd file to
// dst. It is the encoder for a source whose length is not known up
// front, such as the text a gzip or xz decompressor produces, and it is
// the only encoding path: Encode wraps a file in a reader and calls it.
//
// Frames are newline-aligned exactly as Encode documents: each frame
// takes FrameSize bytes and then runs on to the end of the line it
// stopped in, so the output does not depend on how src splits its reads.
//
// Memory is bounded by one batch of frames, not by the input:
//
//	read Workers frames ─▶ compress them in parallel ─▶ write them in order ─▶ repeat
//
// Each batch's frames are compressed by one goroutine per frame, each
// with its own zstd encoder (an encoder is not safe for concurrent use).
// The batch is written only after every goroutine of the batch has
// finished, which keeps the frames in input order without a reorder
// buffer. With Workers=1 no goroutine is started.
//
// An error from src, from dst or from ctx stops the encoding and is
// returned wrapped; dst then holds a partial file the caller must
// discard.
func (e *Encoder) EncodeStream(ctx context.Context, src io.Reader, dst io.Writer) (*SeekTable, error) {
	encoders, err := newWorkerEncoders(e.cfg.Workers, e.cfg.Level)
	// closeWorkerEncoders skips the nil slots a failed construction left,
	// so the deferred call is right on both the error and the normal path.
	defer closeWorkerEncoders(encoders)
	if err != nil {
		return nil, err
	}

	reader := bufio.NewReaderSize(src, streamReadBufferSize)
	var frames []FrameInfo
	var compressedOffset, decompressedOffset int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := readFrameBatch(reader, len(encoders), e.cfg.FrameSize)
		if err != nil {
			return nil, fmt.Errorf("read input at byte %d: %w", decompressedOffset, err)
		}
		if len(batch) == 0 {
			break
		}
		compressed := compressFrameBatch(encoders, batch)
		for i, text := range batch {
			if _, err := dst.Write(compressed[i]); err != nil {
				return nil, fmt.Errorf("write compressed frame %d: %w", len(frames), err)
			}
			frames = append(frames, FrameInfo{
				Index:              len(frames),
				CompressedOffset:   compressedOffset,
				CompressedSize:     int64(len(compressed[i])),
				DecompressedOffset: decompressedOffset,
				DecompressedSize:   int64(len(text)),
			})
			compressedOffset += int64(len(compressed[i]))
			decompressedOffset += int64(len(text))
		}
	}

	if err := WriteSeekTable(dst, frames); err != nil {
		return nil, fmt.Errorf("write seek table: %w", err)
	}
	return &SeekTable{NumFrames: len(frames), Frames: frames}, nil
}

// newWorkerEncoders builds one zstd encoder per worker through the
// newZstdEncoderForWorker seam. On a construction error it returns the
// slice built so far (later slots nil) together with the error, so the
// caller's deferred closeWorkerEncoders releases every encoder that was
// made: each holds a compression window and an internal goroutine until
// Close.
func newWorkerEncoders(workers, level int) ([]workerEncoder, error) {
	encoders := make([]workerEncoder, workers)
	for i := range encoders {
		enc, err := newZstdEncoderForWorker(level)
		if err != nil {
			return encoders, fmt.Errorf("create zstd encoder %d: %w", i, err)
		}
		encoders[i] = enc
	}
	return encoders, nil
}

// closeWorkerEncoders closes every non-nil encoder once.
func closeWorkerEncoders(encoders []workerEncoder) {
	for _, enc := range encoders {
		if enc != nil {
			// Close of an encoder used only through EncodeAll has
			// nothing left to flush; its error carries no information.
			_ = enc.Close()
		}
	}
}

// readFrameBatch reads up to count newline-aligned frames from reader.
// It returns fewer only at the end of the input, and none once the input
// is exhausted.
func readFrameBatch(reader *bufio.Reader, count, frameSize int) ([][]byte, error) {
	batch := make([][]byte, 0, count)
	for len(batch) < count {
		frame, err := readFrame(reader, frameSize)
		if err != nil {
			return nil, err
		}
		if len(frame) == 0 {
			break
		}
		batch = append(batch, frame)
	}
	return batch, nil
}

// readFrame reads frameSize bytes and then the rest of the line they end
// in, newline included. At the end of the input it returns what is left,
// which is empty once everything has been read; any other read error is
// returned as is.
//
// Only io.EOF ends the input. io.ReadFull is not used because it turns a
// short read into io.ErrUnexpectedEOF, the same value a gzip or zstd
// decoder returns for a truncated stream; treating that as the end would
// encode a corrupt input as if its text simply stopped there.
func readFrame(reader *bufio.Reader, frameSize int) ([]byte, error) {
	frame := make([]byte, frameSize)
	n := 0
	for n < frameSize {
		read, err := reader.Read(frame[n:])
		n += read
		if errors.Is(err, io.EOF) {
			return frame[:n], nil
		}
		if err != nil {
			return nil, err
		}
	}
	if frame[n-1] == '\n' {
		return frame, nil
	}
	rest, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return append(frame, rest...), nil
}

// compressFrameBatch compresses batch[i] with encoders[i] and returns
// the results in batch order. len(batch) never exceeds len(encoders).
func compressFrameBatch(encoders []workerEncoder, batch [][]byte) [][]byte {
	compressed := make([][]byte, len(batch))
	if len(batch) == 1 {
		compressed[0] = encoders[0].EncodeAll(batch[0], nil)
		return compressed
	}
	// One goroutine per frame. Each writes only its own slot of
	// compressed, so no lock is needed; wg.Wait is the point after which
	// the caller may read every slot.
	var wg sync.WaitGroup
	for i := range batch {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			compressed[i] = encoders[i].EncodeAll(batch[i], nil)
		}(i)
	}
	wg.Wait()
	return compressed
}
