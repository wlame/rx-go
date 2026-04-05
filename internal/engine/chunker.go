// chunker.go plans newline-aligned byte chunks for parallel file search.
//
// The goal is to divide a file into roughly equal sections that can each be fed to a separate
// rg process via io.SectionReader. Every boundary is pushed forward to the next newline so that
// no line is ever split across chunks.
package engine

import (
	"fmt"
	"os"

	"github.com/wlame/rx/internal/config"
)

// maxNewlineSeek is the maximum number of bytes we scan forward when aligning a
// chunk boundary to a newline. 256 KB matches the Python reference.
const maxNewlineSeek = 256 * 1024

// Chunk describes a contiguous byte range of a file assigned to one worker.
type Chunk struct {
	Index  int   // 0-based chunk index.
	Offset int64 // Byte offset where this chunk starts in the file.
	Length int64 // Number of bytes this chunk covers.
}

// PlanChunks divides a file into newline-aligned chunks for parallel search.
//
// Algorithm (matches Python's get_file_offsets / create_file_tasks):
//  1. numChunks = max(1, fileSize / (minChunkSizeMB * 1 MiB)), capped at cfg.MaxSubprocesses
//  2. Compute raw boundaries at equal intervals.
//  3. For each boundary (except the first, which is always 0), read forward up to 256 KB
//     looking for a '\n'. The boundary moves to the byte AFTER the newline.
//  4. Build Chunk structs from the aligned boundaries.
//
// When numChunks == 1 the result is a single chunk covering the whole file (fast path).
// An empty file produces a single zero-length chunk.
func PlanChunks(filePath string, fileSize int64, cfg *config.Config) ([]Chunk, error) {
	if fileSize < 0 {
		return nil, fmt.Errorf("negative file size: %d", fileSize)
	}

	// An empty file still gets one (zero-length) chunk so the caller doesn't need a special case.
	if fileSize == 0 {
		return []Chunk{{Index: 0, Offset: 0, Length: 0}}, nil
	}

	minChunkBytes := int64(cfg.MinChunkSizeMB) * 1024 * 1024

	// Number of chunks the file can support given the minimum chunk size.
	numChunks := fileSize / minChunkBytes
	if numChunks < 1 {
		numChunks = 1
	}

	// Cap at the maximum subprocess count.
	maxSubs := int64(cfg.MaxSubprocesses)
	if numChunks > maxSubs {
		numChunks = maxSubs
	}

	// Single chunk fast path — no file I/O needed for alignment.
	if numChunks == 1 {
		return []Chunk{{Index: 0, Offset: 0, Length: fileSize}}, nil
	}

	// Open the file for newline alignment reads.
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open for chunk planning: %w", err)
	}
	defer f.Close()

	chunkSize := fileSize / numChunks

	// Build aligned boundary offsets. The first boundary is always 0.
	boundaries := make([]int64, numChunks)
	boundaries[0] = 0

	for i := int64(1); i < numChunks; i++ {
		rawOffset := i * chunkSize
		aligned, err := findNextNewline(f, rawOffset, fileSize)
		if err != nil {
			return nil, fmt.Errorf("align chunk %d: %w", i, err)
		}
		boundaries[i] = aligned
	}

	// Convert boundaries to Chunk structs.
	chunks := make([]Chunk, numChunks)
	for i := int64(0); i < numChunks; i++ {
		start := boundaries[i]
		var length int64
		if i == numChunks-1 {
			length = fileSize - start
		} else {
			length = boundaries[i+1] - start
		}
		chunks[i] = Chunk{
			Index:  int(i),
			Offset: start,
			Length: length,
		}
	}

	return chunks, nil
}

// findNextNewline scans forward from offset up to maxNewlineSeek bytes looking for '\n'.
// Returns the position immediately AFTER the newline (start of next line). If no newline
// is found within the search window, returns the end of the search window (or fileSize,
// whichever is smaller).
func findNextNewline(f *os.File, offset int64, fileSize int64) (int64, error) {
	if offset >= fileSize {
		return fileSize, nil
	}

	readLen := int64(maxNewlineSeek)
	if offset+readLen > fileSize {
		readLen = fileSize - offset
	}

	buf := make([]byte, readLen)
	n, err := f.ReadAt(buf, offset)
	if err != nil && n == 0 {
		return offset, fmt.Errorf("read at offset %d: %w", offset, err)
	}
	buf = buf[:n]

	for i, b := range buf {
		if b == '\n' {
			// Return the position after the newline.
			return offset + int64(i) + 1, nil
		}
	}

	// No newline found — use the end of the search window.
	return offset + int64(n), nil
}
