package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wlame/rx/internal/models"
)

func makeMatch(file string, offset int) models.Match {
	return models.Match{File: file, Offset: offset, AbsoluteLineNumber: -1}
}

func TestMergeResults_EmptyInput(t *testing.T) {
	result := MergeResults(nil)
	assert.Nil(t, result)

	result = MergeResults([][]models.Match{})
	assert.Nil(t, result)

	result = MergeResults([][]models.Match{{}, {}})
	assert.Nil(t, result)
}

func TestMergeResults_SingleSource(t *testing.T) {
	source := []models.Match{
		makeMatch("f1", 10),
		makeMatch("f1", 20),
		makeMatch("f1", 30),
	}

	result := MergeResults([][]models.Match{source})

	// Single source should be returned directly (same slice).
	assert.Len(t, result, 3)
	assert.Equal(t, 10, result[0].Offset)
	assert.Equal(t, 20, result[1].Offset)
	assert.Equal(t, 30, result[2].Offset)
}

func TestMergeResults_TwoSources_SameFile(t *testing.T) {
	source1 := []models.Match{
		makeMatch("f1", 10),
		makeMatch("f1", 30),
	}
	source2 := []models.Match{
		makeMatch("f1", 20),
		makeMatch("f1", 40),
	}

	result := MergeResults([][]models.Match{source1, source2})

	assert.Len(t, result, 4)
	assert.Equal(t, 10, result[0].Offset)
	assert.Equal(t, 20, result[1].Offset)
	assert.Equal(t, 30, result[2].Offset)
	assert.Equal(t, 40, result[3].Offset)
}

func TestMergeResults_MultipleFiles(t *testing.T) {
	source1 := []models.Match{
		makeMatch("f1", 100),
		makeMatch("f2", 50),
	}
	source2 := []models.Match{
		makeMatch("f1", 200),
		makeMatch("f2", 10),
	}

	result := MergeResults([][]models.Match{source1, source2})

	assert.Len(t, result, 4)

	// Should be sorted by (file, offset).
	assert.Equal(t, "f1", result[0].File)
	assert.Equal(t, 100, result[0].Offset)
	assert.Equal(t, "f1", result[1].File)
	assert.Equal(t, 200, result[1].Offset)
	assert.Equal(t, "f2", result[2].File)
	assert.Equal(t, 10, result[2].Offset)
	assert.Equal(t, "f2", result[3].File)
	assert.Equal(t, 50, result[3].Offset)
}

func TestMergeResults_ThreeSources(t *testing.T) {
	s1 := []models.Match{makeMatch("f1", 10), makeMatch("f1", 40)}
	s2 := []models.Match{makeMatch("f1", 20)}
	s3 := []models.Match{makeMatch("f1", 30), makeMatch("f1", 50)}

	result := MergeResults([][]models.Match{s1, s2, s3})

	assert.Len(t, result, 5)
	for i := 1; i < len(result); i++ {
		assert.LessOrEqual(t, result[i-1].Offset, result[i].Offset,
			"result should be sorted by offset")
	}
}

func TestMergeResults_WithEmptySources(t *testing.T) {
	s1 := []models.Match{makeMatch("f1", 10)}
	empty := []models.Match{}
	s2 := []models.Match{makeMatch("f1", 20)}

	result := MergeResults([][]models.Match{s1, empty, s2})

	assert.Len(t, result, 2)
	assert.Equal(t, 10, result[0].Offset)
	assert.Equal(t, 20, result[1].Offset)
}

func TestTruncateResults_UnderLimit(t *testing.T) {
	matches := []models.Match{
		makeMatch("f1", 10),
		makeMatch("f1", 20),
	}

	result := TruncateResults(matches, 5)
	assert.Len(t, result, 2)
}

func TestTruncateResults_AtLimit(t *testing.T) {
	matches := []models.Match{
		makeMatch("f1", 10),
		makeMatch("f1", 20),
	}

	result := TruncateResults(matches, 2)
	assert.Len(t, result, 2)
}

func TestTruncateResults_OverLimit(t *testing.T) {
	matches := []models.Match{
		makeMatch("f1", 10),
		makeMatch("f1", 20),
		makeMatch("f1", 30),
		makeMatch("f1", 40),
	}

	result := TruncateResults(matches, 2)
	assert.Len(t, result, 2)
	assert.Equal(t, 10, result[0].Offset)
	assert.Equal(t, 20, result[1].Offset)
}

func TestTruncateResults_ZeroLimit(t *testing.T) {
	matches := []models.Match{makeMatch("f1", 10)}

	result := TruncateResults(matches, 0)
	assert.Len(t, result, 1, "zero limit means no truncation")
}

func TestTruncateResults_NegativeLimit(t *testing.T) {
	matches := []models.Match{makeMatch("f1", 10)}

	result := TruncateResults(matches, -1)
	assert.Len(t, result, 1, "negative limit means no truncation")
}
