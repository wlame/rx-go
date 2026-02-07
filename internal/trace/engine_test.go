package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/models"
)

func TestEngine_Search_SingleFile(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: info
line 2: ERROR something went wrong
line 3: info
line 4: ERROR another error
line 5: info
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 2)
	assert.Len(t, resp.ScannedFiles, 1)
	assert.Contains(t, resp.ScannedFiles, testFile)
	assert.Greater(t, resp.Time, 0.0)
	assert.Greater(t, resp.SearchTimeMs, 0.0)
}

func TestEngine_Search_Directory(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()

	// Create multiple files
	files := map[string]string{
		"app1.log": "ERROR in app1\nINFO line\n",
		"app2.log": "INFO line\nERROR in app2\n",
		"app3.log": "DEBUG line\nINFO line\n",
	}

	for filename, content := range files {
		path := filepath.Join(tmpDir, filename)
		require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{}, // Empty = allow any path
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{tmpDir},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 2)
	assert.Equal(t, 3, len(resp.ScannedFiles))
}

func TestEngine_Search_MaxResults(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	// Create file with 10 ERROR lines
	content := ""
	for i := 0; i < 10; i++ {
		content += "ERROR line\n"
	}
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{}, // Empty = allow any path
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		MaxResults:    3,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	// Note: max_results is a soft limit - tasks already submitted will complete
	// For small files (single chunk), all matches will be found
	// This is expected behavior - max_results prevents NEW tasks from being submitted
	assert.Len(t, resp.Matches, 10) // All matches in single chunk
}

func TestEngine_Search_MultiplePatterns(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: INFO starting
line 2: ERROR something failed
line 3: WARN potential issue
line 4: ERROR critical failure
line 5: INFO finished
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR", "WARN"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 3) // 2 ERROR + 1 WARN
}

func TestEngine_Search_CaseInsensitive(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `error in lowercase
ERROR in uppercase
Error in mixed case
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: false,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 3)
}

func TestEngine_Search_NoMatches(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `line 1: info
line 2: debug
line 3: trace
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 0)
	assert.Len(t, resp.ScannedFiles, 1)
}

func TestEngine_Search_InvalidPath(t *testing.T) {
	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{"/tmp"},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{"/nonexistent/file.log"},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
	}

	_, err := engine.Search(context.Background(), req)
	assert.Error(t, err)
}

func TestEngine_Search_BinaryFile(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	binaryFile := filepath.Join(tmpDir, "binary.dat")

	// Create binary file with null bytes
	content := []byte{0x00, 0x01, 0x02, 0x03, 0x00, 0xFF}
	require.NoError(t, os.WriteFile(binaryFile, content, 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{}, // Empty = allow any path
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{tmpDir},
		Patterns:      []string{"test"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	// Binary file should be skipped
	assert.Len(t, resp.ScannedFiles, 0)
}

// ============================================================================
// Context Extraction Tests (addContextLines)
// ============================================================================

func TestEngine_Search_WithContext_BeforeOnly(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `Line 1: First line
Line 2: Second line
Line 3: ERROR happened here
Line 4: Fourth line
Line 5: Fifth line
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
		NoIndex:           false,
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		BeforeContext: 2,
		AfterContext:  0,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 1)
	assert.Equal(t, 2, resp.BeforeContext)
	assert.Equal(t, 0, resp.AfterContext)

	// Context lines should be extracted
	assert.NotNil(t, resp.ContextLines)
	assert.NotEmpty(t, resp.ContextLines)
}

func TestEngine_Search_WithContext_AfterOnly(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `Line 1: First line
Line 2: ERROR happened here
Line 3: Third line
Line 4: Fourth line
Line 5: Fifth line
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		BeforeContext: 0,
		AfterContext:  2,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 1)
	assert.Equal(t, 0, resp.BeforeContext)
	assert.Equal(t, 2, resp.AfterContext)
	assert.NotNil(t, resp.ContextLines)
}

func TestEngine_Search_WithContext_BothContexts(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `Line 1: First line
Line 2: Second line
Line 3: Third line
Line 4: ERROR happened here
Line 5: Fifth line
Line 6: Sixth line
Line 7: Seventh line
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		BeforeContext: 2,
		AfterContext:  2,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 1)
	assert.Equal(t, 2, resp.BeforeContext)
	assert.Equal(t, 2, resp.AfterContext)
	assert.NotNil(t, resp.ContextLines)

	// Should have context lines for the match
	// Context key is the match offset as string
	match := resp.Matches[0]
	contextKey := fmt.Sprintf("%d", match.Offset)
	assert.Contains(t, resp.ContextLines, contextKey)
}

func TestEngine_Search_WithContext_MultipleMatches(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `Line 1: First line
Line 2: ERROR first error
Line 3: Third line
Line 4: Fourth line
Line 5: ERROR second error
Line 6: Sixth line
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		BeforeContext: 1,
		AfterContext:  1,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 2)

	// Should have context for both matches
	assert.NotNil(t, resp.ContextLines)
	assert.NotEmpty(t, resp.ContextLines)
}

func TestEngine_Search_WithContext_MultipleFiles(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()

	// Create multiple files
	files := map[string]string{
		"app1.log": "Line 1\nERROR in app1\nLine 3\n",
		"app2.log": "Line 1\nLine 2\nERROR in app2\nLine 4\n",
	}

	for filename, content := range files {
		path := filepath.Join(tmpDir, filename)
		require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{}, // Empty = allow any path
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{tmpDir},
		Patterns:      []string{"ERROR"},
		BeforeContext: 1,
		AfterContext:  1,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 2)

	// Should have context for matches in both files
	assert.NotNil(t, resp.ContextLines)
	// At least 2 context groups (one per file)
	assert.GreaterOrEqual(t, len(resp.ContextLines), 2)
}

func TestEngine_Search_WithContext_NoMatches(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `Line 1: info
Line 2: debug
Line 3: trace
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		BeforeContext: 2,
		AfterContext:  2,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 0)
	// No matches = no context lines
	assert.Empty(t, resp.ContextLines)
}

// ============================================================================
// Absolute Line Number Tests (addAbsoluteLineNumbers)
// ============================================================================

func TestEngine_Search_AbsoluteLineNumbers_WithIndex(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")
	cacheDir := filepath.Join(tmpDir, "cache")

	content := `Line 1: First line
Line 2: Second line
Line 3: ERROR happened here
Line 4: Fourth line
Line 5: Fifth line
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	// Truncate file modtime to seconds to match RFC3339 precision
	fileInfo, err := os.Stat(testFile)
	require.NoError(t, err)
	modTime := fileInfo.ModTime().Truncate(time.Second)
	require.NoError(t, os.Chtimes(testFile, modTime, modTime))

	// Build index immediately after writing file, before creating engine
	// Use very small step size for tests to ensure all lines are indexed
	builder := index.NewBuilder(1) // 1 byte step = index every line
	idx, err := builder.BuildIndex(testFile, false)
	require.NoError(t, err)

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
		CacheDir:          cacheDir,
		NoIndex:           false, // Enable indexing
	}

	engine := NewEngine(cfg)

	// Save index to cache
	require.NoError(t, engine.indexStore.Save(idx))

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 1)

	// With index, absolute line number should be set
	match := resp.Matches[0]
	assert.Greater(t, match.AbsoluteLineNumber, 0, "AbsoluteLineNumber should be set when index is available")
	assert.Equal(t, 3, match.AbsoluteLineNumber, "ERROR is on line 3")
}

func TestEngine_Search_AbsoluteLineNumbers_WithoutIndex(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")

	content := `Line 1: First line
Line 2: Second line
Line 3: ERROR happened here
Line 4: Fourth line
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
		NoIndex:           true, // Disable indexing
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 1)

	// Without index, absolute line number should remain -1
	match := resp.Matches[0]
	assert.Equal(t, -1, match.AbsoluteLineNumber, "AbsoluteLineNumber should be -1 when no index")
}

func TestEngine_Search_AbsoluteLineNumbers_MultipleMatches(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.log")
	cacheDir := filepath.Join(tmpDir, "cache")

	content := `Line 1: First line
Line 2: ERROR first error
Line 3: Third line
Line 4: ERROR second error
Line 5: Fifth line
Line 6: ERROR third error
`
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0644))

	// Truncate file modtime to seconds to match RFC3339 precision
	fileInfo, err := os.Stat(testFile)
	require.NoError(t, err)
	modTime := fileInfo.ModTime().Truncate(time.Second)
	require.NoError(t, os.Chtimes(testFile, modTime, modTime))

	// Build index immediately after writing file
	// Use very small step size for tests to ensure all lines are indexed
	builder := index.NewBuilder(1) // 1 byte step = index every line
	idx, err := builder.BuildIndex(testFile, false)
	require.NoError(t, err)

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        2,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{tmpDir},
		CacheDir:          cacheDir,
		NoIndex:           false,
	}

	engine := NewEngine(cfg)

	// Save index to cache
	require.NoError(t, engine.indexStore.Save(idx))

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 3)

	// All matches should have line numbers
	for i, match := range resp.Matches {
		assert.Greater(t, match.AbsoluteLineNumber, 0, "Match %d should have line number", i)
	}

	// Check specific line numbers
	lineNumbers := []int{
		resp.Matches[0].AbsoluteLineNumber,
		resp.Matches[1].AbsoluteLineNumber,
		resp.Matches[2].AbsoluteLineNumber,
	}

	assert.Contains(t, lineNumbers, 2, "Should have match on line 2")
	assert.Contains(t, lineNumbers, 4, "Should have match on line 4")
	assert.Contains(t, lineNumbers, 6, "Should have match on line 6")
}

func TestEngine_Search_AbsoluteLineNumbers_MultipleFiles(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")

	// Create multiple files
	files := map[string]string{
		"app1.log": "Line 1\nLine 2\nERROR in app1\nLine 4\n",
		"app2.log": "Line 1\nERROR in app2\nLine 3\n",
	}

	// Write files and build indexes immediately
	// Use very small step size for tests to ensure all lines are indexed
	builder := index.NewBuilder(1) // 1 byte step = index every line
	indexes := make(map[string]*models.FileIndex)

	for filename, content := range files {
		path := filepath.Join(tmpDir, filename)
		require.NoError(t, os.WriteFile(path, []byte(content), 0644))

		// Truncate file modtime to seconds to match RFC3339 precision
		fileInfo, err := os.Stat(path)
		require.NoError(t, err)
		modTime := fileInfo.ModTime().Truncate(time.Second)
		require.NoError(t, os.Chtimes(path, modTime, modTime))

		// Build index immediately after writing file
		idx, err := builder.BuildIndex(path, false)
		require.NoError(t, err)
		indexes[path] = idx
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{}, // Empty = allow any path
		CacheDir:          cacheDir,
		NoIndex:           false,
	}

	engine := NewEngine(cfg)

	// Save indexes to cache
	for _, idx := range indexes {
		require.NoError(t, engine.indexStore.Save(idx))
	}

	req := &models.TraceRequest{
		Paths:         []string{tmpDir},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Matches, 2)

	// Both matches should have line numbers
	for _, match := range resp.Matches {
		assert.Greater(t, match.AbsoluteLineNumber, 0, "Match should have line number")
	}
}
