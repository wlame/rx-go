package trace

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResultCollector_SingleResult(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	result := Result{
		TaskID:   "task-1",
		FilePath: "/var/log/app.log",
		ChunkID:  0,
		Matches: []MatchResult{
			{Offset: 100, LineText: "ERROR: something", LineNumber: 5},
			{Offset: 200, LineText: "ERROR: another", LineNumber: 10},
		},
	}

	collector.AddResult(result)
	resp := collector.Finalize()

	assert.Len(t, resp.Matches, 2)
	assert.Len(t, resp.ScannedFiles, 1)
	assert.Contains(t, resp.ScannedFiles, "/var/log/app.log")

	// Check pattern and file IDs
	assert.Contains(t, resp.Patterns, "p1")
	assert.Contains(t, resp.Files, "f1")

	// Check matches
	assert.Equal(t, "p1", resp.Matches[0].Pattern)
	assert.Equal(t, "f1", resp.Matches[0].File)
	assert.Equal(t, int64(100), resp.Matches[0].Offset)
}

func TestResultCollector_MultipleFiles(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	results := []Result{
		{
			TaskID:   "task-1",
			FilePath: "/var/log/app1.log",
			ChunkID:  0,
			Matches: []MatchResult{
				{Offset: 100, LineText: "ERROR in app1", LineNumber: 1},
			},
		},
		{
			TaskID:   "task-2",
			FilePath: "/var/log/app2.log",
			ChunkID:  0,
			Matches: []MatchResult{
				{Offset: 200, LineText: "ERROR in app2", LineNumber: 1},
			},
		},
		{
			TaskID:   "task-3",
			FilePath: "/var/log/app3.log",
			ChunkID:  0,
			Matches: []MatchResult{
				{Offset: 300, LineText: "ERROR in app3", LineNumber: 1},
			},
		},
	}

	for _, result := range results {
		collector.AddResult(result)
	}

	resp := collector.Finalize()

	assert.Len(t, resp.Matches, 3)
	assert.Len(t, resp.ScannedFiles, 3)
	assert.Len(t, resp.Files, 3)

	// Files should be f1, f2, f3
	assert.Contains(t, resp.Files, "f1")
	assert.Contains(t, resp.Files, "f2")
	assert.Contains(t, resp.Files, "f3")
}

func TestResultCollector_Deduplication(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	// Simulate two chunks with overlapping match at boundary
	result1 := Result{
		TaskID:   "task-1",
		FilePath: "/var/log/app.log",
		ChunkID:  0,
		Matches: []MatchResult{
			{Offset: 100, LineText: "ERROR: first", LineNumber: 1},
			{Offset: 200, LineText: "ERROR: boundary", LineNumber: 2}, // At boundary
		},
	}

	result2 := Result{
		TaskID:   "task-2",
		FilePath: "/var/log/app.log",
		ChunkID:  1,
		Matches: []MatchResult{
			{Offset: 200, LineText: "ERROR: boundary", LineNumber: 1}, // Duplicate
			{Offset: 300, LineText: "ERROR: third", LineNumber: 2},
		},
	}

	collector.AddResult(result1)
	collector.AddResult(result2)

	resp := collector.Finalize()

	// Should have 3 unique matches, not 4
	assert.Len(t, resp.Matches, 3)

	// Check chunk count
	assert.Equal(t, 2, resp.FileChunks["/var/log/app.log"])
}

func TestResultCollector_Sorting(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	// Add matches out of order
	result := Result{
		TaskID:   "task-1",
		FilePath: "/var/log/app.log",
		ChunkID:  0,
		Matches: []MatchResult{
			{Offset: 300, LineText: "ERROR: third", LineNumber: 3},
			{Offset: 100, LineText: "ERROR: first", LineNumber: 1},
			{Offset: 200, LineText: "ERROR: second", LineNumber: 2},
		},
	}

	collector.AddResult(result)
	resp := collector.Finalize()

	// Should be sorted by offset
	assert.Equal(t, int64(100), resp.Matches[0].Offset)
	assert.Equal(t, int64(200), resp.Matches[1].Offset)
	assert.Equal(t, int64(300), resp.Matches[2].Offset)
}

func TestResultCollector_SkippedFiles(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	collector.AddSkippedFile("/var/log/binary.dat")
	collector.AddSkippedFile("/var/log/large.log")

	resp := collector.Finalize()

	assert.Len(t, resp.SkippedFiles, 2)
	assert.Contains(t, resp.SkippedFiles, "/var/log/binary.dat")
	assert.Contains(t, resp.SkippedFiles, "/var/log/large.log")
}

func TestResultCollector_MultiplePatterns(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR", "WARN", "CRITICAL"})

	// Verify pattern IDs are created
	assert.Len(t, collector.patternIDs, 3)
	assert.Equal(t, "p1", collector.patternIDs["ERROR"])
	assert.Equal(t, "p2", collector.patternIDs["WARN"])
	assert.Equal(t, "p3", collector.patternIDs["CRITICAL"])

	resp := collector.Finalize()

	assert.Len(t, resp.Patterns, 3)
	assert.Equal(t, "ERROR", resp.Patterns["p1"])
	assert.Equal(t, "WARN", resp.Patterns["p2"])
	assert.Equal(t, "CRITICAL", resp.Patterns["p3"])
}

func TestResultCollector_GetMatchCount(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	result := Result{
		TaskID:   "task-1",
		FilePath: "/var/log/app.log",
		ChunkID:  0,
		Matches: []MatchResult{
			{Offset: 100, LineText: "ERROR 1", LineNumber: 1},
			{Offset: 200, LineText: "ERROR 2", LineNumber: 2},
		},
	}

	collector.AddResult(result)

	count := collector.GetMatchCount()
	assert.Equal(t, 2, count)
}

// ============================================================================
// Error Handling Tests (GetErrors)
// ============================================================================

func TestResultCollector_GetErrors_NoErrors(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	// Add successful result
	result := Result{
		TaskID:   "task-1",
		FilePath: "/var/log/app.log",
		ChunkID:  0,
		Matches: []MatchResult{
			{Offset: 100, LineText: "ERROR: test", LineNumber: 1},
		},
		Error: nil,
	}

	collector.AddResult(result)

	// GetErrors should return empty list
	errors := collector.GetErrors()
	assert.Empty(t, errors, "Should have no errors")
}

func TestResultCollector_GetErrors_SingleError(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	// Add result with error
	result := Result{
		TaskID:   "task-1",
		FilePath: "/var/log/app.log",
		ChunkID:  0,
		Error:    assert.AnError,
	}

	collector.AddResult(result)

	// GetErrors should return the error
	errors := collector.GetErrors()
	assert.Len(t, errors, 1)
	assert.Equal(t, assert.AnError, errors[0])
}

func TestResultCollector_GetErrors_MultipleErrors(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	// Add multiple results with errors
	results := []Result{
		{
			TaskID:   "task-1",
			FilePath: "/var/log/app1.log",
			ChunkID:  0,
			Error:    assert.AnError,
		},
		{
			TaskID:   "task-2",
			FilePath: "/var/log/app2.log",
			ChunkID:  0,
			Matches: []MatchResult{
				{Offset: 100, LineText: "ERROR: test", LineNumber: 1},
			},
			Error: nil, // No error
		},
		{
			TaskID:   "task-3",
			FilePath: "/var/log/app3.log",
			ChunkID:  0,
			Error:    assert.AnError,
		},
	}

	for _, result := range results {
		collector.AddResult(result)
	}

	// GetErrors should return 2 errors (task-1 and task-3)
	errors := collector.GetErrors()
	assert.Len(t, errors, 2)
}

func TestResultCollector_ErrorDoesNotAddMatches(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	// Add result with error (even though it has matches)
	result := Result{
		TaskID:   "task-1",
		FilePath: "/var/log/app.log",
		ChunkID:  0,
		Matches: []MatchResult{
			{Offset: 100, LineText: "ERROR: test", LineNumber: 1},
		},
		Error: assert.AnError,
	}

	collector.AddResult(result)

	resp := collector.Finalize()

	// Matches should not be added when there's an error
	assert.Empty(t, resp.Matches, "Matches should not be added when error is present")

	// But the file should still be scanned (tracked)
	assert.Len(t, resp.ScannedFiles, 1)
	assert.Contains(t, resp.ScannedFiles, "/var/log/app.log")

	// Error should be recorded
	errors := collector.GetErrors()
	assert.Len(t, errors, 1)
}

func TestResultCollector_GetErrors_Concurrent(t *testing.T) {
	collector := NewResultCollector([]string{"ERROR"})

	// Add errors concurrently to test thread safety
	done := make(chan bool, 10)

	for i := 0; i < 10; i++ {
		go func(id int) {
			result := Result{
				TaskID:   fmt.Sprintf("task-%d", id),
				FilePath: fmt.Sprintf("/var/log/app%d.log", id),
				ChunkID:  0,
				Error:    assert.AnError,
			}
			collector.AddResult(result)
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// GetErrors should have all 10 errors
	errors := collector.GetErrors()
	assert.Len(t, errors, 10)
}
