package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/models"
)

// makeTestFile creates a temp file with known content and returns its path.
// Each line is "line N\n" (7 bytes for single-digit, 8 for double-digit, etc.).
func makeTestFile(t *testing.T, numLines int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	for i := 1; i <= numLines; i++ {
		_, err := f.WriteString("line " + itoa(i) + "\n")
		require.NoError(t, err)
	}
	return path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// buildSimpleIndex creates a sparse line index with checkpoints every `step` lines.
// It reads the file to get correct offsets.
func buildSimpleIndex(t *testing.T, path string, step int) *models.FileIndex {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	var lineIndex [][]int
	lineNum := 1
	offset := 0
	buf := make([]byte, 1)

	// Record first line.
	lineIndex = append(lineIndex, []int{1, 0})

	for {
		n, err := f.Read(buf)
		if n == 0 || err != nil {
			break
		}
		offset++
		if buf[0] == '\n' {
			lineNum++
			if lineNum%step == 0 {
				lineIndex = append(lineIndex, []int{lineNum, offset})
			}
		}
	}

	return &models.FileIndex{LineIndex: lineIndex}
}

func TestResolveLineNumbers_NoIndex(t *testing.T) {
	matches := []models.Match{
		{File: "f1", Offset: 100, AbsoluteLineNumber: -1},
		{File: "f1", Offset: 200, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": "/tmp/test.log"}

	result := ResolveLineNumbers(matches, fileIDs, nil)

	for _, m := range result {
		assert.Equal(t, -1, m.AbsoluteLineNumber)
	}
}

func TestResolveLineNumbers_ExactLineResolution(t *testing.T) {
	// Create a file with 200 lines, index checkpointed every 50 lines.
	path := makeTestFile(t, 200)
	idx := buildSimpleIndex(t, path, 50)

	// "line 1\n" = 7 bytes, "line 2\n" = 7 bytes, ... up to "line 9\n" = 7 bytes
	// "line 10\n" = 8 bytes, etc.
	// We need actual offsets. Read the file to get them.
	content, err := os.ReadFile(path)
	require.NoError(t, err)

	// Find offsets for specific lines by scanning content.
	offsets := map[int]int{} // line number -> start offset
	lineNum := 1
	offsets[1] = 0
	for i, b := range content {
		if b == '\n' && i+1 < len(content) {
			lineNum++
			offsets[lineNum] = i + 1
		}
	}

	// Test matches at known line starts.
	matches := []models.Match{
		{File: "f1", Offset: offsets[1], AbsoluteLineNumber: -1},
		{File: "f1", Offset: offsets[25], AbsoluteLineNumber: -1},
		{File: "f1", Offset: offsets[50], AbsoluteLineNumber: -1},
		{File: "f1", Offset: offsets[75], AbsoluteLineNumber: -1},
		{File: "f1", Offset: offsets[100], AbsoluteLineNumber: -1},
		{File: "f1", Offset: offsets[150], AbsoluteLineNumber: -1},
		{File: "f1", Offset: offsets[199], AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": path}

	getIndex := func(p string) *models.FileIndex {
		if p == path {
			return idx
		}
		return nil
	}

	result := ResolveLineNumbers(matches, fileIDs, getIndex)

	// Every line number should now be EXACT, not approximate.
	assert.Equal(t, 1, result[0].AbsoluteLineNumber, "line 1")
	assert.Equal(t, 25, result[1].AbsoluteLineNumber, "line 25")
	assert.Equal(t, 50, result[2].AbsoluteLineNumber, "line 50")
	assert.Equal(t, 75, result[3].AbsoluteLineNumber, "line 75")
	assert.Equal(t, 100, result[4].AbsoluteLineNumber, "line 100")
	assert.Equal(t, 150, result[5].AbsoluteLineNumber, "line 150")
	assert.Equal(t, 199, result[6].AbsoluteLineNumber, "line 199")
}

func TestResolveLineNumbers_MidLineOffset(t *testing.T) {
	// An offset in the middle of a line should resolve to that line's number.
	path := makeTestFile(t, 100)
	idx := buildSimpleIndex(t, path, 20)

	content, err := os.ReadFile(path)
	require.NoError(t, err)

	// Find offset of "line 42\n" start, then add 3 bytes (middle of line).
	lineNum := 1
	lineStart := 0
	for i, b := range content {
		if lineNum == 42 {
			lineStart = i
			break
		}
		if b == '\n' {
			lineNum++
		}
	}

	midOffset := lineStart + 3 // middle of "line 42"

	matches := []models.Match{
		{File: "f1", Offset: midOffset, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": path}
	getIndex := func(p string) *models.FileIndex { return idx }

	result := ResolveLineNumbers(matches, fileIDs, getIndex)

	assert.Equal(t, 42, result[0].AbsoluteLineNumber, "mid-line offset should resolve to line 42")
}

func TestResolveLineNumbers_AlreadyResolved(t *testing.T) {
	matches := []models.Match{
		{File: "f1", Offset: 100, AbsoluteLineNumber: 42},
	}
	fileIDs := map[string]string{"f1": "/tmp/test.log"}
	getIndex := func(path string) *models.FileIndex {
		return &models.FileIndex{LineIndex: [][]int{{1, 0}}}
	}

	result := ResolveLineNumbers(matches, fileIDs, getIndex)
	assert.Equal(t, 42, result[0].AbsoluteLineNumber, "should not overwrite existing value")
}

func TestResolveLineNumbers_UnknownFileID(t *testing.T) {
	matches := []models.Match{
		{File: "f99", Offset: 100, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": "/tmp/test.log"}
	getIndex := func(path string) *models.FileIndex {
		return &models.FileIndex{LineIndex: [][]int{{1, 0}}}
	}

	result := ResolveLineNumbers(matches, fileIDs, getIndex)
	assert.Equal(t, -1, result[0].AbsoluteLineNumber)
}

func TestResolveLineNumbers_EmptyIndex(t *testing.T) {
	matches := []models.Match{
		{File: "f1", Offset: 100, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": "/tmp/test.log"}
	getIndex := func(path string) *models.FileIndex {
		return &models.FileIndex{LineIndex: [][]int{}}
	}

	result := ResolveLineNumbers(matches, fileIDs, getIndex)
	assert.Equal(t, -1, result[0].AbsoluteLineNumber)
}

func TestResolveLineNumbers_IndexCaching(t *testing.T) {
	path := makeTestFile(t, 50)
	idx := buildSimpleIndex(t, path, 10)

	matches := []models.Match{
		{File: "f1", Offset: 10, AbsoluteLineNumber: -1},
		{File: "f1", Offset: 20, AbsoluteLineNumber: -1},
		{File: "f1", Offset: 30, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": path}

	callCount := 0
	getIndex := func(p string) *models.FileIndex {
		callCount++
		return idx
	}

	ResolveLineNumbers(matches, fileIDs, getIndex)
	assert.Equal(t, 1, callCount, "getIndex should be called once per unique file")
}

func TestResolveLineNumbers_MultipleFiles(t *testing.T) {
	path1 := makeTestFile(t, 100)
	path2 := makeTestFile(t, 100)
	idx1 := buildSimpleIndex(t, path1, 20)
	idx2 := buildSimpleIndex(t, path2, 20)

	// Read content to get real offsets for line 50.
	content1, _ := os.ReadFile(path1)
	content2, _ := os.ReadFile(path2)

	findLineOffset := func(content []byte, targetLine int) int {
		line := 1
		for i, b := range content {
			if line == targetLine {
				return i
			}
			if b == '\n' {
				line++
			}
		}
		return 0
	}

	off1_50 := findLineOffset(content1, 50)
	off2_75 := findLineOffset(content2, 75)

	matches := []models.Match{
		{File: "f1", Offset: off1_50, AbsoluteLineNumber: -1},
		{File: "f2", Offset: off2_75, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": path1, "f2": path2}
	getIndex := func(p string) *models.FileIndex {
		if p == path1 {
			return idx1
		}
		return idx2
	}

	result := ResolveLineNumbers(matches, fileIDs, getIndex)

	assert.Equal(t, 50, result[0].AbsoluteLineNumber, "file1 line 50")
	assert.Equal(t, 75, result[1].AbsoluteLineNumber, "file2 line 75")
}

func TestFindCheckpoint(t *testing.T) {
	lineIndex := [][]int{{1, 0}, {50, 500}, {100, 1000}}

	line, offset := findCheckpoint(lineIndex, 0)
	assert.Equal(t, 1, line)
	assert.Equal(t, 0, offset)

	line, offset = findCheckpoint(lineIndex, 250)
	assert.Equal(t, 1, line)
	assert.Equal(t, 0, offset)

	line, offset = findCheckpoint(lineIndex, 500)
	assert.Equal(t, 50, line)
	assert.Equal(t, 500, offset)

	line, offset = findCheckpoint(lineIndex, 750)
	assert.Equal(t, 50, line)
	assert.Equal(t, 500, offset)

	line, offset = findCheckpoint(lineIndex, 1500)
	assert.Equal(t, 100, line)
	assert.Equal(t, 1000, offset)
}

func TestFindCheckpoint_Empty(t *testing.T) {
	line, offset := findCheckpoint(nil, 100)
	assert.Equal(t, 1, line)
	assert.Equal(t, 0, offset)
}
