// compressed.go handles searching within compressed files.
//
// Two strategies are used depending on the compression format:
//
//   - Standard formats (gzip, xz, bz2, non-seekable zstd): single-stream decompression
//     piped to a single rg process. No chunking is possible because these formats don't
//     support random access.
//
//   - Seekable zstd: parallel frame-batch decompression. The seek table is read, frames
//     are grouped into batches by target decompressed size, and each batch is decompressed
//     natively then piped to its own rg process. Results are merged with offset adjustment.
//
// All decompression is done in-process (native Go libraries, no subprocess decompressors).
package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/wlame/rx/internal/compression"
	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/models"
	"github.com/wlame/rx/internal/rgjson"
)

// SearchCompressedFile decompresses a file using the appropriate native reader and
// pipes the decompressed stream to a single rg process. This is used for gzip, xz,
// bz2, and non-seekable zstd files.
//
// Returns matches with offsets relative to the decompressed stream. Line numbers
// are relative (absolute resolution requires an index, handled in Phase 4).
func SearchCompressedFile(
	ctx context.Context,
	path string,
	patterns []string,
	rgExtraArgs []string,
	maxLineSize int,
) ([]models.Match, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	// Open the native decompression reader.
	reader, _, err := compression.NewReader(path)
	if err != nil {
		return nil, fmt.Errorf("open compressed reader for %s: %w", path, err)
	}
	defer reader.Close()

	return searchFromReader(ctx, reader, patterns, rgExtraArgs, maxLineSize)
}

// searchFromReader pipes an io.Reader to rg and parses JSON matches.
// This is the shared implementation for both single-stream compressed search
// and seekable zstd batch search.
func searchFromReader(
	ctx context.Context,
	reader io.Reader,
	patterns []string,
	rgExtraArgs []string,
	maxLineSize int,
) ([]models.Match, error) {
	// Build rg command: rg --json -e pat1 -e pat2 ... [extra args] -
	args := []string{"--json", "--no-heading", "--color=never"}
	for _, p := range patterns {
		args = append(args, "-e", p)
	}

	// Filter incompatible flags.
	for _, a := range rgExtraArgs {
		if a == "--byte-offset" || a == "--only-matching" {
			continue
		}
		args = append(args, a)
	}
	args = append(args, "-") // Read from stdin.

	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Stdin = reader
	cmd.Stderr = io.Discard

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start rg: %w", err)
	}

	parser := rgjson.NewParser(stdout, maxLineSize)
	var matches []models.Match

	for {
		msg, err := parser.Next()
		if err != nil {
			break
		}
		if msg == nil {
			break
		}
		if msg.Type != rgjson.TypeMatch || msg.Match == nil {
			continue
		}

		rm := msg.Match
		lineText := strings.TrimRight(rm.Lines.Text, "\n\r")
		lineNum := rm.LineNumber

		m := models.Match{
			Offset:             rm.AbsoluteOffset,
			AbsoluteLineNumber: -1,
			LineText:           &lineText,
		}
		if lineNum > 0 {
			m.RelativeLineNumber = &lineNum
		}

		subs := make([]models.Submatch, len(rm.Submatches))
		for i, s := range rm.Submatches {
			subs[i] = models.Submatch{
				Text:  s.Match.Text,
				Start: s.Start,
				End:   s.End,
			}
		}
		m.Submatches = &subs

		matches = append(matches, m)
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		if ctx.Err() != nil {
			return matches, ctx.Err()
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return matches, nil
		}
		return matches, fmt.Errorf("rg exited: %w", waitErr)
	}

	return matches, nil
}

// frameBatch groups consecutive seekable zstd frames for parallel processing.
type frameBatch struct {
	startIdx int                   // Index of first frame in the batch.
	frames   []compression.FrameEntry // Frames in this batch.
}

// SearchSeekableZstd performs parallel search on a seekable zstd file.
//
// Algorithm:
//  1. Read the seek table.
//  2. Check line alignment.
//  3. Fast path: if total decompressed size is small, use single-stream.
//  4. Batch frames by target decompressed size (config.FrameBatchSizeMB).
//  5. For each batch (bounded by config.MaxSubprocesses goroutines):
//     a. Decompress all frames in the batch natively.
//     b. Concatenate decompressed data.
//     c. Pipe to rg, parse matches.
//     d. Adjust offsets using frame decompressed_offset.
//  6. Merge all batch results, sort by offset.
func SearchSeekableZstd(
	ctx context.Context,
	path string,
	patterns []string,
	rgExtraArgs []string,
	cfg *config.Config,
) ([]models.Match, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Step 1: Read seek table.
	table, err := compression.ReadSeekTable(f)
	if err != nil {
		return nil, fmt.Errorf("read seek table for %s: %w", path, err)
	}

	totalDecompressed := table.TotalDecompressedSize()

	// Step 3: Fast path — small files go single-stream.
	fastPathThreshold := int64(cfg.MinChunkSizeMB) * 1024 * 1024
	if totalDecompressed < fastPathThreshold {
		slog.Debug("seekable zstd fast path: single-stream",
			"path", path,
			"decompressed_size", totalDecompressed,
			"threshold", fastPathThreshold)
		return SearchCompressedFile(ctx, path, patterns, rgExtraArgs, cfg.MaxLineSizeKB)
	}

	// Step 2: Check line alignment.
	lineAligned, err := compression.IsLineAligned(f, table)
	if err != nil {
		slog.Warn("could not check line alignment, assuming not aligned",
			"path", path, "error", err)
	}

	if !lineAligned {
		// Non-aligned frames can't be batched safely — fall back to single-stream.
		slog.Warn("seekable zstd frames not line-aligned, using single-stream",
			"path", path)
		return SearchCompressedFile(ctx, path, patterns, rgExtraArgs, cfg.MaxLineSizeKB)
	}

	// Step 4: Batch frames by target decompressed size.
	batchTargetBytes := int64(cfg.FrameBatchSizeMB) * 1024 * 1024
	batches := planFrameBatches(table, batchTargetBytes)

	slog.Debug("seekable zstd parallel search",
		"path", path,
		"frames", len(table.Frames),
		"batches", len(batches),
		"batch_target_mb", cfg.FrameBatchSizeMB)

	// Step 5: Process batches in parallel.
	var (
		mu         sync.Mutex
		allResults [][]models.Match
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(cfg.MaxSubprocesses)

	for _, batch := range batches {
		batch := batch // capture for closure
		g.Go(func() error {
			matches, err := searchFrameBatch(gctx, f, batch, patterns, rgExtraArgs, cfg.MaxLineSizeKB)
			if err != nil {
				if gctx.Err() != nil {
					return nil
				}
				slog.Warn("frame batch search failed",
					"path", path,
					"start_frame", batch.startIdx,
					"frame_count", len(batch.frames),
					"error", err)
				return nil
			}

			mu.Lock()
			allResults = append(allResults, matches)
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Step 6: Merge results.
	merged := MergeResults(allResults)
	return merged, nil
}

// planFrameBatches groups consecutive frames into batches where each batch's total
// decompressed size approaches targetBytes. This balances subprocess overhead (too many
// small batches) against memory usage (too few large batches).
func planFrameBatches(table *compression.SeekTable, targetBytes int64) []frameBatch {
	if len(table.Frames) == 0 {
		return nil
	}

	var batches []frameBatch
	var current frameBatch
	var currentSize int64

	current.startIdx = 0

	for _, frame := range table.Frames {
		current.frames = append(current.frames, frame)
		currentSize += int64(frame.DecompressedSize)

		if currentSize >= targetBytes {
			batches = append(batches, current)
			current = frameBatch{startIdx: len(batches) * len(current.frames)} // approximate
			current.startIdx = int(frame.CompressedOffset+int64(frame.CompressedSize)) / 1 // reset
			current = frameBatch{}
			currentSize = 0
		}
	}

	// Don't forget the last batch.
	if len(current.frames) > 0 {
		batches = append(batches, current)
	}

	// Fix startIdx — it should reflect the index of the first frame in each batch.
	frameIdx := 0
	for i := range batches {
		batches[i].startIdx = frameIdx
		frameIdx += len(batches[i].frames)
	}

	return batches
}

// searchFrameBatch decompresses all frames in a batch natively, concatenates the
// decompressed output, pipes it to rg, and adjusts match offsets to be absolute
// within the decompressed file.
func searchFrameBatch(
	ctx context.Context,
	f *os.File,
	batch frameBatch,
	patterns []string,
	rgExtraArgs []string,
	maxLineSize int,
) ([]models.Match, error) {
	// Decompress all frames in the batch and concatenate.
	var decompressed bytes.Buffer
	// Track where each frame's data starts in our concatenated buffer,
	// so we can map rg offsets back to absolute decompressed offsets.
	type frameRange struct {
		bufStart           int   // Start offset in concatenated buffer.
		decompressedOffset int64 // Absolute decompressed offset in the original file.
		decompressedSize   int   // Size of this frame's decompressed data.
	}
	ranges := make([]frameRange, len(batch.frames))

	for i, frame := range batch.frames {
		data, err := compression.DecompressFrame(f, frame)
		if err != nil {
			return nil, fmt.Errorf("decompress frame at offset %d: %w", frame.CompressedOffset, err)
		}

		ranges[i] = frameRange{
			bufStart:           decompressed.Len(),
			decompressedOffset: frame.DecompressedOffset,
			decompressedSize:   len(data),
		}

		decompressed.Write(data)
	}

	// Pipe concatenated decompressed data to rg.
	reader := bytes.NewReader(decompressed.Bytes())
	matches, err := searchFromReader(ctx, reader, patterns, rgExtraArgs, maxLineSize)
	if err != nil {
		return nil, err
	}

	// Adjust offsets: rg reports offsets relative to the concatenated buffer.
	// We need to map them to absolute decompressed file offsets.
	for i := range matches {
		batchOffset := matches[i].Offset

		// Find which frame this offset falls into.
		for _, fr := range ranges {
			if batchOffset >= fr.bufStart && batchOffset < fr.bufStart+fr.decompressedSize {
				offsetInFrame := batchOffset - fr.bufStart
				matches[i].Offset = int(fr.decompressedOffset) + offsetInFrame
				break
			}
		}
	}

	return matches, nil
}
