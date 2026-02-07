package trace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx-go/internal/config"
)

func TestChunker_CreateTasks_SmallFile(t *testing.T) {
	// Create temp file < 20MB
	tmpDir := t.TempDir()
	smallFile := filepath.Join(tmpDir, "small.log")

	// Create 1MB file
	content := make([]byte, 1*1024*1024)
	for i := range content {
		content[i] = 'a'
		if i%100 == 99 {
			content[i] = '\n'
		}
	}
	require.NoError(t, os.WriteFile(smallFile, content, 0644))

	cfg := &config.Config{
		MinChunkSizeBytes: 20 * 1024 * 1024,
		MaxWorkers:        20,
	}

	chunker := NewChunker(cfg)
	tasks, err := chunker.CreateTasks(smallFile)

	require.NoError(t, err)
	assert.Len(t, tasks, 1, "Small file should produce 1 chunk")
	assert.Equal(t, int64(0), tasks[0].Offset)
	assert.Equal(t, int64(1*1024*1024), tasks[0].Length)
}

func TestChunker_CreateTasks_LargeFile(t *testing.T) {
	// Create temp file > 100MB
	tmpDir := t.TempDir()
	largeFile := filepath.Join(tmpDir, "large.log")

	// Create 100MB file with newlines
	f, err := os.Create(largeFile)
	require.NoError(t, err)

	// Write 100MB in chunks
	chunk := make([]byte, 1024*1024) // 1MB chunks
	for i := range chunk {
		chunk[i] = 'a'
		if i%100 == 99 {
			chunk[i] = '\n'
		}
	}

	for i := 0; i < 100; i++ {
		_, err := f.Write(chunk)
		require.NoError(t, err)
	}
	f.Close()

	cfg := &config.Config{
		MinChunkSizeBytes: 20 * 1024 * 1024,
		MaxWorkers:        20,
	}

	chunker := NewChunker(cfg)
	tasks, err := chunker.CreateTasks(largeFile)

	require.NoError(t, err)
	assert.Greater(t, len(tasks), 1, "Large file should produce multiple chunks")
	assert.LessOrEqual(t, len(tasks), 20, "Should not exceed max workers")

	// Verify chunks cover entire file
	fileInfo, _ := os.Stat(largeFile)
	fileSize := fileInfo.Size()

	totalCoverage := int64(0)
	for _, task := range tasks {
		totalCoverage += task.Length
	}
	assert.Equal(t, fileSize, totalCoverage, "Chunks should cover entire file")

	// Verify first chunk starts at 0
	assert.Equal(t, int64(0), tasks[0].Offset)

	// Verify chunks are contiguous
	for i := 1; i < len(tasks); i++ {
		expectedOffset := tasks[i-1].Offset + tasks[i-1].Length
		assert.Equal(t, expectedOffset, tasks[i].Offset, "Chunks should be contiguous")
	}
}

func TestChunker_FindNextNewline(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := "line1\nline2\nline3\nline4\n"
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{}
	chunker := NewChunker(cfg)

	// Find newline after position 3 (should find position 6, after "line1\n")
	offset, err := chunker.findNextNewline(testFile, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(6), offset, "Should find position after first newline")

	// Find newline at position 0 (should find position 6)
	offset, err = chunker.findNextNewline(testFile, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(6), offset)

	// Find newline after position 6 (should find position 12)
	offset, err = chunker.findNextNewline(testFile, 6)
	require.NoError(t, err)
	assert.Equal(t, int64(12), offset)
}

func TestChunker_CalculateChunkCount(t *testing.T) {
	cfg := &config.Config{
		MinChunkSizeBytes: 20 * 1024 * 1024,
		MaxWorkers:        20,
	}

	chunker := NewChunker(cfg)

	tests := []struct {
		name       string
		fileSize   int64
		wantChunks int
	}{
		{
			name:       "very small file",
			fileSize:   1 * 1024 * 1024, // 1MB
			wantChunks: 1,
		},
		{
			name:       "exactly min chunk size",
			fileSize:   20 * 1024 * 1024, // 20MB
			wantChunks: 1,
		},
		{
			name:       "double min chunk size",
			fileSize:   40 * 1024 * 1024, // 40MB
			wantChunks: 2,
		},
		{
			name:       "very large file",
			fileSize:   500 * 1024 * 1024, // 500MB
			wantChunks: 20, // Capped at MaxWorkers
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count := chunker.calculateChunkCount(tt.fileSize)
			assert.Equal(t, tt.wantChunks, count)
		})
	}
}
