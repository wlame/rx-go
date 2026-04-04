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
