package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/models"
)

func TestResolveLineNumbers_NoIndex(t *testing.T) {
	matches := []models.Match{
		{File: "f1", Offset: 100, AbsoluteLineNumber: -1},
		{File: "f1", Offset: 200, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": "/tmp/test.log"}

	// nil getIndex means no index available.
	result := ResolveLineNumbers(matches, fileIDs, nil)

	// Without an index, line numbers stay at -1.
	for _, m := range result {
		assert.Equal(t, -1, m.AbsoluteLineNumber)
	}
}

func TestResolveLineNumbers_WithIndex(t *testing.T) {
	// Build a mock file index with line_index entries:
	// line 1 at offset 0, line 50 at offset 500, line 100 at offset 1000
	idx := &models.FileIndex{
		LineIndex: [][]int{
			{1, 0},
			{50, 500},
			{100, 1000},
		},
	}

	matches := []models.Match{
		{File: "f1", Offset: 0, AbsoluteLineNumber: -1},     // Exactly at line 1
		{File: "f1", Offset: 500, AbsoluteLineNumber: -1},   // Exactly at line 50
		{File: "f1", Offset: 750, AbsoluteLineNumber: -1},   // Between line 50 and 100
		{File: "f1", Offset: 1000, AbsoluteLineNumber: -1},  // Exactly at line 100
	}
	fileIDs := map[string]string{"f1": "/tmp/test.log"}

	getIndex := func(path string) *models.FileIndex {
		if path == "/tmp/test.log" {
			return idx
		}
		return nil
	}

	result := ResolveLineNumbers(matches, fileIDs, getIndex)

	// Exact match at offset 0 should resolve to line 1.
	assert.Equal(t, 1, result[0].AbsoluteLineNumber)

	// Exact match at offset 500 should resolve to line 50.
	assert.Equal(t, 50, result[1].AbsoluteLineNumber)

	// Offset 750 is between checkpoints — should resolve to the nearest lower checkpoint (line 50).
	assert.Equal(t, 50, result[2].AbsoluteLineNumber)

	// Exact match at offset 1000 should resolve to line 100.
	assert.Equal(t, 100, result[3].AbsoluteLineNumber)
}

func TestResolveLineNumbers_AlreadyResolved(t *testing.T) {
	// Matches that already have an absolute line number should not be changed.
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

	// Unknown file ID should get -1.
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
	// Verify that getIndex is called only once per file, not once per match.
	idx := &models.FileIndex{
		LineIndex: [][]int{{1, 0}, {100, 1000}},
	}

	matches := []models.Match{
		{File: "f1", Offset: 10, AbsoluteLineNumber: -1},
		{File: "f1", Offset: 20, AbsoluteLineNumber: -1},
		{File: "f1", Offset: 30, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{"f1": "/tmp/test.log"}

	callCount := 0
	getIndex := func(path string) *models.FileIndex {
		callCount++
		return idx
	}

	ResolveLineNumbers(matches, fileIDs, getIndex)

	assert.Equal(t, 1, callCount, "getIndex should be called once per unique file, not per match")
}

func TestLookupLineNumber_ExactMatch(t *testing.T) {
	lineIndex := [][]int{{1, 0}, {50, 500}, {100, 1000}}

	assert.Equal(t, 1, lookupLineNumber(lineIndex, 0))
	assert.Equal(t, 50, lookupLineNumber(lineIndex, 500))
	assert.Equal(t, 100, lookupLineNumber(lineIndex, 1000))
}

func TestLookupLineNumber_BetweenCheckpoints(t *testing.T) {
	lineIndex := [][]int{{1, 0}, {50, 500}, {100, 1000}}

	// Between line 1 (offset 0) and line 50 (offset 500).
	result := lookupLineNumber(lineIndex, 250)
	assert.Equal(t, 1, result, "should return nearest lower checkpoint")

	// Between line 50 (offset 500) and line 100 (offset 1000).
	result = lookupLineNumber(lineIndex, 750)
	assert.Equal(t, 50, result)
}

func TestLookupLineNumber_EmptyIndex(t *testing.T) {
	assert.Equal(t, -1, lookupLineNumber(nil, 0))
	assert.Equal(t, -1, lookupLineNumber([][]int{}, 0))
}

func TestLookupLineNumber_SingleEntry(t *testing.T) {
	lineIndex := [][]int{{1, 0}}

	assert.Equal(t, 1, lookupLineNumber(lineIndex, 0))
	assert.Equal(t, 1, lookupLineNumber(lineIndex, 500))
}

func TestResolveLineNumbers_MultipleFiles(t *testing.T) {
	idx1 := &models.FileIndex{LineIndex: [][]int{{1, 0}, {100, 1000}}}
	idx2 := &models.FileIndex{LineIndex: [][]int{{1, 0}, {200, 2000}}}

	matches := []models.Match{
		{File: "f1", Offset: 500, AbsoluteLineNumber: -1},
		{File: "f2", Offset: 1500, AbsoluteLineNumber: -1},
		{File: "f1", Offset: 1000, AbsoluteLineNumber: -1},
	}
	fileIDs := map[string]string{
		"f1": "/tmp/file1.log",
		"f2": "/tmp/file2.log",
	}

	getIndex := func(path string) *models.FileIndex {
		switch path {
		case "/tmp/file1.log":
			return idx1
		case "/tmp/file2.log":
			return idx2
		}
		return nil
	}

	result := ResolveLineNumbers(matches, fileIDs, getIndex)
	require.Len(t, result, 3)

	// f1 offset 500: between checkpoint 1@0 and 100@1000 → line 1
	assert.Equal(t, 1, result[0].AbsoluteLineNumber)
	// f2 offset 1500: between checkpoint 1@0 and 200@2000 → line 1
	assert.Equal(t, 1, result[1].AbsoluteLineNumber)
	// f1 offset 1000: exactly at checkpoint 100@1000 → line 100
	assert.Equal(t, 100, result[2].AbsoluteLineNumber)
}
