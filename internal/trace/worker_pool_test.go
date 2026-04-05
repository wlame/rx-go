package trace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerPool_SingleTask(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: info
line 2: ERROR something
line 3: info
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	// Create match channel for streaming
	matchChan := make(chan MatchResult, 10)
	pool := NewWorkerPool(2, []string{"ERROR"}, true, matchChan)
	pool.Start()

	task := Task{
		ID:       "task-1",
		FilePath: testFile,
		Offset:   0,
		Length:   int64(len(content)),
		ChunkID:  0,
	}

	// Submit task
	submitted := pool.SubmitTask(task)
	assert.True(t, submitted)

	// Close and collect results
	pool.Close()
	close(matchChan)

	var matches []MatchResult
	for match := range matchChan {
		matches = append(matches, match)
	}

	require.Len(t, matches, 1)
	assert.Contains(t, matches[0].LineText, "ERROR")
}

func TestWorkerPool_MultipleTasks(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()

	// Create multiple test files
	files := []string{"test1.log", "test2.log", "test3.log"}
	for _, filename := range files {
		testFile := filepath.Join(tmpDir, filename)
		content := `line 1: info
line 2: ERROR in ` + filename + `
line 3: info
`
		require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))
	}

	// Create match channel for streaming
	matchChan := make(chan MatchResult, 10)
	pool := NewWorkerPool(3, []string{"ERROR"}, true, matchChan)
	pool.Start()

	// Submit tasks for all files
	for i, filename := range files {
		testFile := filepath.Join(tmpDir, filename)
		fileInfo, _ := os.Stat(testFile)

		task := Task{
			ID:       filename,
			FilePath: testFile,
			Offset:   0,
			Length:   fileInfo.Size(),
			ChunkID:  i,
		}
		pool.SubmitTask(task)
	}

	pool.Close()
	close(matchChan)

	// Collect matches
	var matches []MatchResult
	for match := range matchChan {
		matches = append(matches, match)
	}

	require.Len(t, matches, 3)

	// Each match should contain ERROR
	for _, match := range matches {
		assert.Contains(t, match.LineText, "ERROR")
	}
}

func TestWorkerPool_MaxResults(t *testing.T) {
	// NOTE: Max results handling is now done by ResultCollector, not WorkerPool
	// See TestStreamingMaxResults in streaming_test.go instead
	t.Skip("Max results is now handled by ResultCollector")
}

func TestWorkerPool_Cancel(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	// Create large file
	content := ""
	for i := 0; i < 1000; i++ {
		content += "line: info\n"
	}
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	matchChan := make(chan MatchResult, 100)
	pool := NewWorkerPool(2, []string{"info"}, true, matchChan)
	pool.Start()

	// Submit task
	task := Task{
		ID:       "task-1",
		FilePath: testFile,
		Offset:   0,
		Length:   int64(len(content)),
		ChunkID:  0,
	}
	pool.SubmitTask(task)

	// Cancel immediately
	pool.Cancel()
	pool.Close()
	close(matchChan)

	// Should complete without hanging
	timeout := time.After(2 * time.Second)
	done := make(chan bool)

	go func() {
		for range matchChan {
			// Drain results
		}
		done <- true
	}()

	select {
	case <-done:
		// Success
	case <-timeout:
		t.Fatal("Pool did not close after cancel")
	}
}

func TestWorkerPool_GetMatchCount(t *testing.T) {
	// NOTE: Match counting is now handled by ResultCollector
	// See TestStreamingMaxResults in streaming_test.go instead
	t.Skip("Match counting is now handled by ResultCollector")
}
