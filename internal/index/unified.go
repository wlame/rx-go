// Package index builds and manages line-offset indexes for fast byte-offset-to-line-number lookups.
//
// Three index types share a unified JSON storage format, all stored under
// $RX_CACHE_DIR/indexes/:
//
//   - Regular text files: sampled [line_number, byte_offset] entries
//   - Compressed files: sampled [line_number, decompressed_byte_offset] entries
//   - Seekable zstd: frame-to-line mapping with [line_number, decompressed_offset, frame_index]
//
// Cache invalidation is based on source file mtime + size matching.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	json "github.com/goccy/go-json"

	"github.com/wlame/rx/internal/models"
)

// UnifiedIndexVersion is the cache format version — increment when format changes.
// Must match Python's UNIFIED_INDEX_VERSION for cache sharing.
const UnifiedIndexVersion = 2

// Load reads a FileIndex from a JSON cache file on disk.
// Returns nil and an error if the file doesn't exist, is corrupt, or has a version mismatch.
func Load(cachePath string) (*models.FileIndex, error) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, fmt.Errorf("read index cache %s: %w", cachePath, err)
	}

	var idx models.FileIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("unmarshal index cache %s: %w", cachePath, err)
	}

	// Reject version mismatches — the format may have changed.
	if idx.Version != UnifiedIndexVersion {
		return nil, fmt.Errorf("index version mismatch: got %d, want %d", idx.Version, UnifiedIndexVersion)
	}

	return &idx, nil
}

// Save writes a FileIndex to a JSON cache file, creating parent directories as needed.
func Save(cachePath string, index *models.FileIndex) error {
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create index cache dir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}

	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		return fmt.Errorf("write index cache %s: %w", cachePath, err)
	}

	slog.Debug("index cache saved", "path", cachePath)
	return nil
}

// Validate checks whether a cached index is still valid for the source file.
// The index is valid when both the modification time (ISO 8601) and file size
// still match the actual file on disk.
func Validate(index *models.FileIndex, filePath string) bool {
	stat, err := os.Stat(filePath)
	if err != nil {
		return false
	}

	currentMtime := stat.ModTime().Format(time.RFC3339Nano)
	return index.SourceModifiedAt == currentMtime && index.SourceSizeBytes == int(stat.Size())
}

// Invalidate removes a stale index cache file from disk.
// Returns nil if the file was deleted or didn't exist.
func Invalidate(cachePath string) error {
	err := os.Remove(cachePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove stale index %s: %w", cachePath, err)
	}
	slog.Debug("index cache invalidated", "path", cachePath)
	return nil
}

// IndexCachePath computes the cache file path for a source file's index.
//
// Format: {cacheDir}/indexes/{safe_filename}_{SHA256(abspath)[:16]}.json
//
// This matches the Python implementation's get_index_path for cross-language
// cache compatibility.
func IndexCachePath(cacheDir, filePath string) string {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	pathHash := hashPath(absPath)
	safeName := SafeFilename(filepath.Base(absPath))

	return filepath.Join(cacheDir, "indexes", fmt.Sprintf("%s_%s.json", safeName, pathHash))
}

// SafeFilename sanitizes a path's basename for safe use as a filename component.
// Non-alphanumeric characters are replaced with '_', except '.', '-', and '_' which
// are preserved. This matches the Python implementation's sanitization logic.
func SafeFilename(name string) string {
	result := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAlphanumeric(c) || c == '.' || c == '-' || c == '_' {
			result[i] = c
		} else {
			result[i] = '_'
		}
	}
	return string(result)
}

// hashPath computes SHA256(absPath)[:16] — the first 16 hex characters.
// This is the same hashing scheme used by the Python implementation.
func hashPath(absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return hex.EncodeToString(h[:])[:16]
}

// isAlphanumeric returns true for ASCII letters and digits.
func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
