package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/cache"
	"github.com/wlame/rx/internal/compression"
	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/index"
	"github.com/wlame/rx/internal/models"
)

// testdataDir returns the absolute path to the testdata directory.
// It walks up from the test file's directory to find the project root.
func testdataDir(t *testing.T) string {
	t.Helper()
	// The test is in internal/engine/, so testdata is at ../../testdata/
	dir, err := filepath.Abs("../../testdata")
	require.NoError(t, err)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skipf("testdata directory not found at %s, skipping integration test", dir)
	}
	return dir
}

// --- Core trace integration tests ---

func TestIntegration_Trace_SinglePattern(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// sample.txt has multiple ERROR lines.
	assert.GreaterOrEqual(t, len(resp.Matches), 5, "sample.txt should have at least 5 ERROR matches")

	// All matches should reference the correct pattern.
	for _, m := range resp.Matches {
		assert.Equal(t, "p1", m.Pattern)
		require.NotNil(t, m.LineText)
		assert.Contains(t, *m.LineText, "ERROR")
	}

	// Matches should be sorted by offset.
	for i := 1; i < len(resp.Matches); i++ {
		assert.LessOrEqual(t, resp.Matches[i-1].Offset, resp.Matches[i].Offset,
			"matches should be sorted by byte offset")
	}

	// Offsets should be non-negative.
	for _, m := range resp.Matches {
		assert.GreaterOrEqual(t, m.Offset, 0)
	}

	assert.Greater(t, resp.Time, 0.0, "time should be positive")
}

func TestIntegration_Trace_MultiplePatterns(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR", "WARNING"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Len(t, resp.Patterns, 2)
	assert.Equal(t, "ERROR", resp.Patterns["p1"])
	assert.Equal(t, "WARNING", resp.Patterns["p2"])

	// Verify pattern IDs are assigned correctly.
	patternsSeen := map[string]bool{}
	for _, m := range resp.Matches {
		patternsSeen[m.Pattern] = true
	}
	assert.True(t, patternsSeen["p1"], "should have p1 (ERROR) matches")
	assert.True(t, patternsSeen["p2"], "should have p2 (WARNING) matches")
}

func TestIntegration_Trace_MaxResults(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:      []string{path},
		Patterns:   []string{"INFO"},
		MaxResults: 3,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.LessOrEqual(t, len(resp.Matches), 3,
		"should not return more than MaxResults matches")
	assert.NotNil(t, resp.MaxResults)
	assert.Equal(t, 3, *resp.MaxResults)
}

func TestIntegration_Trace_EmptyFile(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "empty.txt")

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Empty(t, resp.Matches, "empty file should produce zero matches")
	assert.Len(t, resp.Patterns, 1)
}

func TestIntegration_Trace_BinaryFileSkipped(t *testing.T) {
	td := testdataDir(t)

	// Create a directory containing only the binary file and a text file, then
	// scan the directory. The binary file should be skipped.
	dir := t.TempDir()
	binData, err := os.ReadFile(filepath.Join(td, "binary.bin"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "binary.bin"), binData, 0o644))

	txtContent := "ERROR in text file\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte(txtContent), 0o644))

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{dir},
		Patterns: []string{"ERROR"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Should find the match in the text file, not crash on the binary.
	assert.Len(t, resp.Matches, 1)

	// Binary file should appear in skipped files.
	found := false
	for _, f := range resp.SkippedFiles {
		if strings.Contains(f, "binary.bin") {
			found = true
			break
		}
	}
	assert.True(t, found, "binary.bin should be in skipped files")
}

func TestIntegration_Trace_UnicodeFile(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "unicode.txt")

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// unicode.txt has multiple ERROR lines across CJK, Japanese, Korean, emoji sections.
	assert.GreaterOrEqual(t, len(resp.Matches), 8,
		"unicode.txt should have at least 8 ERROR matches across all sections")

	for _, m := range resp.Matches {
		require.NotNil(t, m.LineText)
		assert.Contains(t, *m.LineText, "ERROR",
			"every match line should contain ERROR")
	}
}

func TestIntegration_Trace_DirectoryRecursive(t *testing.T) {
	td := testdataDir(t)

	// Scan the testdata directory itself. Should find matches across multiple files.
	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{td},
		Patterns: []string{"ERROR"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, len(resp.Files), 2,
		"scanning testdata/ should find at least 2 files with ERROR matches")
	assert.GreaterOrEqual(t, len(resp.Matches), 10,
		"should find many ERROR matches across multiple files")
}

func TestIntegration_Trace_TimestampPattern(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: []string{`^\d{4}-\d{2}-\d{2}`},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Every line in sample.txt starts with a timestamp.
	assert.GreaterOrEqual(t, len(resp.Matches), 90,
		"nearly every line in sample.txt starts with a date pattern")
}

func TestIntegration_Trace_LongLines(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "longlines.txt")

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ERROR"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, len(resp.Matches), 1,
		"longlines.txt should have at least 1 ERROR match")

	// Verify that long-line matches actually have long line text.
	hasLong := false
	for _, m := range resp.Matches {
		if m.LineText != nil && len(*m.LineText) > 1000 {
			hasLong = true
			break
		}
	}
	assert.True(t, hasLong, "at least one match should be on a line >1000 bytes")
}

// --- Compressed file integration tests ---

func TestIntegration_CompressedSearch_SeekableZstd(t *testing.T) {
	td := testdataDir(t)
	samplePath := filepath.Join(td, "sample.txt")

	// Read the original content.
	original, err := os.ReadFile(samplePath)
	require.NoError(t, err)

	// Create a seekable zstd compressed version in a temp dir.
	tmpDir := t.TempDir()
	zstdPath := filepath.Join(tmpDir, "sample.txt.zst")

	zstdFile, err := os.Create(zstdPath)
	require.NoError(t, err)
	_, err = compression.CreateSeekableZstd(
		bytes.NewReader(original), zstdFile,
		compression.CompressOpts{FrameSize: 512, CompressionLevel: 1},
	)
	require.NoError(t, err)
	require.NoError(t, zstdFile.Close())

	// Search the compressed file.
	matches, searchErr := SearchCompressedFile(context.Background(), zstdPath, []string{"ERROR"}, nil, 8)
	require.NoError(t, searchErr)

	// Search the original (uncompressed) file for comparison.
	origResp, origErr := Trace(context.Background(), TraceRequest{
		Paths:    []string{samplePath},
		Patterns: []string{"ERROR"},
	})
	require.NoError(t, origErr)

	// The compressed search should find the same number of matches.
	assert.Equal(t, len(origResp.Matches), len(matches),
		"compressed search should find the same number of matches as uncompressed")
}

func TestIntegration_CompressedSearch_Gzip(t *testing.T) {
	td := testdataDir(t)
	samplePath := filepath.Join(td, "sample.txt")

	original, err := os.ReadFile(samplePath)
	require.NoError(t, err)

	// Create a gzip compressed version.
	tmpDir := t.TempDir()
	gzPath := filepath.Join(tmpDir, "sample.txt.gz")
	gzFile, err := os.Create(gzPath)
	require.NoError(t, err)
	gzWriter := gzip.NewWriter(gzFile)
	_, err = gzWriter.Write(original)
	require.NoError(t, err)
	require.NoError(t, gzWriter.Close())
	require.NoError(t, gzFile.Close())

	// Search the gzip file.
	matches, searchErr := SearchCompressedFile(context.Background(), gzPath, []string{"ERROR"}, nil, 8)
	require.NoError(t, searchErr)

	// Search the original.
	origResp, origErr := Trace(context.Background(), TraceRequest{
		Paths:    []string{samplePath},
		Patterns: []string{"ERROR"},
	})
	require.NoError(t, origErr)

	assert.Equal(t, len(origResp.Matches), len(matches),
		"gzip search should find the same number of matches as uncompressed")
}

// --- Index and cache integration tests ---

func TestIntegration_Index_LineNumberResolution(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	cfg := config.Load()
	idx, err := index.BuildIndex(path, &cfg)
	require.NoError(t, err)
	require.NotNil(t, idx)

	// The index should have line entries.
	assert.GreaterOrEqual(t, len(idx.LineIndex), 1, "index should have line entries")

	// First entry should be line 1 at offset 0.
	assert.Equal(t, 1, idx.LineIndex[0][0], "first line entry should be line 1")
	assert.Equal(t, 0, idx.LineIndex[0][1], "first line entry should be at offset 0")
}

func TestIntegration_Cache_StoreAndLoad(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_NO_CACHE", "")
	// Set a very low threshold so our test file counts as "large" and gets cached.
	t.Setenv("RX_LARGE_FILE_MB", "0")

	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")
	patterns := []string{"ERROR"}

	// First search -- cold, no cache.
	resp1, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: patterns,
		UseCache: true,
		UseIndex: true,
	})
	require.NoError(t, err)

	// Verify cache was populated.
	_, hit, loadErr := cache.Load(cacheDir, patterns, nil, path)
	require.NoError(t, loadErr)
	assert.True(t, hit, "cache should be populated after first search on a 'large' file")

	// Second search -- should use cache.
	resp2, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: patterns,
		UseCache: true,
		UseIndex: true,
	})
	require.NoError(t, err)

	// Cached results should have the same match count.
	assert.Equal(t, len(resp1.Matches), len(resp2.Matches),
		"cached and uncached results should have the same number of matches")

	// Verify all offsets match.
	for i := range resp1.Matches {
		if i < len(resp2.Matches) {
			assert.Equal(t, resp1.Matches[i].Offset, resp2.Matches[i].Offset,
				"match %d offset should be identical between cached and uncached", i)
		}
	}
}

func TestIntegration_Cache_Isolation(t *testing.T) {
	// Each test gets its own cache directory to avoid cross-test contamination.
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_NO_CACHE", "")

	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	// Verify there is no cache hit initially.
	_, hit, _ := cache.Load(cacheDir, []string{"ERROR"}, nil, path)
	assert.False(t, hit, "fresh cache dir should have no cached data")
}

// --- Edge case tests ---

func TestIntegration_EdgeCase_NoMatches(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	resp, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"ZZZZZ_NONEXISTENT_PATTERN_ZZZZZ"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Empty(t, resp.Matches, "no matches should produce empty matches array, not error")
	assert.Len(t, resp.Patterns, 1)
}

func TestIntegration_EdgeCase_InvalidRegex(t *testing.T) {
	td := testdataDir(t)
	path := filepath.Join(td, "sample.txt")

	_, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{path},
		Patterns: []string{"(unclosed"},
	})
	assert.Error(t, err, "invalid regex should return an error")
	assert.Contains(t, err.Error(), "invalid regex")
}

func TestIntegration_EdgeCase_NonexistentFile(t *testing.T) {
	_, err := Trace(context.Background(), TraceRequest{
		Paths:    []string{"/this/path/does/not/exist/at/all.log"},
		Patterns: []string{"ERROR"},
	})
	assert.Error(t, err, "non-existent file should return an error")
}

func TestIntegration_EdgeCase_ConcurrentSearches(t *testing.T) {
	td := testdataDir(t)
	paths := []string{
		filepath.Join(td, "sample.txt"),
		filepath.Join(td, "unicode.txt"),
		filepath.Join(td, "longlines.txt"),
	}

	// Run 5 concurrent traces in parallel goroutines.
	var wg sync.WaitGroup
	errors := make([]error, 5)
	results := make([]*models.TraceResponse, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path := paths[idx%len(paths)]
			resp, err := Trace(context.Background(), TraceRequest{
				Paths:    []string{path},
				Patterns: []string{"ERROR"},
			})
			errors[idx] = err
			results[idx] = resp
		}(i)
	}

	wg.Wait()

	for i, err := range errors {
		assert.NoError(t, err, "concurrent trace %d should not error", i)
		assert.NotNil(t, results[i], "concurrent trace %d should return results", i)
		if results[i] != nil {
			assert.GreaterOrEqual(t, len(results[i].Matches), 1,
				"concurrent trace %d should find matches", i)
		}
	}
}

// --- Race condition testing (9.7/9.8) ---

func TestConcurrentTraces(t *testing.T) {
	td := testdataDir(t)

	// Create 10 distinct test files to avoid any shared state.
	files := make([]string, 10)
	tmpDir := t.TempDir()
	for i := 0; i < 10; i++ {
		content := strings.Repeat("ERROR: line\nINFO: ok\nWARNING: caution\n", 20)
		path := filepath.Join(tmpDir, "concurrent_"+string(rune('a'+i))+".log")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		files[i] = path
	}

	// Also include the real test fixtures for variety.
	files[0] = filepath.Join(td, "sample.txt")
	files[1] = filepath.Join(td, "unicode.txt")
	files[2] = filepath.Join(td, "longlines.txt")

	var wg sync.WaitGroup
	errors := make([]error, 10)
	results := make([]*models.TraceResponse, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := Trace(context.Background(), TraceRequest{
				Paths:    []string{files[idx]},
				Patterns: []string{"ERROR", "WARNING"},
			})
			errors[idx] = err
			results[idx] = resp
		}(i)
	}

	wg.Wait()

	for i, err := range errors {
		assert.NoError(t, err, "concurrent trace %d should not error", i)
		assert.NotNil(t, results[i], "concurrent trace %d should return a response", i)
	}
}
