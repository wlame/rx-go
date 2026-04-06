package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/models"
)

func makeTestResponse(path string) *models.TraceResponse {
	lineText := "ERROR: something broke"
	lineNum := 10
	resp := models.NewTraceResponse("test-req-1", []string{path})
	resp.Patterns = map[string]string{"p1": "ERROR"}
	resp.Files = map[string]string{"f1": path}
	resp.Matches = []models.Match{
		{
			Pattern:            "p1",
			File:               "f1",
			Offset:             100,
			RelativeLineNumber: &lineNum,
			AbsoluteLineNumber: 10,
			LineText:           &lineText,
		},
	}
	resp.Time = 0.5
	return &resp
}

func TestStoreLoadRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)

	// Create a source file to cache results for.
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "app.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR: something broke\nok\n"), 0o644))

	patterns := []string{"ERROR"}
	rgFlags := []string{"-i"}
	resp := makeTestResponse(srcPath)

	// Store.
	err := Store(cacheDir, patterns, rgFlags, srcPath, resp)
	require.NoError(t, err)

	// Load.
	loaded, hit, err := Load(cacheDir, patterns, rgFlags, srcPath)
	require.NoError(t, err)
	assert.True(t, hit)
	require.NotNil(t, loaded)

	assert.Len(t, loaded.Matches, 1)
	assert.Equal(t, "p1", loaded.Matches[0].Pattern)
	assert.Equal(t, 100, loaded.Matches[0].Offset)
}

func TestLoad_CacheMiss(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "missing.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("data\n"), 0o644))

	loaded, hit, err := Load(cacheDir, []string{"NOPE"}, nil, srcPath)
	require.NoError(t, err)
	assert.False(t, hit)
	assert.Nil(t, loaded)
}

func TestLoad_InvalidatedByMtimeChange(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "changing.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\n"), 0o644))

	patterns := []string{"ERROR"}
	resp := makeTestResponse(srcPath)

	// Store a cache entry.
	require.NoError(t, Store(cacheDir, patterns, nil, srcPath, resp))

	// Verify cache hit.
	_, hit, err := Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.True(t, hit)

	// Modify the file to change mtime.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\nmore\n"), 0o644))

	// Cache should now be invalid.
	_, hit, err = Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.False(t, hit, "cache should be invalidated after file modification")
}

func TestLoad_InvalidatedBySizeChange(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "resize.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR\n"), 0o644))

	patterns := []string{"ERROR"}
	resp := makeTestResponse(srcPath)
	require.NoError(t, Store(cacheDir, patterns, nil, srcPath, resp))

	// Change file size.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR extra content\n"), 0o644))

	_, hit, err := Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.False(t, hit)
}

func TestPatternsHash_Deterministic(t *testing.T) {
	h1 := PatternsHash([]string{"error", "warning"}, []string{"-i"})
	h2 := PatternsHash([]string{"error", "warning"}, []string{"-i"})
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 16)
}

func TestPatternsHash_OrderIndependent(t *testing.T) {
	// Patterns are sorted before hashing, so order shouldn't matter.
	h1 := PatternsHash([]string{"error", "warning"}, []string{"-i"})
	h2 := PatternsHash([]string{"warning", "error"}, []string{"-i"})
	assert.Equal(t, h1, h2, "hash should be the same regardless of pattern order")
}

func TestPatternsHash_DifferentPatternsProduceDifferentHashes(t *testing.T) {
	h1 := PatternsHash([]string{"error"}, nil)
	h2 := PatternsHash([]string{"warning"}, nil)
	assert.NotEqual(t, h1, h2)
}

func TestPatternsHash_FlagsAffectHash(t *testing.T) {
	h1 := PatternsHash([]string{"error"}, nil)
	h2 := PatternsHash([]string{"error"}, []string{"-i"})
	assert.NotEqual(t, h1, h2, "matching flags should change the hash")
}

func TestPatternsHash_NonMatchingFlagsIgnored(t *testing.T) {
	// -A and -B are not matching flags, so they should be ignored.
	h1 := PatternsHash([]string{"error"}, []string{"-A", "3"})
	h2 := PatternsHash([]string{"error"}, nil)
	assert.Equal(t, h1, h2, "non-matching flags should not affect the hash")
}

func TestPatternsHash_MatchingFlagsFiltered(t *testing.T) {
	// Only matching flags should be kept.
	h1 := PatternsHash([]string{"error"}, []string{"-i", "-A", "3", "-w"})
	h2 := PatternsHash([]string{"error"}, []string{"-w", "-i"})
	assert.Equal(t, h1, h2)
}

func TestTraceCachePath_Deterministic(t *testing.T) {
	cacheDir := "/tmp/rx-test"
	patterns := []string{"ERROR"}
	rgFlags := []string{"-i"}
	path := "/var/log/app.log"

	p1 := TraceCachePath(cacheDir, patterns, rgFlags, path)
	p2 := TraceCachePath(cacheDir, patterns, rgFlags, path)
	assert.Equal(t, p1, p2)
	assert.Contains(t, p1, "trace_cache")
	assert.True(t, filepath.Ext(p1) == ".json")
}

func TestTraceCachePath_DifferentPatternsProduceDifferentPaths(t *testing.T) {
	cacheDir := "/tmp/rx-test"
	path := "/var/log/app.log"

	p1 := TraceCachePath(cacheDir, []string{"ERROR"}, nil, path)
	p2 := TraceCachePath(cacheDir, []string{"WARNING"}, nil, path)
	assert.NotEqual(t, p1, p2)
}

func TestTraceCachePath_DifferentFilesProduceDifferentPaths(t *testing.T) {
	cacheDir := "/tmp/rx-test"
	patterns := []string{"ERROR"}

	p1 := TraceCachePath(cacheDir, patterns, nil, "/var/log/app1.log")
	p2 := TraceCachePath(cacheDir, patterns, nil, "/var/log/app2.log")
	assert.NotEqual(t, p1, p2)
}

func TestRxNoCacheDisables(t *testing.T) {
	// RX_NO_CACHE should be checked by the caller (engine), not by cache
	// package itself. This test documents that the cache package functions
	// are unconditional — the engine layer checks config.NoCache.
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	t.Setenv("RX_NO_CACHE", "1")

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR\n"), 0o644))

	// Store should still work at the package level.
	resp := makeTestResponse(srcPath)
	err := Store(cacheDir, []string{"ERROR"}, nil, srcPath, resp)
	assert.NoError(t, err, "Store should work regardless of RX_NO_CACHE (caller enforces)")
}

func TestExtractMatchingFlags(t *testing.T) {
	flags := extractMatchingFlags([]string{"-i", "-A", "3", "-w", "--color=never", "--case-sensitive"})
	assert.Equal(t, []string{"--case-sensitive", "-i", "-w"}, flags)
}

func TestExtractMatchingFlags_Empty(t *testing.T) {
	flags := extractMatchingFlags(nil)
	assert.Nil(t, flags)
}

func TestBuildHashJSON_SortedKeys(t *testing.T) {
	// Verify that "flags" comes before "patterns" (alphabetical sort_keys).
	result := buildHashJSON([]string{"b", "a"}, []string{"x"})
	assert.Contains(t, result, `"flags"`)
	assert.Contains(t, result, `"patterns"`)

	// "flags" should appear before "patterns" in the output.
	flagsIdx := indexOf(result, "flags")
	patternsIdx := indexOf(result, "patterns")
	assert.True(t, flagsIdx < patternsIdx, "flags should come before patterns in sorted JSON")
}

// indexOf returns the position of substr in s, or -1 if not found.
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func TestLoad_VersionMismatch(t *testing.T) {
	cacheDir := t.TempDir()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "version.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\n"), 0o644))

	patterns := []string{"ERROR"}
	resp := makeTestResponse(srcPath)

	// Store a valid entry.
	require.NoError(t, Store(cacheDir, patterns, nil, srcPath, resp))

	// Tamper with the cached file to change the version.
	cachePath := TraceCachePath(cacheDir, patterns, nil, srcPath)
	data, err := os.ReadFile(cachePath)
	require.NoError(t, err)
	// Replace "version": 2 with "version": 999.
	tampered := []byte(strings.ReplaceAll(string(data), `"version": 2`, `"version": 999`))
	require.NoError(t, os.WriteFile(cachePath, tampered, 0o644))

	loaded, hit, err := Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.False(t, hit, "cache should be a miss when version doesn't match")
	assert.Nil(t, loaded)
}

func TestLoad_CorruptedJSON(t *testing.T) {
	cacheDir := t.TempDir()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "corrupt.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\n"), 0o644))

	patterns := []string{"ERROR"}
	resp := makeTestResponse(srcPath)

	// Store a valid entry.
	require.NoError(t, Store(cacheDir, patterns, nil, srcPath, resp))

	// Corrupt the cached file.
	cachePath := TraceCachePath(cacheDir, patterns, nil, srcPath)
	require.NoError(t, os.WriteFile(cachePath, []byte("{bad json!!!"), 0o644))

	loaded, hit, err := Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.False(t, hit, "cache should be a miss on corrupted JSON")
	assert.Nil(t, loaded)
}

func TestLoad_PatternHashMismatch(t *testing.T) {
	cacheDir := t.TempDir()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "hash.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\n"), 0o644))

	patterns := []string{"ERROR"}
	resp := makeTestResponse(srcPath)

	// Store with one set of patterns.
	require.NoError(t, Store(cacheDir, patterns, nil, srcPath, resp))

	// Tamper with the patterns_hash in the cache file to cause a mismatch.
	cachePath := TraceCachePath(cacheDir, patterns, nil, srcPath)
	data, err := os.ReadFile(cachePath)
	require.NoError(t, err)
	hash := PatternsHash(patterns, nil)
	tampered := []byte(strings.ReplaceAll(string(data), hash, "0000000000000000"))
	require.NoError(t, os.WriteFile(cachePath, tampered, 0o644))

	loaded, hit, err := Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.False(t, hit, "cache should be a miss when patterns hash doesn't match")
	assert.Nil(t, loaded)
}

func TestLoad_SourceFileDeleted(t *testing.T) {
	cacheDir := t.TempDir()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "deleted.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\n"), 0o644))

	patterns := []string{"ERROR"}
	resp := makeTestResponse(srcPath)
	require.NoError(t, Store(cacheDir, patterns, nil, srcPath, resp))

	// Delete the source file.
	require.NoError(t, os.Remove(srcPath))

	loaded, hit, err := Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.False(t, hit, "cache should be a miss when source file is deleted")
	assert.Nil(t, loaded)
}

func TestStore_CreatesDirectories(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "deep", "nested", "cache")
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "dirs.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\n"), 0o644))

	resp := makeTestResponse(srcPath)
	err := Store(cacheDir, []string{"ERROR"}, nil, srcPath, resp)
	require.NoError(t, err, "Store should create parent directories")

	// Verify the file was created.
	cachePath := TraceCachePath(cacheDir, []string{"ERROR"}, nil, srcPath)
	_, statErr := os.Stat(cachePath)
	assert.NoError(t, statErr, "cached file should exist")
}

func TestBuildHashJSON_NilSlices(t *testing.T) {
	// nil slices should be serialized as empty arrays, not null.
	result := buildHashJSON(nil, nil)
	assert.Contains(t, result, "[]")
	assert.NotContains(t, result, "null")
}

func TestHashString(t *testing.T) {
	h := hashString("/var/log/test.log")
	assert.Len(t, h, 16)
	// Same input should always produce same hash.
	assert.Equal(t, h, hashString("/var/log/test.log"))
	// Different input should produce different hash.
	assert.NotEqual(t, h, hashString("/var/log/other.log"))
}

// --- Edge-case tests: empty, special, unicode, long, many patterns ---

func TestPatternsHash_EmptyPatterns(t *testing.T) {
	// Empty pattern list should still produce a valid 16-char hex hash.
	h := PatternsHash(nil, nil)
	assert.Len(t, h, 16, "empty patterns should produce a 16-char hash")

	h2 := PatternsHash([]string{}, []string{})
	assert.Len(t, h2, 16)
	assert.Equal(t, h, h2, "nil and empty slice should produce the same hash")
}

func TestPatternsHash_SpecialRegexChars(t *testing.T) {
	// Patterns containing regex metacharacters should be hashed verbatim
	// without any escaping, and produce consistent results.
	patterns := []string{`(a+)+`, `[a-z].*`, `\d+\.\d+`, `foo|bar`}
	h1 := PatternsHash(patterns, nil)
	h2 := PatternsHash(patterns, nil)
	assert.Equal(t, h1, h2, "special regex chars should hash consistently")
	assert.Len(t, h1, 16)

	// Different special-char patterns should give a different hash.
	h3 := PatternsHash([]string{`(b+)+`}, nil)
	assert.NotEqual(t, h1, h3)
}

func TestPatternsHash_UnicodePattern(t *testing.T) {
	// Unicode patterns should be hashed correctly.
	patterns := []string{"nihongo", "nono", "uber"}
	h1 := PatternsHash(patterns, nil)
	h2 := PatternsHash(patterns, nil)
	assert.Equal(t, h1, h2, "unicode patterns should hash consistently")
	assert.Len(t, h1, 16)

	// Different unicode should differ.
	h3 := PatternsHash([]string{"zhongwen"}, nil)
	assert.NotEqual(t, h1, h3)
}

func TestPatternsHash_VeryLongPattern(t *testing.T) {
	// A 1000+ character pattern should still produce a valid hash.
	longPattern := strings.Repeat("a", 2000)
	h := PatternsHash([]string{longPattern}, nil)
	assert.Len(t, h, 16, "very long pattern should produce a 16-char hash")

	// Same long pattern should be deterministic.
	h2 := PatternsHash([]string{longPattern}, nil)
	assert.Equal(t, h, h2)
}

func TestPatternsHash_ManyPatterns(t *testing.T) {
	// 25 patterns -- key computation should handle ordering consistently.
	patterns := make([]string, 25)
	for i := range patterns {
		patterns[i] = fmt.Sprintf("pattern_%03d", i)
	}
	h1 := PatternsHash(patterns, nil)
	assert.Len(t, h1, 16)

	// Reversed order should give the same hash (patterns are sorted before hashing).
	reversed := make([]string, len(patterns))
	copy(reversed, patterns)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	h2 := PatternsHash(reversed, nil)
	assert.Equal(t, h1, h2, "many patterns in different order should hash the same")
}

// --- Cache deletion: store, delete file, then load returns miss ---

func TestCacheDeletion_StoreDeleteLoad(t *testing.T) {
	cacheDir := t.TempDir()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "delete_test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\n"), 0o644))

	patterns := []string{"ERROR"}
	resp := makeTestResponse(srcPath)

	// Store a cache entry.
	require.NoError(t, Store(cacheDir, patterns, nil, srcPath, resp))

	// Confirm cache hit.
	_, hit, err := Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.True(t, hit, "cache should be a hit after Store")

	// Delete the cache file directly (the Go package has no Delete func).
	cachePath := TraceCachePath(cacheDir, patterns, nil, srcPath)
	require.NoError(t, os.Remove(cachePath))

	// Load should now miss.
	loaded, hit, err := Load(cacheDir, patterns, nil, srcPath)
	require.NoError(t, err)
	assert.False(t, hit, "cache should be a miss after deletion")
	assert.Nil(t, loaded)
}

// --- Cache info retrieval: store data, verify metadata round-trips ---

func TestCacheInfoRetrieval(t *testing.T) {
	cacheDir := t.TempDir()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "info_test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("ERROR here\nOK\n"), 0o644))

	patterns := []string{"ERROR"}
	rgFlags := []string{"-i"}
	resp := makeTestResponse(srcPath)

	require.NoError(t, Store(cacheDir, patterns, rgFlags, srcPath, resp))

	// Read the raw cache file and verify metadata fields.
	cachePath := TraceCachePath(cacheDir, patterns, rgFlags, srcPath)
	data, err := os.ReadFile(cachePath)
	require.NoError(t, err)

	var entry traceCacheEntry
	require.NoError(t, json.Unmarshal(data, &entry))

	assert.Equal(t, TraceCacheVersion, entry.Version, "version should match")
	assert.Equal(t, PatternsHash(patterns, rgFlags), entry.PatternsHash, "patterns hash should match")
	assert.Equal(t, patterns, entry.Patterns, "patterns should round-trip")
	assert.Equal(t, []string{"-i"}, entry.RgFlags, "only matching flags should be stored")
	assert.NotEmpty(t, entry.CreatedAt, "created_at should be set")
	assert.NotEmpty(t, entry.SourceModifiedAt, "source_modified_at should be set")
	assert.Greater(t, entry.SourceSizeBytes, int64(0), "source_size_bytes should be positive")

	absPath, _ := filepath.Abs(srcPath)
	assert.Equal(t, absPath, entry.SourcePath, "source_path should be absolute")
}

// --- Matching flags: test each individually ---

func TestMatchingFlags_EachFlagIndividually(t *testing.T) {
	// Each matching flag should change the hash compared to no flags.
	baseHash := PatternsHash([]string{"test"}, nil)

	matchingFlagsList := []string{"-i", "-w", "-x", "-F", "-P", "--case-sensitive", "--ignore-case"}
	for _, flag := range matchingFlagsList {
		h := PatternsHash([]string{"test"}, []string{flag})
		assert.NotEqual(t, baseHash, h, "matching flag %q should change the hash", flag)
		assert.Len(t, h, 16)
	}
}

// --- Non-matching flags: test each is excluded ---

func TestNonMatchingFlags_EachExcluded(t *testing.T) {
	// Flags that do NOT affect matching semantics should NOT change the hash.
	baseHash := PatternsHash([]string{"test"}, nil)

	nonMatchingFlagsList := []string{
		"--color", "--color=never", "--no-heading",
		"-A", "3", "-B", "2", "-C", "5",
		"--context", "--line-number", "--count",
		"--json", "--vimgrep", "--sort",
	}
	for _, flag := range nonMatchingFlagsList {
		h := PatternsHash([]string{"test"}, []string{flag})
		assert.Equal(t, baseHash, h, "non-matching flag %q should NOT change the hash", flag)
	}
}

func TestExtractMatchingFlags_AllMatchingFlags(t *testing.T) {
	// Pass every matching flag mixed with non-matching flags.
	input := []string{
		"-i", "-w", "-x", "-F", "-P", "--case-sensitive", "--ignore-case",
		"--color=never", "-A", "3", "-B", "2", "--json",
	}
	got := extractMatchingFlags(input)
	expected := []string{"--case-sensitive", "--ignore-case", "-F", "-P", "-i", "-w", "-x"}
	assert.Equal(t, expected, got, "should extract and sort only matching flags")
}
