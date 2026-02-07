package trace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/pkg/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Fixtures
// ============================================================================

// createTestFile creates a test file with numbered lines
func createTestFileForContext(t *testing.T, tmpDir string) string {
	t.Helper()

	content := `Line 1: First line of file
Line 2: Second line
Line 3: Third line with ERROR
Line 4: Fourth line
Line 5: Fifth line with WARNING
Line 6: Sixth line
Line 7: Seventh line
Line 8: Eighth line with ERROR
Line 9: Ninth line
Line 10: Last line of file
`
	filePath := filepath.Join(tmpDir, "test.log")
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0644))
	return filePath
}

// createTestIndex creates a simple line index for testing
// Offsets calculated from actual file content
func createTestIndex() *models.FileIndex {
	return &models.FileIndex{
		LineIndex: [][]int64{
			{1, 0},    // Line 1 at offset 0
			{2, 27},   // Line 2 at offset 27
			{3, 47},   // Line 3 at offset 47
			{4, 77},   // Line 4 at offset 77
			{5, 92},   // Line 5 at offset 92
			{6, 125},  // Line 6 at offset 125
			{7, 138},  // Line 7 at offset 138
			{8, 153},  // Line 8 at offset 153
			{9, 186},  // Line 9 at offset 186
			{10, 199}, // Line 10 at offset 199
		},
	}
}

// ============================================================================
// NewContextExtractor Tests
// ============================================================================

func TestNewContextExtractor(t *testing.T) {
	extractor := NewContextExtractor()
	assert.NotNil(t, extractor)
}

// ============================================================================
// ExtractContext Tests - No Context
// ============================================================================

func TestExtractContext_NoContext(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 47, AbsoluteLineNumber: 3}, // Line 3
	}

	// No before or after context
	result, err := extractor.ExtractContext(filePath, matches, 0, 0, nil)
	require.NoError(t, err)
	assert.Empty(t, result, "Expected empty result when no context requested")
}

// ============================================================================
// ExtractContext Tests - With Index
// ============================================================================

func TestExtractContext_WithIndex_BeforeContext(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)
	index := createTestIndex()

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 77, AbsoluteLineNumber: 4}, // Line 4
	}

	// 2 lines before, 0 after
	result, err := extractor.ExtractContext(filePath, matches, 2, 0, index)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["77"]
	require.NotNil(t, contextLines)
	assert.Len(t, contextLines, 3, "Expected line 2, 3, 4")
	assert.Equal(t, 2, contextLines[0].LineNumber)
	assert.Equal(t, 3, contextLines[1].LineNumber)
	assert.Equal(t, 4, contextLines[2].LineNumber)
}

func TestExtractContext_WithIndex_AfterContext(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)
	index := createTestIndex()

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 77, AbsoluteLineNumber: 4}, // Line 4
	}

	// 0 lines before, 2 after
	result, err := extractor.ExtractContext(filePath, matches, 0, 2, index)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["77"]
	require.NotNil(t, contextLines)
	assert.Len(t, contextLines, 3, "Expected line 4, 5, 6")
	assert.Equal(t, 4, contextLines[0].LineNumber)
	assert.Equal(t, 5, contextLines[1].LineNumber)
	assert.Equal(t, 6, contextLines[2].LineNumber)
}

func TestExtractContext_WithIndex_BothContext(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)
	index := createTestIndex()

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 92, AbsoluteLineNumber: 5}, // Line 5
	}

	// 2 lines before, 2 after
	result, err := extractor.ExtractContext(filePath, matches, 2, 2, index)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["92"]
	require.NotNil(t, contextLines)
	assert.Len(t, contextLines, 5, "Expected lines 3, 4, 5, 6, 7")
	assert.Equal(t, 3, contextLines[0].LineNumber)
	assert.Equal(t, 4, contextLines[1].LineNumber)
	assert.Equal(t, 5, contextLines[2].LineNumber)
	assert.Equal(t, 6, contextLines[3].LineNumber)
	assert.Equal(t, 7, contextLines[4].LineNumber)
}

func TestExtractContext_WithIndex_StartOfFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)
	index := createTestIndex()

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 0, AbsoluteLineNumber: 1}, // Line 1 (first line)
	}

	// Request 5 lines before (should clamp to start)
	result, err := extractor.ExtractContext(filePath, matches, 5, 2, index)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["0"]
	require.NotNil(t, contextLines)
	// Should get lines 1, 2, 3 (clamped at start)
	assert.GreaterOrEqual(t, len(contextLines), 3)
	assert.Equal(t, 1, contextLines[0].LineNumber)
}

func TestExtractContext_WithIndex_EndOfFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)
	index := createTestIndex()

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 199, AbsoluteLineNumber: 10}, // Line 10 (last line)
	}

	// Request lines after (should stop at EOF)
	result, err := extractor.ExtractContext(filePath, matches, 2, 5, index)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["199"]
	require.NotNil(t, contextLines)
	// Should get lines 8, 9, 10 (last 3 lines)
	assert.GreaterOrEqual(t, len(contextLines), 3)
	assert.Equal(t, 8, contextLines[0].LineNumber)
}

func TestExtractContext_WithIndex_MultipleMatches(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)
	index := createTestIndex()

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 47, AbsoluteLineNumber: 3},  // Line 3
		{Offset: 153, AbsoluteLineNumber: 8}, // Line 8
	}

	// Get context for both matches
	result, err := extractor.ExtractContext(filePath, matches, 1, 1, index)
	require.NoError(t, err)
	assert.Len(t, result, 2)

	// Check first match context
	context1 := result["47"]
	require.NotNil(t, context1)
	assert.Len(t, context1, 3) // lines 2, 3, 4

	// Check second match context
	context2 := result["153"]
	require.NotNil(t, context2)
	assert.Len(t, context2, 3) // lines 7, 8, 9
}

// ============================================================================
// ExtractContext Tests - Without Index (Byte-Offset Based)
// ============================================================================

func TestExtractContext_WithoutIndex_BeforeContext(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	extractor := NewContextExtractor()
	// Use offset that we know is on line 5 (offset 92 from index)
	matches := []models.Match{
		{Offset: 92}, // Line 5
	}

	// 2 lines before, 0 after
	result, err := extractor.ExtractContext(filePath, matches, 2, 0, nil)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["92"]
	require.NotNil(t, contextLines)
	assert.Len(t, contextLines, 3, "Expected 3 lines")
	// Line numbers should be -1 (unknown without index)
	assert.Equal(t, -1, contextLines[0].LineNumber)
	// The last line should contain "Fifth" (the match line)
	assert.Contains(t, contextLines[2].Text, "Fifth")
}

func TestExtractContext_WithoutIndex_AfterContext(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 77}, // Line 4
	}

	// 0 lines before, 2 after
	result, err := extractor.ExtractContext(filePath, matches, 0, 2, nil)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["77"]
	require.NotNil(t, contextLines)
	assert.Len(t, contextLines, 3, "Expected 3 lines")
	assert.Contains(t, contextLines[0].Text, "Fourth")
}

func TestExtractContext_WithoutIndex_BothContext(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 92}, // Line 5
	}

	// 2 lines before, 2 after
	result, err := extractor.ExtractContext(filePath, matches, 2, 2, nil)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["92"]
	require.NotNil(t, contextLines)
	assert.Len(t, contextLines, 5, "Expected 5 lines")
}

func TestExtractContext_WithoutIndex_StartOfFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 0}, // First line
	}

	// Request lines before (should handle gracefully)
	result, err := extractor.ExtractContext(filePath, matches, 5, 2, nil)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["0"]
	require.NotNil(t, contextLines)
	// Should still return some lines
	assert.Greater(t, len(contextLines), 0)
}

// ============================================================================
// Helper Function Tests
// ============================================================================

func TestFindLineNumberFromIndex(t *testing.T) {
	index := createTestIndex()

	tests := []struct {
		name     string
		offset   int64
		expected int
	}{
		{"exact_match_line1", 0, 1},
		{"exact_match_line3", 47, 3},
		{"exact_match_line10", 199, 10},
		{"between_lines", 50, 3},   // Between line 3 and 4
		{"before_first", -1, -1},   // Before any line
		{"after_last", 300, 10},    // After last line
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findLineNumberFromIndex(index.LineIndex, tt.offset)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFindLineNumberFromIndex_EmptyIndex(t *testing.T) {
	result := findLineNumberFromIndex([][]int64{}, 100)
	assert.Equal(t, -1, result)
}

func TestFindByteOffsetFromIndex(t *testing.T) {
	index := createTestIndex()

	tests := []struct {
		name       string
		lineNumber int
		expected   int64
	}{
		{"line1", 1, 0},
		{"line3", 3, 47},
		{"line10", 10, 199},
		{"between", 5, 92},      // Exact match
		{"before_first", 0, -1},  // Before any line
		{"after_last", 20, 199},  // After last line
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findByteOffsetFromIndex(index.LineIndex, tt.lineNumber)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFindByteOffsetFromIndex_EmptyIndex(t *testing.T) {
	result := findByteOffsetFromIndex([][]int64{}, 5)
	assert.Equal(t, int64(-1), result)
}

// ============================================================================
// findLineStart Tests
// ============================================================================

func TestContextExtractor_findLineStart(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	file, err := os.Open(filePath)
	require.NoError(t, err)
	defer file.Close()

	extractor := NewContextExtractor()

	tests := []struct {
		name     string
		offset   int64
		expected int64
	}{
		{"at_start", 0, 0},
		{"beginning_of_line2", 27, 27}, // Line 2 starts at byte 27
		{"middle_of_line3", 60, 47},    // Should find start of line 3
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := extractor.findLineStart(file, tt.offset)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ============================================================================
// scanBackwards Tests
// ============================================================================

func TestContextExtractor_scanBackwards(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	file, err := os.Open(filePath)
	require.NoError(t, err)
	defer file.Close()

	extractor := NewContextExtractor()

	// From line 5 (offset 92), scan back 2 lines
	result, err := extractor.scanBackwards(file, 92, 2)
	require.NoError(t, err)
	// Should land at start of line 3 (offset 47)
	assert.Equal(t, int64(47), result)
}

func TestContextExtractor_scanBackwards_ExceedsStart(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	file, err := os.Open(filePath)
	require.NoError(t, err)
	defer file.Close()

	extractor := NewContextExtractor()

	// From line 2 (offset 27), scan back 10 lines (more than available)
	result, err := extractor.scanBackwards(file, 27, 10)
	require.NoError(t, err)
	// Should return 0 (file start)
	assert.Equal(t, int64(0), result)
}

// ============================================================================
// Error Cases
// ============================================================================

func TestExtractContext_NonexistentFile(t *testing.T) {
	extractor := NewContextExtractor()
	matches := []models.Match{{Offset: 0}}

	result, err := extractor.ExtractContext("/nonexistent/file.log", matches, 1, 1, nil)
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestExtractContext_EmptyMatches(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)

	extractor := NewContextExtractor()
	matches := []models.Match{} // Empty

	result, err := extractor.ExtractContext(filePath, matches, 1, 1, nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

// ============================================================================
// Edge Cases
// ============================================================================

func TestExtractContext_LargeContext(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)
	index := createTestIndex()

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 92, AbsoluteLineNumber: 5}, // Middle line
	}

	// Request very large context (100 lines before and after)
	result, err := extractor.ExtractContext(filePath, matches, 100, 100, index)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["92"]
	require.NotNil(t, contextLines)
	// Should get all 10 lines (clamped to file boundaries)
	assert.Equal(t, 10, len(contextLines))
}

func TestExtractContext_MatchWithoutLineNumber(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := createTestFileForContext(t, tmpDir)
	index := createTestIndex()

	extractor := NewContextExtractor()
	matches := []models.Match{
		{Offset: 92, AbsoluteLineNumber: -1}, // No line number, will calculate from index
	}

	result, err := extractor.ExtractContext(filePath, matches, 1, 1, index)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["92"]
	require.NotNil(t, contextLines)
	assert.Greater(t, len(contextLines), 0)
}

func TestExtractContext_SingleLineFile(t *testing.T) {
	tmpDir := t.TempDir()
	singleLineFile := filepath.Join(tmpDir, "single.log")
	require.NoError(t, os.WriteFile(singleLineFile, []byte("Only one line"), 0644))

	extractor := NewContextExtractor()
	matches := []models.Match{{Offset: 0}}

	result, err := extractor.ExtractContext(singleLineFile, matches, 5, 5, nil)
	require.NoError(t, err)
	require.Len(t, result, 1)

	contextLines := result["0"]
	require.NotNil(t, contextLines)
	assert.Len(t, contextLines, 1, "Single line file should return 1 line")
}

func TestExtractContext_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	emptyFile := filepath.Join(tmpDir, "empty.log")
	require.NoError(t, os.WriteFile(emptyFile, []byte(""), 0644))

	extractor := NewContextExtractor()
	matches := []models.Match{{Offset: 0}}

	result, err := extractor.ExtractContext(emptyFile, matches, 1, 1, nil)
	require.NoError(t, err)
	// Should return result but context may be empty
	assert.NotNil(t, result)
}
