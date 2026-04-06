package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/models"
)

func makeTestIndex(t *testing.T, filePath string) *models.FileIndex {
	t.Helper()
	stat, err := os.Stat(filePath)
	require.NoError(t, err)

	idx := models.NewFileIndex(
		UnifiedIndexVersion,
		models.IndexTypeRegular,
		filePath,
		stat.ModTime().Format(time.RFC3339Nano),
		int(stat.Size()),
	)
	idx.CreatedAt = time.Now().Format(time.RFC3339Nano)
	idx.LineIndex = [][]int{{1, 0}, {100, 5000}, {200, 10000}}
	return &idx
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", dir)

	// Create a source file so we can build a realistic index.
	srcPath := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("line1\nline2\nline3\n"), 0o644))

	idx := makeTestIndex(t, srcPath)
	cachePath := IndexCachePath(dir, srcPath)

	// Save.
	require.NoError(t, Save(cachePath, idx))

	// Load.
	loaded, err := Load(cachePath)
	require.NoError(t, err)

	assert.Equal(t, idx.Version, loaded.Version)
	assert.Equal(t, idx.IndexType, loaded.IndexType)
	assert.Equal(t, idx.SourcePath, loaded.SourcePath)
	assert.Equal(t, idx.SourceModifiedAt, loaded.SourceModifiedAt)
	assert.Equal(t, idx.SourceSizeBytes, loaded.SourceSizeBytes)
	assert.Equal(t, idx.LineIndex, loaded.LineIndex)
}

func TestLoad_NonexistentFile(t *testing.T) {
	_, err := Load("/nonexistent/path/to/index.json")
	assert.Error(t, err)
}

func TestLoad_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("data\n"), 0o644))

	idx := makeTestIndex(t, srcPath)
	idx.Version = 999 // Wrong version.

	cachePath := filepath.Join(dir, "bad_version.json")
	require.NoError(t, Save(cachePath, idx))

	// Override the version check expectation — Save doesn't validate version.
	_, err := Load(cachePath)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "version mismatch")
}

func TestValidate_MatchingFile(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello\n"), 0o644))

	idx := makeTestIndex(t, srcPath)
	assert.True(t, Validate(idx, srcPath))
}

func TestValidate_MtimeChanged(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello\n"), 0o644))

	idx := makeTestIndex(t, srcPath)

	// Modify the file to change mtime.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(srcPath, []byte("hello\n"), 0o644))

	assert.False(t, Validate(idx, srcPath), "should be invalid after file mtime changes")
}

func TestValidate_SizeChanged(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello\n"), 0o644))

	idx := makeTestIndex(t, srcPath)

	// Change file content (different size, same mtime is hard to achieve,
	// but we can just change the index's recorded size).
	idx.SourceSizeBytes = 999
	assert.False(t, Validate(idx, srcPath), "should be invalid when size doesn't match")
}

func TestValidate_FileDeleted(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello\n"), 0o644))

	idx := makeTestIndex(t, srcPath)
	require.NoError(t, os.Remove(srcPath))

	assert.False(t, Validate(idx, srcPath), "should be invalid when file is deleted")
}

func TestInvalidate_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "stale.json")
	require.NoError(t, os.WriteFile(cachePath, []byte("{}"), 0o644))

	require.NoError(t, Invalidate(cachePath))

	_, err := os.Stat(cachePath)
	assert.True(t, os.IsNotExist(err))
}

func TestInvalidate_NonexistentFile(t *testing.T) {
	// Should succeed silently when the file doesn't exist.
	assert.NoError(t, Invalidate("/nonexistent/cache/file.json"))
}

func TestIndexCachePath_Deterministic(t *testing.T) {
	cacheDir := "/tmp/rx-test-cache"
	filePath := "/var/log/app.log"

	path1 := IndexCachePath(cacheDir, filePath)
	path2 := IndexCachePath(cacheDir, filePath)

	assert.Equal(t, path1, path2, "same inputs should produce the same cache path")
	assert.Contains(t, path1, "indexes")
	assert.Contains(t, path1, "app.log")
	assert.True(t, filepath.Ext(path1) == ".json")
}

func TestIndexCachePath_DifferentFilesProduceDifferentPaths(t *testing.T) {
	cacheDir := "/tmp/rx-test-cache"

	path1 := IndexCachePath(cacheDir, "/var/log/app.log")
	path2 := IndexCachePath(cacheDir, "/var/log/other.log")

	assert.NotEqual(t, path1, path2)
}

func TestSafeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple.log", "simple.log"},
		{"my-file_v2.txt", "my-file_v2.txt"},
		{"path/with/slashes", "path_with_slashes"},
		{"special!@#$.log", "special____.log"},
		{"spaces here.txt", "spaces_here.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SafeFilename(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHashPath_Deterministic(t *testing.T) {
	h1 := hashPath("/var/log/app.log")
	h2 := hashPath("/var/log/app.log")
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 16, "hash should be 16 hex characters")
}

func TestHashPath_DifferentInputs(t *testing.T) {
	h1 := hashPath("/var/log/app.log")
	h2 := hashPath("/var/log/other.log")
	assert.NotEqual(t, h1, h2)
}

// TestValidate_PythonTimestampFormat verifies that Go can validate indexes
// built by Python, which stores source_modified_at in datetime.isoformat()
// format (local time, no timezone, microsecond precision).
func TestValidate_PythonTimestampFormat(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello\n"), 0o644))

	stat, err := os.Stat(srcPath)
	require.NoError(t, err)

	// Simulate a Python-built index: datetime.fromtimestamp(mtime).isoformat()
	// produces local time without timezone, with microsecond precision.
	pyMtime := pythonISOFormat(stat.ModTime())

	idx := models.NewFileIndex(
		UnifiedIndexVersion,
		models.IndexTypeRegular,
		srcPath,
		pyMtime,
		int(stat.Size()),
	)

	assert.True(t, Validate(&idx, srcPath),
		"Go Validate should accept Python's isoformat() timestamp: %s", pyMtime)
}

// TestValidate_PythonTimestampNoFractionalSeconds covers the edge case where
// Python's isoformat() omits fractional seconds entirely (microseconds == 0).
func TestValidate_PythonTimestampNoFractionalSeconds(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "test.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello\n"), 0o644))

	stat, err := os.Stat(srcPath)
	require.NoError(t, err)

	// Truncate to second precision — simulates a file whose mtime has no
	// sub-second component, so Python's isoformat() omits the fractional part.
	wholeSecond := stat.ModTime().Truncate(time.Second)
	require.NoError(t, os.Chtimes(srcPath, wholeSecond, wholeSecond))

	pyMtime := pythonISOFormat(wholeSecond)
	assert.NotContains(t, pyMtime, ".", "sanity: no fractional seconds expected")

	idx := models.NewFileIndex(
		UnifiedIndexVersion,
		models.IndexTypeRegular,
		srcPath,
		pyMtime,
		int(stat.Size()),
	)

	assert.True(t, Validate(&idx, srcPath),
		"Go Validate should accept Python's isoformat() without fractional seconds: %s", pyMtime)
}

func TestPythonISOFormat(t *testing.T) {
	// With microseconds.
	ts := time.Date(2024, 1, 15, 10, 30, 45, 123456000, time.Local)
	assert.Equal(t, "2024-01-15T10:30:45.123456", pythonISOFormat(ts))

	// Without fractional seconds.
	ts = time.Date(2024, 1, 15, 10, 30, 45, 0, time.Local)
	assert.Equal(t, "2024-01-15T10:30:45", pythonISOFormat(ts))

	// Nanosecond precision truncated to microseconds.
	ts = time.Date(2024, 1, 15, 10, 30, 45, 123456789, time.Local)
	assert.Equal(t, "2024-01-15T10:30:45.123456", pythonISOFormat(ts))

	// Trailing zeros in microseconds preserved (Python always shows 6 digits).
	ts = time.Date(2024, 1, 15, 10, 30, 45, 120000000, time.Local)
	assert.Equal(t, "2024-01-15T10:30:45.120000", pythonISOFormat(ts))
}

// TestGetLineCount verifies the unified line count accessor works for both
// Go-built indexes (nested Analysis) and Python-built indexes (top-level).
func TestGetLineCount(t *testing.T) {
	lc := 5000

	// Go format: line_count inside Analysis.
	goIdx := models.FileIndex{
		Analysis: &models.IndexAnalysis{LineCount: &lc},
	}
	assert.Equal(t, &lc, goIdx.GetLineCount())

	// Python format: line_count at top level.
	pyIdx := models.FileIndex{
		PyLineCount: &lc,
	}
	assert.Equal(t, &lc, pyIdx.GetLineCount())

	// Go format takes precedence when both present.
	bothIdx := models.FileIndex{
		Analysis:    &models.IndexAnalysis{LineCount: &lc},
		PyLineCount: &lc,
	}
	assert.Equal(t, &lc, bothIdx.GetLineCount())

	// Neither present returns nil.
	emptyIdx := models.FileIndex{}
	assert.Nil(t, emptyIdx.GetLineCount())
}
