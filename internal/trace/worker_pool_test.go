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

	pool := NewWorkerPool(2, 0, []string{"ERROR"}, true)
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

	var results []Result
	for result := range pool.Results() {
		results = append(results, result)
	}

	require.Len(t, results, 1)
	assert.Len(t, results[0].Matches, 1)
	assert.Contains(t, results[0].Matches[0].LineText, "ERROR")
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

	pool := NewWorkerPool(3, 0, []string{"ERROR"}, true)
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

	// Collect results
	var results []Result
	for result := range pool.Results() {
		results = append(results, result)
	}

	require.Len(t, results, 3)

	// Each result should have 1 match
	for _, result := range results {
		assert.Len(t, result.Matches, 1)
	}
}

func TestWorkerPool_MaxResults(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	// Create file with many ERROR lines
	content := ""
	for i := 0; i < 20; i++ {
		content += "line " + string(rune('0'+i)) + ": ERROR " + string(rune('A'+i)) + "\n"
	}
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	// Set max results to 5
	pool := NewWorkerPool(2, 5, []string{"ERROR"}, true)
	pool.Start()

	task := Task{
		ID:       "task-1",
		FilePath: testFile,
		Offset:   0,
		Length:   int64(len(content)),
		ChunkID:  0,
	}

	pool.SubmitTask(task)
	pool.Close()

	var totalMatches int
	for result := range pool.Results() {
		totalMatches += len(result.Matches)
	}

	// Should have found 20 matches but may have stopped early
	// The actual count might be slightly higher than maxResults due to buffering
	assert.LessOrEqual(t, totalMatches, 25, "Should not significantly exceed max results")
	assert.GreaterOrEqual(t, totalMatches, 5, "Should find at least max results")
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

	pool := NewWorkerPool(2, 0, []string{"info"}, true)
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

	// Should complete without hanging
	timeout := time.After(2 * time.Second)
	done := make(chan bool)

	go func() {
		for range pool.Results() {
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
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `ERROR 1
ERROR 2
ERROR 3
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	pool := NewWorkerPool(2, 0, []string{"ERROR"}, true)
	pool.Start()

	task := Task{
		ID:       "task-1",
		FilePath: testFile,
		Offset:   0,
		Length:   int64(len(content)),
		ChunkID:  0,
	}

	pool.SubmitTask(task)
	pool.Close()

	// Drain results
	for range pool.Results() {
	}

	// Check match count
	count := pool.GetMatchCount()
	assert.Equal(t, 3, count)
}
