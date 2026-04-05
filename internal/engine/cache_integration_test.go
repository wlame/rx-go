package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/cache"
	"github.com/wlame/rx/internal/models"
)

func TestTrace_CacheHit_ReturnsCachedResults(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_NO_CACHE", "")

	srcDir := t.TempDir()
	path := filepath.Join(srcDir, "app.log")
	content := "ERROR: failure\nok line\nERROR: again\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	patterns := []string{"ERROR"}

	// Pre-populate the cache with a known response.
	lineText1 := "ERROR: failure"
	lineNum1 := 1
	lineText2 := "ERROR: again"
	lineNum2 := 3
	fakeResp := models.NewTraceResponse("cached-req", []string{path})
	fakeResp.Patterns = map[string]string{"p1": "ERROR"}
	fakeResp.Files = map[string]string{"f1": path}
	fakeResp.Matches = []models.Match{
		{Pattern: "p1", File: "f1", Offset: 0, RelativeLineNumber: &lineNum1, AbsoluteLineNumber: 1, LineText: &lineText1},
		{Pattern: "p1", File: "f1", Offset: 15, RelativeLineNumber: &lineNum2, AbsoluteLineNumber: 3, LineText: &lineText2},
	}
	fakeResp.Time = 0.1
	require.NoError(t, cache.Store(cacheDir, patterns, nil, path, &fakeResp))

	// Run trace — should use the cached response.
	req := TraceRequest{
		Paths:    []string{path},
		Patterns: patterns,
		UseCache: true,
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Should have matches (from cache).
	assert.GreaterOrEqual(t, len(resp.Matches), 2, "should return cached matches")
}

func TestTrace_CacheMiss_SearchesAndStoresForLargeFiles(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_NO_CACHE", "")
	// Set a very low threshold so our test file counts as "large".
	t.Setenv("RX_LARGE_FILE_MB", "0")

	srcDir := t.TempDir()
	path := filepath.Join(srcDir, "big.log")
	// Create a file with content.
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "ERROR: line "+strings.Repeat("x", 50))
	}
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	patterns := []string{"ERROR"}

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: patterns,
		UseCache: true,
	}

	// First call — cache miss, should search.
	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Len(t, resp.Matches, 100)

	// Verify that the cache was populated (for large files).
	_, hit, loadErr := cache.Load(cacheDir, patterns, nil, path)
	require.NoError(t, loadErr)
	assert.True(t, hit, "cache should be populated after first search on a large file")
}

func TestTrace_NoCacheEnvDisablesCaching(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_NO_CACHE", "1")
	t.Setenv("RX_LARGE_FILE_MB", "0")

	srcDir := t.TempDir()
	path := filepath.Join(srcDir, "test.log")
	require.NoError(t, os.WriteFile(path, []byte("ERROR here\n"), 0o644))

	req := TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
	}

	resp, err := Trace(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Len(t, resp.Matches, 1)

	// Cache should NOT be populated when RX_NO_CACHE=1.
	_, hit, _ := cache.Load(cacheDir, []string{"ERROR"}, nil, path)
	assert.False(t, hit, "cache should not be populated when RX_NO_CACHE=1")
}

func TestTrace_CachedResultsMatchUncached(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_NO_CACHE", "")
	t.Setenv("RX_LARGE_FILE_MB", "0")

	srcDir := t.TempDir()
	path := filepath.Join(srcDir, "verify.log")
	content := "ERROR: first\nok\nERROR: second\nfine\nERROR: third\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	patterns := []string{"ERROR"}
	req := TraceRequest{
		Paths:    []string{path},
		Patterns: patterns,
	}

	// First search — uncached.
	resp1, err := Trace(context.Background(), req)
	require.NoError(t, err)

	// Second search — should use cache.
	resp2, err := Trace(context.Background(), req)
	require.NoError(t, err)

	// Match counts should be equal.
	assert.Equal(t, len(resp1.Matches), len(resp2.Matches),
		"cached and uncached results should have the same number of matches")

	// Offsets should match.
	for i := range resp1.Matches {
		if i < len(resp2.Matches) {
			assert.Equal(t, resp1.Matches[i].Offset, resp2.Matches[i].Offset,
				"match %d offset should be the same", i)
		}
	}
}
