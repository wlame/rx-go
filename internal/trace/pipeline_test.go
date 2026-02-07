package trace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPipeline_Run_SimpleMatch(t *testing.T) {
	// Skip if rg is not installed
	if !isRipgrepInstalled() {
		t.Skip("ripgrep (rg) not installed")
	}

	// Create test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: info
line 2: ERROR something went wrong
line 3: info
line 4: ERROR another error
line 5: info
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	// Create task for entire file
	task := Task{
		ID:       "test-task",
		FilePath: testFile,
		Offset:   0,
		Length:   int64(len(content)),
		ChunkID:  0,
	}

	ctx := context.Background()
	patterns := []string{"ERROR"}

	pipeline := NewPipeline(ctx, task, patterns, true)
	matches, err := pipeline.Run()

	require.NoError(t, err)
	assert.Len(t, matches, 2, "Should find 2 ERROR matches")

	// Verify matches contain ERROR
	for _, match := range matches {
		assert.Contains(t, match.LineText, "ERROR")
	}
}

func TestPipeline_Run_NoMatches(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep (rg) not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: info
line 2: debug
line 3: info
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	task := Task{
		ID:       "test-task",
		FilePath: testFile,
		Offset:   0,
		Length:   int64(len(content)),
		ChunkID:  0,
	}

	ctx := context.Background()
	patterns := []string{"ERROR"}

	pipeline := NewPipeline(ctx, task, patterns, true)
	matches, err := pipeline.Run()

	require.NoError(t, err)
	assert.Len(t, matches, 0, "Should find no matches")
}

func TestPipeline_Run_CaseInsensitive(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep (rg) not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: error in lowercase
line 2: ERROR in uppercase
line 3: Error in mixed case
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	task := Task{
		ID:       "test-task",
		FilePath: testFile,
		Offset:   0,
		Length:   int64(len(content)),
		ChunkID:  0,
	}

	ctx := context.Background()
	patterns := []string{"ERROR"}

	// Case insensitive search
	pipeline := NewPipeline(ctx, task, patterns, false)
	matches, err := pipeline.Run()

	require.NoError(t, err)
	assert.Len(t, matches, 3, "Should find all 3 matches (case insensitive)")
}

func TestPipeline_Run_MultiplePatterns(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep (rg) not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: info
line 2: ERROR something
line 3: WARN warning
line 4: ERROR again
line 5: info
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	task := Task{
		ID:       "test-task",
		FilePath: testFile,
		Offset:   0,
		Length:   int64(len(content)),
		ChunkID:  0,
	}

	ctx := context.Background()
	patterns := []string{"ERROR", "WARN"}

	pipeline := NewPipeline(ctx, task, patterns, true)
	matches, err := pipeline.Run()

	require.NoError(t, err)
	assert.Len(t, matches, 3, "Should find 2 ERROR + 1 WARN = 3 matches")
}

func TestPipeline_Run_ChunkMatches(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep (rg) not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	// Create file with known content
	lines := []string{
		"line 0: info\n",
		"line 1: ERROR first\n",
		"line 2: info\n",
		"line 3: ERROR second\n",
		"line 4: info\n",
		"line 5: ERROR third\n",
		"line 6: info\n",
	}

	var content string
	for _, line := range lines {
		content += line
	}
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	// Create task for middle chunk (should only match "ERROR second")
	// Start after "line 1: ERROR first\n"
	offset := int64(len(lines[0]) + len(lines[1]))
	// Length covers lines 2, 3, 4
	length := int64(len(lines[2]) + len(lines[3]) + len(lines[4]))

	task := Task{
		ID:       "test-task",
		FilePath: testFile,
		Offset:   offset,
		Length:   length,
		ChunkID:  1,
	}

	ctx := context.Background()
	patterns := []string{"ERROR"}

	pipeline := NewPipeline(ctx, task, patterns, true)
	matches, err := pipeline.Run()

	require.NoError(t, err)
	assert.Len(t, matches, 1, "Should only find ERROR in chunk range")
	assert.Contains(t, matches[0].LineText, "second")
}

// Helper function to check if ripgrep is installed
func isRipgrepInstalled() bool {
	_, err := os.Stat("/usr/bin/rg")
	if err == nil {
		return true
	}
	_, err = os.Stat("/usr/local/bin/rg")
	return err == nil
}
