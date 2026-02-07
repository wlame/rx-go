package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx-go/pkg/models"
)

func TestBuilder_BuildIndex(t *testing.T) {
	// Create temporary test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	// Note: Content has 5 actual text lines, but scanner sees 6 lines total
	// because of trailing newline creating an empty 6th line
	content := "line 1\nline 2\nline 3\nline 4\nline 5"
	err := os.WriteFile(testFile, []byte(content), 0644)
	require.NoError(t, err)

	builder := NewBuilder(10) // Small step for testing

	index, err := builder.BuildIndex(testFile, false)
	require.NoError(t, err)
	assert.NotNil(t, index)

	// Check basic fields
	assert.Equal(t, 1, index.Version)
	assert.Equal(t, models.IndexTypeRegular, index.IndexType)
	assert.Equal(t, testFile, index.SourcePath)
	assert.NotEmpty(t, index.SourceModifiedAt)
	assert.NotEmpty(t, index.CreatedAt)
	assert.Equal(t, int64(len(content)), index.SourceSizeBytes)
	assert.NotNil(t, index.BuildTimeSeconds)
	assert.Greater(t, *index.BuildTimeSeconds, 0.0)

	// Check line index
	assert.NotEmpty(t, index.LineIndex)
	assert.Equal(t, int64(1), index.LineIndex[0][0]) // First line is line 1
	assert.Equal(t, int64(0), index.LineIndex[0][1]) // First offset is 0

	// Check total lines
	require.NotNil(t, index.TotalLines)
	assert.Equal(t, 5, *index.TotalLines)
}

func TestBuilder_BuildIndex_WithAnalysis(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := "short\nmedium line\nvery long line here\n\nlast line\n"
	err := os.WriteFile(testFile, []byte(content), 0644)
	require.NoError(t, err)

	builder := NewBuilder(10)
	index, err := builder.BuildIndex(testFile, true)
	require.NoError(t, err)
	require.NotNil(t, index.Analysis)

	analysis := index.Analysis
	assert.Equal(t, 5, analysis.LineCount)
	assert.Equal(t, 1, analysis.EmptyLineCount) // One empty line
	assert.Equal(t, 19, analysis.LineLengthMax) // "very long line here"
	assert.Greater(t, analysis.LineLengthAvg, 0.0)
	assert.Equal(t, models.LineEndingLF, analysis.LineEnding)
}

func TestBuilder_BuildIndex_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "empty.log")

	err := os.WriteFile(testFile, []byte(""), 0644)
	require.NoError(t, err)

	builder := NewBuilder(100)
	index, err := builder.BuildIndex(testFile, true)
	require.NoError(t, err)
	assert.NotNil(t, index)

	require.NotNil(t, index.Analysis)
	assert.Equal(t, 0, index.Analysis.LineCount)
}

func TestBuilder_BuildIndex_LargeFile(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "large.log")

	// Create file with exactly 1000 lines
	f, err := os.Create(testFile)
	require.NoError(t, err)

	for i := 1; i <= 999; i++ {
		_, err = fmt.Fprintf(f, "This is line number %d\n", i)
		require.NoError(t, err)
	}
	// Last line without newline
	_, err = fmt.Fprintf(f, "This is line number 1000")
	require.NoError(t, err)
	f.Close()

	builder := NewBuilder(1024) // 1KB step
	index, err := builder.BuildIndex(testFile, true)
	require.NoError(t, err)

	// Should have sparse index (not every line recorded)
	assert.Less(t, len(index.LineIndex), 1000)
	assert.Greater(t, len(index.LineIndex), 1) // But more than 1

	// Analysis should show 1000 lines
	require.NotNil(t, index.Analysis)
	assert.Equal(t, 1000, index.Analysis.LineCount)
}

func TestFindLineNumber(t *testing.T) {
	lineIndex := [][]int64{
		{1, 0},
		{10, 100},
		{20, 200},
		{30, 300},
	}

	tests := []struct {
		name     string
		offset   int64
		expected int
	}{
		{"exact match first", 0, 1},
		{"exact match middle", 200, 20},
		{"between entries", 150, 10},
		{"before first", -1, 0},
		{"after last", 400, 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FindLineNumber(lineIndex, tt.offset)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFindLineNumber_EmptyIndex(t *testing.T) {
	result := FindLineNumber([][]int64{}, 100)
	assert.Equal(t, -1, result)
}

func TestFindByteOffset(t *testing.T) {
	lineIndex := [][]int64{
		{1, 0},
		{10, 100},
		{20, 200},
		{30, 300},
	}

	tests := []struct {
		name       string
		lineNumber int
		expected   int64
	}{
		{"exact match first", 1, 0},
		{"exact match middle", 20, 200},
		{"between entries", 15, 100},
		{"before first", 0, -1},
		{"after last", 40, 300},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FindByteOffset(lineIndex, tt.lineNumber)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFindByteOffset_EmptyIndex(t *testing.T) {
	result := FindByteOffset([][]int64{}, 10)
	assert.Equal(t, int64(-1), result)
}

// ============================================================================
// NewBuilder Tests
// ============================================================================

func TestNewBuilder_DefaultStepBytes(t *testing.T) {
	// Test with stepBytes = 0 (should use default)
	builder := NewBuilder(0)
	assert.NotNil(t, builder)
	assert.Equal(t, int64(100*1024*1024), builder.stepBytes, "Should use default 100MB step")
}

func TestNewBuilder_NegativeStepBytes(t *testing.T) {
	// Test with negative stepBytes (should use default)
	builder := NewBuilder(-100)
	assert.NotNil(t, builder)
	assert.Equal(t, int64(100*1024*1024), builder.stepBytes, "Should use default 100MB step for negative values")
}

func TestNewBuilder_CustomStepBytes(t *testing.T) {
	// Test with custom stepBytes
	customStep := int64(1024 * 1024) // 1MB
	builder := NewBuilder(customStep)
	assert.NotNil(t, builder)
	assert.Equal(t, customStep, builder.stepBytes, "Should use custom step bytes")
}

func TestNewBuilder_VeryLargeStepBytes(t *testing.T) {
	// Test with very large stepBytes
	largeStep := int64(1024 * 1024 * 1024) // 1GB
	builder := NewBuilder(largeStep)
	assert.NotNil(t, builder)
	assert.Equal(t, largeStep, builder.stepBytes, "Should accept very large step bytes")
}
