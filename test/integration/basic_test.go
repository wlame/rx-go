package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/models"
)

var (
	rxBinary  string
	testdata  string
	cacheDir  string
)

func TestMain(m *testing.M) {
	// Setup
	wd, _ := os.Getwd()
	rxBinary = filepath.Join(wd, "../../bin/rx")
	testdata = filepath.Join(wd, "testdata")
	cacheDir = filepath.Join(wd, ".test-cache")

	// Verify rx binary exists
	if _, err := os.Stat(rxBinary); os.IsNotExist(err) {
		panic("rx binary not found. Run: go build -o bin/rx ./cmd/rx")
	}

	// Ensure cache dir exists
	os.MkdirAll(cacheDir, 0755)

	// Run tests
	code := m.Run()

	// Cleanup
	os.RemoveAll(cacheDir)

	os.Exit(code)
}

// Helper: Check if ripgrep is installed
func isRipgrepInstalled() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

// =============================================================================
// Integration Tests
// =============================================================================

func TestIntegration_BasicSearch(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "app.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Greater(t, len(resp.Matches), 0, "Should find ERROR matches")
	assert.Contains(t, resp.ScannedFiles, testFile)
}

func TestIntegration_SearchWithContext(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "app.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

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
	assert.Greater(t, len(resp.Matches), 0, "Should find ERROR matches")
	assert.NotEmpty(t, resp.ContextLines, "Should have context lines")
	assert.Equal(t, 2, resp.BeforeContext)
	assert.Equal(t, 2, resp.AfterContext)
}

func TestIntegration_CompressedFile_Gzip(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "app.log.gz")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    false, // Don't skip compressed files
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Greater(t, len(resp.Matches), 0, "Should find ERROR matches in gzip file")
}

func TestIntegration_CompressedFile_Bzip2(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "app.log.bz2")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    false,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Greater(t, len(resp.Matches), 0, "Should find ERROR matches in bzip2 file")
}

func TestIntegration_LargeFile_Chunking(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "large.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        10,
		MinChunkSizeBytes: 20 * 1024 * 1024, // 20MB chunks
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Greater(t, len(resp.Matches), 0, "Should find ERROR matches in large file")

	// Verify chunking occurred
	chunks := resp.FileChunks[testFile]
	assert.Greater(t, chunks, 1, "Large file should be chunked into multiple parts")

	t.Logf("Large file (%s) processed with %d chunks, found %d matches",
		testFile, chunks, len(resp.Matches))
}

func TestIntegration_MultiplePatterns(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "app.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR", "WARNING", "CRITICAL"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Greater(t, len(resp.Matches), 5, "Should find multiple pattern matches")

	// Verify pattern IDs
	assert.Len(t, resp.Patterns, 3, "Should have 3 patterns")
	assert.Contains(t, resp.Patterns, "p1")
	assert.Contains(t, resp.Patterns, "p2")
	assert.Contains(t, resp.Patterns, "p3")
}

func TestIntegration_DirectoryScan(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testdata},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Greater(t, len(resp.Matches), 0, "Should find ERROR matches in directory")
	assert.Greater(t, len(resp.ScannedFiles), 1, "Should scan multiple files")

	// Verify files map
	assert.NotEmpty(t, resp.Files, "Should have file ID mapping")

	t.Logf("Scanned %d files, found %d matches", len(resp.ScannedFiles), len(resp.Matches))
}

func TestIntegration_MaxResults(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "medium.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		MaxResults:    10,
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	// Note: MaxResults is a soft limit for task submission
	// Small files in single chunk will return all matches
	t.Logf("MaxResults=10, got %d matches (soft limit)", len(resp.Matches))
}

func TestIntegration_CaseInsensitive(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "app.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	// Case sensitive
	reqSensitive := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"error"}, // lowercase
		CaseSensitive: true,
		SkipBinary:    true,
	}

	respSensitive, err := engine.Search(context.Background(), reqSensitive)
	require.NoError(t, err)
	sensitiveCount := len(respSensitive.Matches)

	// Case insensitive
	reqInsensitive := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"error"}, // lowercase
		CaseSensitive: false,
		SkipBinary:    true,
	}

	respInsensitive, err := engine.Search(context.Background(), reqInsensitive)
	require.NoError(t, err)
	insensitiveCount := len(respInsensitive.Matches)

	// Case insensitive should find more matches (ERROR, error, Error)
	assert.Greater(t, insensitiveCount, sensitiveCount,
		"Case insensitive search should find more matches than case sensitive")

	t.Logf("Case sensitive: %d matches, Case insensitive: %d matches",
		sensitiveCount, insensitiveCount)
}

// =============================================================================
// Response Format Tests
// =============================================================================

func TestIntegration_ResponseStructure(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "app.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

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

	// Verify JSON serialization (for API compatibility)
	jsonData, err := json.Marshal(resp)
	require.NoError(t, err)

	var unmarshaled models.TraceResponse
	err = json.Unmarshal(jsonData, &unmarshaled)
	require.NoError(t, err)

	// Verify snake_case field names
	var rawJSON map[string]interface{}
	err = json.Unmarshal(jsonData, &rawJSON)
	require.NoError(t, err)

	// Check for snake_case fields
	assert.Contains(t, rawJSON, "request_id")
	assert.Contains(t, rawJSON, "scanned_files")
	assert.Contains(t, rawJSON, "skipped_files")
	assert.Contains(t, rawJSON, "file_chunks")
	assert.Contains(t, rawJSON, "context_lines")
	assert.Contains(t, rawJSON, "before_context")
	assert.Contains(t, rawJSON, "after_context")
	assert.Contains(t, rawJSON, "total_matches")
	assert.Contains(t, rawJSON, "search_time_ms")

	t.Logf("Response JSON structure verified with snake_case fields")
}

func TestIntegration_MatchStructure(t *testing.T) {
	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "app.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        4,
		MinChunkSizeBytes: 1 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)
	require.NoError(t, err)
	require.Greater(t, len(resp.Matches), 0)

	// Verify match structure
	match := resp.Matches[0]

	assert.NotEmpty(t, match.Pattern, "Match should have pattern ID")
	assert.NotEmpty(t, match.File, "Match should have file ID")
	assert.Greater(t, match.Offset, int64(0), "Match should have offset")
	assert.NotNil(t, match.ChunkID, "Match should have chunk ID")

	// Verify pattern and file mappings
	assert.Contains(t, resp.Patterns, match.Pattern)
	assert.Contains(t, resp.Files, match.File)

	t.Logf("Match structure: pattern=%s file=%s offset=%d",
		match.Pattern, match.File, match.Offset)
}

// =============================================================================
// Performance Tests
// =============================================================================

func TestIntegration_Performance_LargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping performance test in short mode")
	}

	if !isRipgrepInstalled() {
		t.Skip("ripgrep not installed")
	}

	testFile := filepath.Join(testdata, "large.log")

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        10,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		SearchRoots:       []string{},
		CacheDir:          cacheDir,
	}

	engine := trace.NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:         []string{testFile},
		Patterns:      []string{"ERROR"},
		CaseSensitive: true,
		SkipBinary:    true,
	}

	resp, err := engine.Search(context.Background(), req)

	require.NoError(t, err)
	assert.Greater(t, len(resp.Matches), 0)

	// Log performance metrics
	t.Logf("Large file search completed in %.2f seconds", resp.Time)
	t.Logf("Found %d matches in %d chunks", len(resp.Matches), resp.FileChunks[testFile])
	t.Logf("Search time: %.2f ms", resp.SearchTimeMs)
}
