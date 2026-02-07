package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wlame/rx-go/pkg/models"
)

func TestStore_SaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	store := NewStore(cacheDir)

	// Create test index
	testPath := "/tmp/test.log"
	buildTime := 1.234
	stepBytes := int64(100 * 1024 * 1024)
	totalLines := 100

	index := &models.FileIndex{
		Version:          1,
		IndexType:        models.IndexTypeRegular,
		SourcePath:       testPath,
		SourceModifiedAt: time.Now().Format(time.RFC3339),
		SourceSizeBytes:  1024,
		CreatedAt:        time.Now().Format(time.RFC3339),
		BuildTimeSeconds: &buildTime,
		IndexStepBytes:   &stepBytes,
		LineIndex:        [][]int64{{1, 0}, {10, 100}, {20, 200}},
		TotalLines:       &totalLines,
	}

	// Save index
	err := store.Save(index)
	require.NoError(t, err)

	// Verify cache directory was created
	assert.DirExists(t, cacheDir)

	// Create source file for validation with matching modification time
	err = os.WriteFile(testPath, make([]byte, 1024), 0644)
	require.NoError(t, err)
	defer os.Remove(testPath)

	// Set file modification time to match index
	modTime, _ := time.Parse(time.RFC3339, index.SourceModifiedAt)
	os.Chtimes(testPath, modTime, modTime)

	// Load index
	loaded, err := store.Load(testPath)
	require.NoError(t, err)
	assert.NotNil(t, loaded)

	// Verify fields
	assert.Equal(t, index.Version, loaded.Version)
	assert.Equal(t, index.IndexType, loaded.IndexType)
	assert.Equal(t, index.SourcePath, loaded.SourcePath)
	assert.Equal(t, index.SourceSizeBytes, loaded.SourceSizeBytes)
	assert.Equal(t, len(index.LineIndex), len(loaded.LineIndex))
	assert.Equal(t, *index.TotalLines, *loaded.TotalLines)
}

func TestStore_Load_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	_, err := store.Load("/nonexistent/file.log")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStore_Load_StaleCache(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	store := NewStore(cacheDir)

	// Create source file
	testFile := filepath.Join(tmpDir, "test.log")
	err := os.WriteFile(testFile, []byte("initial content"), 0644)
	require.NoError(t, err)

	// Create index with old modification time
	oldTime := time.Now().Add(-1 * time.Hour)
	buildTime := 1.0
	totalLines := 1

	index := &models.FileIndex{
		Version:          1,
		IndexType:        models.IndexTypeRegular,
		SourcePath:       testFile,
		SourceModifiedAt: oldTime.Format(time.RFC3339),
		SourceSizeBytes:  15,
		CreatedAt:        oldTime.Format(time.RFC3339),
		BuildTimeSeconds: &buildTime,
		LineIndex:        [][]int64{{1, 0}},
		TotalLines:       &totalLines,
	}

	err = store.Save(index)
	require.NoError(t, err)

	// Modify source file (new modification time)
	time.Sleep(10 * time.Millisecond)
	err = os.WriteFile(testFile, []byte("modified content"), 0644)
	require.NoError(t, err)

	// Load should fail due to stale cache
	_, err = store.Load(testFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "stale")
}

func TestStore_Load_SizeMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	store := NewStore(cacheDir)

	// Create source file
	testFile := filepath.Join(tmpDir, "test.log")
	err := os.WriteFile(testFile, []byte("content"), 0644)
	require.NoError(t, err)

	// Create index with wrong size
	buildTime := 1.0
	totalLines := 1

	index := &models.FileIndex{
		Version:          1,
		IndexType:        models.IndexTypeRegular,
		SourcePath:       testFile,
		SourceModifiedAt: time.Now().Format(time.RFC3339),
		SourceSizeBytes:  999, // Wrong size
		CreatedAt:        time.Now().Format(time.RFC3339),
		BuildTimeSeconds: &buildTime,
		LineIndex:        [][]int64{{1, 0}},
		TotalLines:       &totalLines,
	}

	err = store.Save(index)
	require.NoError(t, err)

	// Set file modification time to match index to test size check specifically
	modTime, _ := time.Parse(time.RFC3339, index.SourceModifiedAt)
	os.Chtimes(testFile, modTime, modTime)

	// Load should fail due to size mismatch
	_, err = store.Load(testFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mismatch")
}

func TestStore_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	store := NewStore(cacheDir)

	testPath := "/tmp/test.log"
	buildTime := 1.0
	totalLines := 1

	index := &models.FileIndex{
		Version:          1,
		IndexType:        models.IndexTypeRegular,
		SourcePath:       testPath,
		SourceModifiedAt: time.Now().Format(time.RFC3339),
		SourceSizeBytes:  100,
		CreatedAt:        time.Now().Format(time.RFC3339),
		BuildTimeSeconds: &buildTime,
		LineIndex:        [][]int64{{1, 0}},
		TotalLines:       &totalLines,
	}

	// Save and verify exists
	err := store.Save(index)
	require.NoError(t, err)
	assert.True(t, store.Exists(testPath))

	// Delete
	err = store.Delete(testPath)
	require.NoError(t, err)
	assert.False(t, store.Exists(testPath))

	// Delete again should not error
	err = store.Delete(testPath)
	require.NoError(t, err)
}

func TestStore_Exists(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	testPath := "/tmp/test.log"
	assert.False(t, store.Exists(testPath))

	buildTime := 1.0
	totalLines := 1

	index := &models.FileIndex{
		Version:          1,
		IndexType:        models.IndexTypeRegular,
		SourcePath:       testPath,
		SourceModifiedAt: time.Now().Format(time.RFC3339),
		SourceSizeBytes:  100,
		CreatedAt:        time.Now().Format(time.RFC3339),
		BuildTimeSeconds: &buildTime,
		LineIndex:        [][]int64{{1, 0}},
		TotalLines:       &totalLines,
	}

	err := store.Save(index)
	require.NoError(t, err)
	assert.True(t, store.Exists(testPath))
}

func TestStore_ListCached(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	store := NewStore(cacheDir)

	// Initially empty
	paths, err := store.ListCached()
	require.NoError(t, err)
	assert.Empty(t, paths)

	// Add some indexes
	buildTime := 1.0
	totalLines := 1

	for i := 1; i <= 3; i++ {
		testPath := filepath.Join("/tmp", "test"+string(rune(i))+".log")
		index := &models.FileIndex{
			Version:          1,
			IndexType:        models.IndexTypeRegular,
			SourcePath:       testPath,
			SourceModifiedAt: time.Now().Format(time.RFC3339),
			SourceSizeBytes:  100,
			CreatedAt:        time.Now().Format(time.RFC3339),
			BuildTimeSeconds: &buildTime,
			LineIndex:        [][]int64{{1, 0}},
			TotalLines:       &totalLines,
		}
		err := store.Save(index)
		require.NoError(t, err)
	}

	// List should return 3 paths
	paths, err = store.ListCached()
	require.NoError(t, err)
	assert.Len(t, paths, 3)
}

func TestStore_ClearAll(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	store := NewStore(cacheDir)

	// Add some indexes
	buildTime := 1.0
	totalLines := 1

	for i := 1; i <= 3; i++ {
		testPath := filepath.Join("/tmp", "test"+string(rune(i))+".log")
		index := &models.FileIndex{
			Version:          1,
			IndexType:        models.IndexTypeRegular,
			SourcePath:       testPath,
			SourceModifiedAt: time.Now().Format(time.RFC3339),
			SourceSizeBytes:  100,
			CreatedAt:        time.Now().Format(time.RFC3339),
			BuildTimeSeconds: &buildTime,
			LineIndex:        [][]int64{{1, 0}},
			TotalLines:       &totalLines,
		}
		err := store.Save(index)
		require.NoError(t, err)
	}

	// Verify exists
	paths, err := store.ListCached()
	require.NoError(t, err)
	assert.Len(t, paths, 3)

	// Clear all
	err = store.ClearAll()
	require.NoError(t, err)

	// Verify empty
	paths, err = store.ListCached()
	require.NoError(t, err)
	assert.Empty(t, paths)
}
