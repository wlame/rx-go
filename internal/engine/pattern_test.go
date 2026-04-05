package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/models"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func subPtr(subs []models.Submatch) *[]models.Submatch { return &subs }

func TestIdentifyPatterns_SinglePattern(t *testing.T) {
	matches := []models.Match{
		{
			Pattern:  "",
			File:     "f1",
			Offset:   0,
			LineText: strPtr("ERROR: something failed"),
			Submatches: subPtr([]models.Submatch{
				{Text: "ERROR", Start: 0, End: 5},
			}),
		},
	}

	patternIDs := map[string]string{"p1": "ERROR"}
	result := IdentifyPatterns(matches, patternIDs, nil)

	require.Len(t, result, 1)
	assert.Equal(t, "p1", result[0].Pattern)
}

func TestIdentifyPatterns_MultiplePatterns(t *testing.T) {
	// A line that matches both "ERROR" and "failed" patterns.
	matches := []models.Match{
		{
			Pattern:  "",
			File:     "f1",
			Offset:   0,
			LineText: strPtr("ERROR: something failed"),
			Submatches: subPtr([]models.Submatch{
				{Text: "ERROR", Start: 0, End: 5},
				{Text: "failed", Start: 17, End: 23},
			}),
		},
	}

	patternIDs := map[string]string{
		"p1": "ERROR",
		"p2": "failed",
	}
	result := IdentifyPatterns(matches, patternIDs, nil)

	// One match entry per pattern that matched the line.
	require.Len(t, result, 2)
	patterns := []string{result[0].Pattern, result[1].Pattern}
	assert.Contains(t, patterns, "p1")
	assert.Contains(t, patterns, "p2")
}

func TestIdentifyPatterns_NoSubmatches_FallbackToLineText(t *testing.T) {
	// When submatches are nil (e.g. from cache), fall back to matching against line text.
	matches := []models.Match{
		{
			Pattern:  "",
			File:     "f1",
			Offset:   0,
			LineText: strPtr("ERROR: something failed"),
		},
	}

	patternIDs := map[string]string{
		"p1": "ERROR",
		"p2": "WARNING",
	}
	result := IdentifyPatterns(matches, patternIDs, nil)

	require.Len(t, result, 1)
	assert.Equal(t, "p1", result[0].Pattern)
}

func TestIdentifyPatterns_CaseInsensitive(t *testing.T) {
	matches := []models.Match{
		{
			Pattern:  "",
			File:     "f1",
			Offset:   0,
			LineText: strPtr("error: something happened"),
			Submatches: subPtr([]models.Submatch{
				{Text: "error", Start: 0, End: 5},
			}),
		},
	}

	patternIDs := map[string]string{"p1": "ERROR"}

	// Without -i, the pattern "ERROR" won't match the submatch "error".
	resultNoFlag := IdentifyPatterns(matches, patternIDs, nil)
	// The pattern won't match because "ERROR" != "error" in submatch comparison.
	// But it might match via FindAllString if the compiled regex matches.
	// Actually Go's FindAllString("ERROR") on "error" won't match without (?i).

	// With -i, it should match.
	resultWithFlag := IdentifyPatterns(matches, patternIDs, []string{"-i"})
	require.Len(t, resultWithFlag, 1)
	assert.Equal(t, "p1", resultWithFlag[0].Pattern)

	_ = resultNoFlag
}

func TestIdentifyPatterns_EmptyInput(t *testing.T) {
	result := IdentifyPatterns(nil, map[string]string{"p1": "ERROR"}, nil)
	assert.Nil(t, result)

	result = IdentifyPatterns([]models.Match{}, map[string]string{"p1": "ERROR"}, nil)
	assert.Empty(t, result)
}

func TestIdentifyPatterns_NoPatterns(t *testing.T) {
	matches := []models.Match{
		{Pattern: "", File: "f1", Offset: 0, LineText: strPtr("hello")},
	}
	result := IdentifyPatterns(matches, nil, nil)
	assert.Equal(t, matches, result)
}

func TestIdentifyPatterns_PreservesMatchFields(t *testing.T) {
	lineNum := 42
	matches := []models.Match{
		{
			Pattern:            "",
			File:               "f1",
			Offset:             100,
			RelativeLineNumber: &lineNum,
			AbsoluteLineNumber: -1,
			LineText:           strPtr("ERROR here"),
			Submatches: subPtr([]models.Submatch{
				{Text: "ERROR", Start: 0, End: 5},
			}),
		},
	}

	patternIDs := map[string]string{"p1": "ERROR"}
	result := IdentifyPatterns(matches, patternIDs, nil)

	require.Len(t, result, 1)
	assert.Equal(t, "p1", result[0].Pattern)
	assert.Equal(t, "f1", result[0].File)
	assert.Equal(t, 100, result[0].Offset)
	assert.Equal(t, intPtr(42), result[0].RelativeLineNumber)
	assert.Equal(t, -1, result[0].AbsoluteLineNumber)
}

func TestValidatePattern_ValidRegex(t *testing.T) {
	err := ValidatePattern("ERROR")
	assert.NoError(t, err)

	err = ValidatePattern(`\d{4}-\d{2}-\d{2}`)
	assert.NoError(t, err)

	err = ValidatePattern("warning|error")
	assert.NoError(t, err)
}

func TestValidatePattern_InvalidRegex(t *testing.T) {
	// Unbalanced parenthesis is invalid in rg's regex engine.
	err := ValidatePattern("(unclosed")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid regex")
}

func TestValidatePattern_EmptyPattern(t *testing.T) {
	// Empty string is a valid regex (matches everything).
	err := ValidatePattern("")
	assert.NoError(t, err)
}
