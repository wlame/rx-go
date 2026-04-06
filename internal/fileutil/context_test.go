package fileutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeContextTestFile creates a file with numbered lines for context extraction tests.
// Lines are: "line 1\n", "line 2\n", ..., "line N\n".
// Returns the path and a slice of byte offsets where each line starts.
func makeContextTestFile(t *testing.T, lineCount int) (string, []int64) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "context_test.txt")

	var content []byte
	var offsets []int64
	for i := 1; i <= lineCount; i++ {
		offsets = append(offsets, int64(len(content)))
		line := []byte("line " + itoa(i) + "\n")
		content = append(content, line...)
	}

	require.NoError(t, os.WriteFile(path, content, 0644))
	return path, offsets
}

// itoa is a simple int-to-string without importing strconv in tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// --- GetContext (byte offset) tests ---

func TestGetContext_MiddleOfFile(t *testing.T) {
	path, offsets := makeContextTestFile(t, 10)

	// Get context around line 5 (before=2, after=2).
	lines, err := GetContext(path, offsets[4], 2, 2)
	require.NoError(t, err)

	expected := []string{"line 3", "line 4", "line 5", "line 6", "line 7"}
	assert.Equal(t, expected, lines)
}

func TestGetContext_StartOfFile(t *testing.T) {
	path, offsets := makeContextTestFile(t, 10)

	// Get context around line 1 with before=3 — should clamp to start.
	lines, err := GetContext(path, offsets[0], 3, 2)
	require.NoError(t, err)

	expected := []string{"line 1", "line 2", "line 3"}
	assert.Equal(t, expected, lines)
}

func TestGetContext_EndOfFile(t *testing.T) {
	path, offsets := makeContextTestFile(t, 10)

	// Get context around line 10 with after=5 — should clamp to end.
	lines, err := GetContext(path, offsets[9], 2, 5)
	require.NoError(t, err)

	expected := []string{"line 8", "line 9", "line 10"}
	assert.Equal(t, expected, lines)
}

func TestGetContext_ZeroContext(t *testing.T) {
	path, offsets := makeContextTestFile(t, 10)

	// Just the line at the offset, no surrounding context.
	lines, err := GetContext(path, offsets[4], 0, 0)
	require.NoError(t, err)

	expected := []string{"line 5"}
	assert.Equal(t, expected, lines)
}

func TestGetContext_NegativeOffset(t *testing.T) {
	path, _ := makeContextTestFile(t, 5)

	lines, err := GetContext(path, -1, 1, 1)
	require.NoError(t, err)
	assert.Nil(t, lines, "negative offset should return nil")
}

func TestGetContext_OffsetBeyondFile(t *testing.T) {
	path, _ := makeContextTestFile(t, 5)

	lines, err := GetContext(path, 99999, 1, 1)
	require.NoError(t, err)
	assert.Nil(t, lines, "offset beyond file should return nil")
}

func TestGetContext_NegativeContextValues(t *testing.T) {
	path, _ := makeContextTestFile(t, 5)

	_, err := GetContext(path, 0, -1, 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-negative")
}

func TestGetContext_NonExistentFile(t *testing.T) {
	_, err := GetContext("/nonexistent/file.txt", 0, 1, 1)
	assert.Error(t, err)
}

func TestGetContext_SingleLineFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.txt")
	require.NoError(t, os.WriteFile(path, []byte("only line\n"), 0644))

	lines, err := GetContext(path, 0, 3, 3)
	require.NoError(t, err)

	expected := []string{"only line"}
	assert.Equal(t, expected, lines)
}

// --- GetContextByLines (line number) tests ---

func TestGetContextByLines_MiddleOfFile(t *testing.T) {
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContextByLines(path, 5, 2, 2)
	require.NoError(t, err)

	expected := []string{"line 3", "line 4", "line 5", "line 6", "line 7"}
	assert.Equal(t, expected, lines)
}

func TestGetContextByLines_FirstLine(t *testing.T) {
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContextByLines(path, 1, 3, 2)
	require.NoError(t, err)

	expected := []string{"line 1", "line 2", "line 3"}
	assert.Equal(t, expected, lines)
}

func TestGetContextByLines_LastLine(t *testing.T) {
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContextByLines(path, 10, 2, 5)
	require.NoError(t, err)

	expected := []string{"line 8", "line 9", "line 10"}
	assert.Equal(t, expected, lines)
}

func TestGetContextByLines_ZeroContext(t *testing.T) {
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContextByLines(path, 5, 0, 0)
	require.NoError(t, err)

	expected := []string{"line 5"}
	assert.Equal(t, expected, lines)
}

func TestGetContextByLines_OutOfBounds(t *testing.T) {
	path, _ := makeContextTestFile(t, 5)

	_, err := GetContextByLines(path, 99, 1, 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "out of bounds")
}

func TestGetContextByLines_ZeroLineNumber(t *testing.T) {
	path, _ := makeContextTestFile(t, 5)

	_, err := GetContextByLines(path, 0, 1, 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), ">= 1")
}

func TestGetContextByLines_NegativeContext(t *testing.T) {
	path, _ := makeContextTestFile(t, 5)

	_, err := GetContextByLines(path, 1, -1, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-negative")
}

func TestGetContextByLines_NonExistentFile(t *testing.T) {
	_, err := GetContextByLines("/nonexistent/file.txt", 1, 1, 1)
	assert.Error(t, err)
}

func TestGetContextByLines_LargeContextOnSmallFile(t *testing.T) {
	path, _ := makeContextTestFile(t, 3)

	// Request more context than the file has.
	lines, err := GetContextByLines(path, 2, 10, 10)
	require.NoError(t, err)

	expected := []string{"line 1", "line 2", "line 3"}
	assert.Equal(t, expected, lines)
}

func TestGetContextByLines_FileWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no_trailing.txt")
	require.NoError(t, os.WriteFile(path, []byte("line 1\nline 2\nline 3"), 0644))

	lines, err := GetContextByLines(path, 3, 1, 0)
	require.NoError(t, err)

	expected := []string{"line 2", "line 3"}
	assert.Equal(t, expected, lines)
}

// --- splitLines tests ---

func TestSplitLines_Normal(t *testing.T) {
	lines := splitLines("a\nb\nc\n")
	assert.Equal(t, []string{"a\n", "b\n", "c\n"}, lines)
}

func TestSplitLines_NoTrailingNewline(t *testing.T) {
	lines := splitLines("a\nb\nc")
	assert.Equal(t, []string{"a\n", "b\n", "c"}, lines)
}

func TestSplitLines_Empty(t *testing.T) {
	lines := splitLines("")
	assert.Nil(t, lines)
}

func TestSplitLines_SingleLine(t *testing.T) {
	lines := splitLines("hello\n")
	assert.Equal(t, []string{"hello\n"}, lines)
}

func TestSplitLines_SingleLineNoNewline(t *testing.T) {
	lines := splitLines("hello")
	assert.Equal(t, []string{"hello"}, lines)
}

// --- Part B: Context/samples output correctness ---

func TestGetContextByLines_Context0_ReturnsOnlyTargetLine(t *testing.T) {
	// Context=0 should return exactly 1 line: the target line.
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContextByLines(path, 5, 0, 0)
	require.NoError(t, err)
	assert.Len(t, lines, 1, "context=0 should return exactly 1 line")
	assert.Equal(t, "line 5", lines[0])
}

func TestGetContextByLines_MatchAtFirstLineWithContext(t *testing.T) {
	// Line 1 with before=3: before context is clamped to 0 (can't go before line 1).
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContextByLines(path, 1, 3, 3)
	require.NoError(t, err)

	// Should have line 1 (no before context) + 3 after = 4 lines total.
	expected := []string{"line 1", "line 2", "line 3", "line 4"}
	assert.Equal(t, expected, lines, "before context should be clamped at file start")
}

func TestGetContextByLines_MatchAtLastLineWithContext(t *testing.T) {
	// Last line with after=3: after context is clamped to 0 (can't go past last line).
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContextByLines(path, 10, 3, 3)
	require.NoError(t, err)

	// Should have 3 before + line 10 (no after context) = 4 lines total.
	expected := []string{"line 7", "line 8", "line 9", "line 10"}
	assert.Equal(t, expected, lines, "after context should be clamped at file end")
}

func TestGetContextByLines_AsymmetricContext(t *testing.T) {
	// 1 line before, 5 lines after.
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContextByLines(path, 5, 1, 5)
	require.NoError(t, err)

	// line 4 (1 before) + line 5 (target) + lines 6-10 (5 after) = 7 lines.
	expected := []string{"line 4", "line 5", "line 6", "line 7", "line 8", "line 9", "line 10"}
	assert.Equal(t, expected, lines)
}

func TestGetContext_AtByteOffset0(t *testing.T) {
	// Byte offset 0 is the very first byte of the file.
	path, _ := makeContextTestFile(t, 10)

	lines, err := GetContext(path, 0, 0, 2)
	require.NoError(t, err)

	// Should return line 1 (at offset 0) + 2 after.
	expected := []string{"line 1", "line 2", "line 3"}
	assert.Equal(t, expected, lines)
}

func TestGetContext_AtLastByte(t *testing.T) {
	// Byte offset pointing to the last line of the file.
	path, offsets := makeContextTestFile(t, 10)

	// offsets[9] is the byte offset where "line 10\n" starts.
	lines, err := GetContext(path, offsets[9], 2, 0)
	require.NoError(t, err)

	expected := []string{"line 8", "line 9", "line 10"}
	assert.Equal(t, expected, lines)
}

func TestGetLineRange_EndLineBeyondEOF(t *testing.T) {
	// startLine valid, endLine beyond file -- should return what exists.
	path, _ := makeContextTestFile(t, 5)

	lines, err := GetLineRange(path, 3, 100)
	// Should not error -- just return lines 3-5.
	require.NoError(t, err)

	expected := []string{"line 3", "line 4", "line 5"}
	assert.Equal(t, expected, lines)
}

func TestGetLineRange_EntireFile(t *testing.T) {
	// Read all lines: 1 to totalLines.
	path, _ := makeContextTestFile(t, 5)

	lines, err := GetLineRange(path, 1, 5)
	require.NoError(t, err)

	expected := []string{"line 1", "line 2", "line 3", "line 4", "line 5"}
	assert.Equal(t, expected, lines)
}

func TestGetByteRange_NegativeOffset(t *testing.T) {
	// Negative startByte should return an error.
	path, _ := makeContextTestFile(t, 5)

	_, err := GetByteRange(path, -1, 10)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "startByte must be >= 0")
}

func TestEmptyFile_AllContextFunctions(t *testing.T) {
	// All context functions on a 0-byte file should handle gracefully.
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.txt")
	require.NoError(t, os.WriteFile(emptyPath, []byte{}, 0o644))

	// GetContext on empty file: offset 0 is beyond file (size=0), should return nil.
	lines, err := GetContext(emptyPath, 0, 1, 1)
	require.NoError(t, err)
	assert.Nil(t, lines, "GetContext on empty file should return nil")

	// GetContextByLines: line 1 doesn't exist in an empty file.
	_, err = GetContextByLines(emptyPath, 1, 0, 0)
	assert.Error(t, err, "GetContextByLines on empty file should error (line 1 out of bounds)")

	// GetLineRange: no lines to read.
	lines, err = GetLineRange(emptyPath, 1, 1)
	require.NoError(t, err)
	assert.Empty(t, lines, "GetLineRange on empty file should return empty slice")

	// GetByteRange: startByte=0 is at/beyond file size (0), should return nil.
	lines, err = GetByteRange(emptyPath, 0, 0)
	require.NoError(t, err)
	assert.Nil(t, lines, "GetByteRange on empty file should return nil")

	// CountLines: empty file should have 0 lines.
	count, err := CountLines(emptyPath)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "CountLines on empty file should be 0")
}
