package index

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wlame/rx-go/pkg/models"
)

// Store handles persistence of file indexes
type Store struct {
	cacheDir string
}

// NewStore creates a new index store
func NewStore(cacheDir string) *Store {
	return &Store{
		cacheDir: cacheDir,
	}
}

// Save persists an index to disk
func (s *Store) Save(index *models.FileIndex) error {
	// Ensure cache directory exists
	if err := os.MkdirAll(s.cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Generate cache key from file path
	cacheKey := s.generateCacheKey(index.SourcePath)
	cachePath := filepath.Join(s.cacheDir, cacheKey+".json")

	// Serialize to JSON
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal index: %w", err)
	}

	// Write to file
	if err := os.WriteFile(cachePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write index file: %w", err)
	}

	return nil
}

// Load retrieves an index from disk
func (s *Store) Load(filePath string) (*models.FileIndex, error) {
	// Generate cache key
	cacheKey := s.generateCacheKey(filePath)
	cachePath := filepath.Join(s.cacheDir, cacheKey+".json")

	// Check if cache file exists
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("index not found in cache")
	}

	// Read cache file
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read index file: %w", err)
	}

	// Deserialize
	var index models.FileIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to unmarshal index: %w", err)
	}

	// Validate cache freshness
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat source file: %w", err)
	}

	// Check if source file has been modified since index was created
	sourceModTime, err := time.Parse(time.RFC3339, index.SourceModifiedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid source_modified_at in index: %w", err)
	}

	if fileInfo.ModTime().After(sourceModTime) {
		return nil, fmt.Errorf("index is stale (source file modified)")
	}

	// Check if file size matches
	if fileInfo.Size() != index.SourceSizeBytes {
		return nil, fmt.Errorf("index is invalid (source file size mismatch)")
	}

	return &index, nil
}

// Delete removes an index from the cache
func (s *Store) Delete(filePath string) error {
	cacheKey := s.generateCacheKey(filePath)
	cachePath := filepath.Join(s.cacheDir, cacheKey+".json")

	if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete index file: %w", err)
	}

	return nil
}

// Exists checks if an index exists in the cache
func (s *Store) Exists(filePath string) bool {
	cacheKey := s.generateCacheKey(filePath)
	cachePath := filepath.Join(s.cacheDir, cacheKey+".json")

	_, err := os.Stat(cachePath)
	return err == nil
}

// generateCacheKey creates a cache key from a file path
func (s *Store) generateCacheKey(filePath string) string {
	// Use absolute path for consistency
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	// SHA256 hash of absolute path
	hash := sha256.Sum256([]byte(absPath))
	return fmt.Sprintf("%x", hash)
}

// ListCached returns a list of all cached index file paths
func (s *Store) ListCached() ([]string, error) {
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read cache directory: %w", err)
	}

	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		// Read index to get source path
		cachePath := filepath.Join(s.cacheDir, entry.Name())
		data, err := os.ReadFile(cachePath)
		if err != nil {
			continue // Skip unreadable files
		}

		var index models.FileIndex
		if err := json.Unmarshal(data, &index); err != nil {
			continue // Skip invalid indexes
		}

		paths = append(paths, index.SourcePath)
	}

	return paths, nil
}

// ClearAll removes all cached indexes
func (s *Store) ClearAll() error {
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to clear
		}
		return fmt.Errorf("failed to read cache directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		cachePath := filepath.Join(s.cacheDir, entry.Name())
		if err := os.Remove(cachePath); err != nil {
			return fmt.Errorf("failed to delete %s: %w", entry.Name(), err)
		}
	}

	return nil
}
