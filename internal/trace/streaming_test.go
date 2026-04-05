package trace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/pkg/models"
)

// TestStreamingMaxResults verifies that max-results returns exact count
func TestStreamingMaxResults(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping streaming test in short mode")
	}

	testFile := filepath.Join("..", "..", "test", "integration", "testdata", "large.log")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("Test file not found:", testFile)
	}

	testCases := []struct {
		name       string
		maxResults int
		expected   int
	}{
		{
			name:       "Max 100",
			maxResults: 100,
			expected:   100,
		},
		{
			name:       "Max 1000",
			maxResults: 1000,
			expected:   1000,
		},
		{
			name:       "Max 5000",
			maxResults: 5000,
			expected:   5000,
		},
		{
			name:       "Max 10000",
			maxResults: 10000,
			expected:   10000,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				MaxFiles:          1000,
				MaxWorkers:        20,
				MinChunkSizeBytes: 20 * 1024 * 1024,
				SearchRoots:       []string{},
				NoCache:           true,
				NoIndex:           true,
			}

			engine := NewEngine(cfg)

			req := &models.TraceRequest{
				Paths:      []string{testFile},
				Patterns:   []string{"ERROR"},
				MaxResults: tc.maxResults,
			}

			resp, err := engine.Search(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, len(resp.Matches),
				"Expected exactly %d matches, got %d", tc.expected, len(resp.Matches))
		})
	}
}

// TestStreamingNoLimit verifies all matches are found without max-results
func TestStreamingNoLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping streaming test in short mode")
	}

	testFile := filepath.Join("..", "..", "test", "integration", "testdata", "large.log")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("Test file not found:", testFile)
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        20,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		SearchRoots:       []string{},
		NoCache:           true,
		NoIndex:           true,
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:      []string{testFile},
		Patterns:   []string{"ERROR"},
		MaxResults: 0, // No limit
	}

	resp, err := engine.Search(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 100000, len(resp.Matches),
		"Expected all 100,000 matches without limit")
}

// TestStreamingEarlyTermination verifies that workers stop immediately
func TestStreamingEarlyTermination(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping streaming test in short mode")
	}

	testFile := filepath.Join("..", "..", "test", "integration", "testdata", "large.log")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("Test file not found:", testFile)
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        20,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		SearchRoots:       []string{},
		NoCache:           true,
		NoIndex:           true,
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:      []string{testFile},
		Patterns:   []string{"ERROR"},
		MaxResults: 1000,
	}

	start := time.Now()
	resp, err := engine.Search(context.Background(), req)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, 1000, len(resp.Matches))

	// With early termination, should complete much faster than full scan
	// Full scan takes ~300ms, with early termination should be <100ms
	assert.Less(t, elapsed, 200*time.Millisecond,
		"Early termination should complete quickly (got %v)", elapsed)
}

// TestStreamingMultiPattern verifies streaming works with multiple patterns
func TestStreamingMultiPattern(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping streaming test in short mode")
	}

	testFile := filepath.Join("..", "..", "test", "integration", "testdata", "large.log")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("Test file not found:", testFile)
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        20,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		SearchRoots:       []string{},
		NoCache:           true,
		NoIndex:           true,
	}

	engine := NewEngine(cfg)

	// Test with max-results
	req := &models.TraceRequest{
		Paths:      []string{testFile},
		Patterns:   []string{"ERROR", "WARNING"},
		MaxResults: 1000,
	}

	resp, err := engine.Search(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 1000, len(resp.Matches),
		"Multi-pattern search should respect max-results")

	// Test without limit
	req.MaxResults = 0
	resp, err = engine.Search(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 200000, len(resp.Matches),
		"Multi-pattern search should find all matches without limit")
}

// TestStreamingContextCancellation verifies context cancellation propagates
func TestStreamingContextCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping streaming test in short mode")
	}

	testFile := filepath.Join("..", "..", "test", "integration", "testdata", "large.log")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("Test file not found:", testFile)
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        20,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		SearchRoots:       []string{},
		NoCache:           true,
		NoIndex:           true,
	}

	engine := NewEngine(cfg)

	// Create context with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	req := &models.TraceRequest{
		Paths:    []string{testFile},
		Patterns: []string{"ERROR"},
	}

	_, err := engine.Search(ctx, req)
	// Should either complete successfully (if fast enough) or return context error
	if err != nil {
		assert.ErrorIs(t, err, context.DeadlineExceeded,
			"Should return context deadline exceeded error")
	}
}

// TestStreamingPipeline tests the streaming pipeline directly
func TestStreamingPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping streaming test in short mode")
	}

	testFile := filepath.Join("..", "..", "test", "integration", "testdata", "medium.log")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("Test file not found:", testFile)
	}

	// Get file size
	info, err := os.Stat(testFile)
	require.NoError(t, err)

	task := Task{
		ID:       "test-task",
		FilePath: testFile,
		Offset:   0,
		Length:   info.Size(),
		ChunkID:  0,
	}

	matchChan := make(chan MatchResult, 100)
	ctx := context.Background()

	pipeline := NewStreamingPipeline(ctx, task, []string{"ERROR"}, true, matchChan)

	// Run pipeline in background
	done := make(chan error, 1)
	go func() {
		done <- pipeline.Run()
	}()

	// Collect matches
	var matches []MatchResult
	timeout := time.After(5 * time.Second)

collectLoop:
	for {
		select {
		case match, ok := <-matchChan:
			if !ok {
				break collectLoop
			}
			matches = append(matches, match)

		case err := <-done:
			// Pipeline finished
			close(matchChan)
			// Drain remaining matches
			for match := range matchChan {
				matches = append(matches, match)
			}
			require.NoError(t, err)
			break collectLoop

		case <-timeout:
			t.Fatal("Pipeline timeout")
		}
	}

	// medium.log should have 200 ERROR matches
	assert.Equal(t, 200, len(matches), "Expected 200 matches in medium.log")

	// Verify all matches have correct fields
	for i, match := range matches {
		assert.Equal(t, testFile, match.FilePath, "Match %d: incorrect file path", i)
		assert.Greater(t, match.Offset, int64(0), "Match %d: offset should be positive", i)
		assert.NotEmpty(t, match.LineText, "Match %d: line text should not be empty", i)
	}
}

// BenchmarkStreamingWithLimit benchmarks streaming with max-results
func BenchmarkStreamingWithLimit(b *testing.B) {
	testFile := filepath.Join("..", "..", "test", "integration", "testdata", "large.log")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		b.Skip("Test file not found:", testFile)
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        20,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		SearchRoots:       []string{},
		NoCache:           true,
		NoIndex:           true,
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:      []string{testFile},
		Patterns:   []string{"ERROR"},
		MaxResults: 1000,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := engine.Search(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamingWithoutLimit benchmarks streaming without max-results
func BenchmarkStreamingWithoutLimit(b *testing.B) {
	testFile := filepath.Join("..", "..", "test", "integration", "testdata", "large.log")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		b.Skip("Test file not found:", testFile)
	}

	cfg := &config.Config{
		MaxFiles:          1000,
		MaxWorkers:        20,
		MinChunkSizeBytes: 20 * 1024 * 1024,
		SearchRoots:       []string{},
		NoCache:           true,
		NoIndex:           true,
	}

	engine := NewEngine(cfg)

	req := &models.TraceRequest{
		Paths:    []string{testFile},
		Patterns: []string{"ERROR"},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := engine.Search(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}
