package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/config"
)

func defaultCfg() *config.Config {
	cfg := config.Load()
	return &cfg
}

// writeTempFile creates a temporary file with the given content and returns its path.
func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err)
	return path
}

func TestPlanChunks_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "empty.txt", "")

	chunks, err := PlanChunks(path, 0, defaultCfg())
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	assert.Equal(t, 0, chunks[0].Index)
	assert.Equal(t, int64(0), chunks[0].Offset)
	assert.Equal(t, int64(0), chunks[0].Length)
}

func TestPlanChunks_SmallFile_SingleChunk(t *testing.T) {
	// A file smaller than MinChunkSizeMB should produce a single chunk.
	dir := t.TempDir()
	content := strings.Repeat("hello world\n", 1000) // ~12 KB
	path := writeTempFile(t, dir, "small.txt", content)
	size := int64(len(content))

	chunks, err := PlanChunks(path, size, defaultCfg())
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	assert.Equal(t, int64(0), chunks[0].Offset)
	assert.Equal(t, size, chunks[0].Length)
}

func TestPlanChunks_SingleChunk_NoFileIO(t *testing.T) {
	// When numChunks == 1, PlanChunks should not open the file.
	// We can verify this by passing a non-existent path with a small size.
	chunks, err := PlanChunks("/nonexistent/path", 100, defaultCfg())
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	assert.Equal(t, int64(0), chunks[0].Offset)
	assert.Equal(t, int64(100), chunks[0].Length)
}

func TestPlanChunks_MultipleChunks_NewlineAlignment(t *testing.T) {
	dir := t.TempDir()

	// Build a 5 MB file with known newline positions.
	// Each line is exactly 100 bytes (99 chars + '\n').
	bigLine := strings.Repeat("a", 99) + "\n"
	bigContent := strings.Repeat(bigLine, 50000) // 5,000,000 bytes
	bigPath := writeTempFile(t, dir, "big.txt", bigContent)
	bigSize := int64(len(bigContent))

	testCfg := &config.Config{
		MinChunkSizeMB:  1, // 1 MB min -> 5 MB file -> 5 chunks
		MaxSubprocesses: 20,
	}
	chunks, err := PlanChunks(bigPath, bigSize, testCfg)
	require.NoError(t, err)

	// Should have multiple chunks (5 MB / 1 MB = 5 chunks).
	assert.GreaterOrEqual(t, len(chunks), 2)

	// Verify all chunks are newline-aligned.
	fileData, err := os.ReadFile(bigPath)
	require.NoError(t, err)

	for i, chunk := range chunks {
		if i == 0 {
			assert.Equal(t, int64(0), chunk.Offset, "first chunk must start at 0")
			continue
		}

		// All other chunk boundaries should be at the start of a line
		// (the byte after a newline).
		if chunk.Offset > 0 && chunk.Offset <= int64(len(fileData)) {
			assert.Equal(t, byte('\n'), fileData[chunk.Offset-1],
				"chunk %d boundary at offset %d should follow a newline", i, chunk.Offset)
		}
	}

	// Verify chunks cover the entire file without gaps or overlaps.
	totalLength := int64(0)
	for i, chunk := range chunks {
		assert.Equal(t, i, chunk.Index)
		totalLength += chunk.Length
	}
	assert.Equal(t, bigSize, totalLength, "chunks must cover the entire file")

	// Verify chunks are contiguous.
	for i := 1; i < len(chunks); i++ {
		expectedOffset := chunks[i-1].Offset + chunks[i-1].Length
		assert.Equal(t, expectedOffset, chunks[i].Offset,
			"chunk %d should start where chunk %d ends", i, i-1)
	}
}

func TestPlanChunks_CappedAtMaxSubprocesses(t *testing.T) {
	dir := t.TempDir()

	// Create a file that would need many chunks.
	bigContent := strings.Repeat(strings.Repeat("x", 99)+"\n", 100000) // ~10 MB
	path := writeTempFile(t, dir, "capped.txt", bigContent)
	size := int64(len(bigContent))

	cfg := &config.Config{
		MinChunkSizeMB:  1, // Would allow 10 chunks...
		MaxSubprocesses: 3, // ...but we cap at 3.
	}

	chunks, err := PlanChunks(path, size, cfg)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(chunks), 3, "should not exceed MaxSubprocesses")
}

func TestPlanChunks_NegativeFileSize(t *testing.T) {
	_, err := PlanChunks("/any/path", -1, defaultCfg())
	assert.Error(t, err)
}
