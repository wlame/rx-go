package trace

import (
	"fmt"
	"os"

	"github.com/wlame/rx-go/internal/config"
)

// Chunker generates tasks by splitting files into line-aligned chunks
type Chunker struct {
	cfg *config.Config
}

// NewChunker creates a new chunker
func NewChunker(cfg *config.Config) *Chunker {
	return &Chunker{cfg: cfg}
}

// CreateTasks generates tasks for parallel processing of a file
// Files are split into chunks aligned to newline boundaries
func (c *Chunker) CreateTasks(filePath string) ([]Task, error) {
	// Get file size
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	fileSize := fileInfo.Size()

	// Calculate number of chunks
	numChunks := c.calculateChunkCount(fileSize)

	// Small files get a single chunk
	if numChunks == 1 {
		return []Task{{
			ID:       fmt.Sprintf("%s-chunk-0", filePath),
			FilePath: filePath,
			Offset:   0,
			Length:   fileSize,
			ChunkID:  0,
		}}, nil
	}

	// Calculate chunk size
	chunkSize := fileSize / int64(numChunks)

	// Generate raw offsets
	rawOffsets := make([]int64, numChunks)
	for i := 0; i < numChunks; i++ {
		rawOffsets[i] = int64(i) * chunkSize
	}

	// Align offsets to newlines
	alignedOffsets := make([]int64, numChunks)
	alignedOffsets[0] = 0 // First chunk always starts at 0

	for i := 1; i < numChunks; i++ {
		offset, err := c.findNextNewline(filePath, rawOffsets[i])
		if err != nil {
			return nil, fmt.Errorf("failed to align chunk %d: %w", i, err)
		}
		alignedOffsets[i] = offset
	}

	// Create tasks
	tasks := make([]Task, numChunks)
	for i := 0; i < numChunks; i++ {
		start := alignedOffsets[i]
		var length int64

		if i == numChunks-1 {
			// Last chunk goes to end of file
			length = fileSize - start
		} else {
			length = alignedOffsets[i+1] - start
		}

		tasks[i] = Task{
			ID:       fmt.Sprintf("%s-chunk-%d", filePath, i),
			FilePath: filePath,
			Offset:   start,
			Length:   length,
			ChunkID:  i,
		}
	}

	return tasks, nil
}

// calculateChunkCount determines how many chunks to split a file into
func (c *Chunker) calculateChunkCount(fileSize int64) int {
	minChunkSize := c.cfg.MinChunkSizeBytes
	maxWorkers := int64(c.cfg.MaxWorkers)

	// Calculate potential number of chunks
	potentialChunks := fileSize / minChunkSize

	// Limit to max workers
	if potentialChunks > maxWorkers {
		return int(maxWorkers)
	}

	if potentialChunks < 1 {
		return 1
	}

	return int(potentialChunks)
}

// findNextNewline finds the next newline character after the given offset
// Returns the position AFTER the newline (start of next line)
func (c *Chunker) findNextNewline(filePath string, offset int64) (int64, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Seek to offset
	if _, err := f.Seek(offset, 0); err != nil {
		return 0, err
	}

	// Read up to 256KB looking for newline
	const readSize = 256 * 1024
	buf := make([]byte, readSize)

	n, err := f.Read(buf)
	if err != nil && err.Error() != "EOF" {
		return 0, err
	}

	// Find first newline
	for i := 0; i < n; i++ {
		if buf[i] == '\n' {
			return offset + int64(i) + 1, nil // Position AFTER newline
		}
	}

	// No newline found in read buffer, return end of read
	return offset + int64(n), nil
}
