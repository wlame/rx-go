// Package cache implements trace result caching keyed by pattern + file metadata.
//
// The cache stores search results for large files so subsequent searches with the
// same patterns can return immediately without re-running ripgrep. Cache keys are
// computed identically to the Python implementation for cross-language compatibility.
//
// Cache layout:
//
//	{cacheDir}/trace_cache/{patternsHash}/{pathHash}_{filename}.json
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	json "github.com/goccy/go-json"

	"github.com/wlame/rx/internal/models"
)

// TraceCacheVersion is the cache format version. Must match Python's TRACE_CACHE_VERSION.
const TraceCacheVersion = 2

// matchingFlags are the rg flags that affect pattern matching semantics and therefore
// must be part of the cache key. Other flags (like -A, -B for context) don't change
// which lines match, so they are excluded.
var matchingFlags = map[string]bool{
	"-i":               true,
	"-w":               true,
	"-x":               true,
	"-F":               true,
	"-P":               true,
	"--case-sensitive": true,
	"--ignore-case":    true,
}

// traceCacheEntry is the on-disk JSON format for cached trace results.
// Field names match the Python implementation for cross-language cache sharing.
type traceCacheEntry struct {
	Version          int                    `json:"version"`
	SourcePath       string                 `json:"source_path"`
	SourceModifiedAt string                 `json:"source_modified_at"`
	SourceSizeBytes  int64                  `json:"source_size_bytes"`
	Patterns         []string               `json:"patterns"`
	PatternsHash     string                 `json:"patterns_hash"`
	RgFlags          []string               `json:"rg_flags"`
	CreatedAt        string                 `json:"created_at"`
	Matches          []models.Match         `json:"matches"`
	Response         *models.TraceResponse  `json:"response,omitempty"`
}

// PatternsHash computes a deterministic hash of patterns and matching-relevant rg flags.
//
// Algorithm (must match Python exactly for cache compatibility):
//  1. Sort the patterns list.
//  2. Extract and sort only flags that affect matching semantics.
//  3. Build JSON: {"patterns": [...], "flags": [...]}
//  4. Return SHA256(json)[:16].
func PatternsHash(patterns []string, rgFlags []string) string {
	sortedPatterns := make([]string, len(patterns))
	copy(sortedPatterns, patterns)
	sort.Strings(sortedPatterns)

	var relevantFlags []string
	for _, f := range rgFlags {
		if matchingFlags[f] {
			relevantFlags = append(relevantFlags, f)
		}
	}
	sort.Strings(relevantFlags)

	// Build the JSON hash input identically to Python:
	//   json.dumps({"patterns": sorted_patterns, "flags": relevant_flags}, sort_keys=True)
	// With sort_keys, "flags" comes before "patterns" alphabetically.
	hashInput := buildHashJSON(sortedPatterns, relevantFlags)

	h := sha256.Sum256([]byte(hashInput))
	return hex.EncodeToString(h[:])[:16]
}

// buildHashJSON produces the exact same JSON string as Python's json.dumps with sort_keys=True.
// Keys are sorted alphabetically: "flags" before "patterns".
func buildHashJSON(patterns, flags []string) string {
	// Ensure nil slices serialize as [] not null.
	if patterns == nil {
		patterns = []string{}
	}
	if flags == nil {
		flags = []string{}
	}

	m := map[string][]string{
		"flags":    flags,
		"patterns": patterns,
	}

	// Use standard encoding/json for deterministic output matching Python.
	// goccy/go-json may differ in whitespace; Python's json.dumps uses
	// compact format with sorted keys by default.
	data, err := json.Marshal(m)
	if err != nil {
		// Should never happen with string slices.
		return "{}"
	}
	return string(data)
}

// TraceCachePath computes the cache file path for a source file + pattern combination.
//
// Format: {cacheDir}/trace_cache/{patternsHash}/{pathHash}_{filename}.json
func TraceCachePath(cacheDir string, patterns []string, rgFlags []string, filePath string) string {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	pHash := PatternsHash(patterns, rgFlags)
	pathHash := hashString(absPath)
	filename := filepath.Base(absPath)

	return filepath.Join(cacheDir, "trace_cache", pHash, fmt.Sprintf("%s_%s.json", pathHash, filename))
}

// Store writes a TraceResponse to the cache for later retrieval.
// Creates parent directories as needed. Returns an error on write failure.
func Store(
	cacheDir string,
	patterns []string,
	rgFlags []string,
	path string,
	result *models.TraceResponse,
) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path %s: %w", path, err)
	}

	stat, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", absPath, err)
	}

	entry := traceCacheEntry{
		Version:          TraceCacheVersion,
		SourcePath:       absPath,
		SourceModifiedAt: stat.ModTime().Format(time.RFC3339Nano),
		SourceSizeBytes:  stat.Size(),
		Patterns:         patterns,
		PatternsHash:     PatternsHash(patterns, rgFlags),
		RgFlags:          extractMatchingFlags(rgFlags),
		CreatedAt:        time.Now().Format(time.RFC3339Nano),
		Matches:          result.Matches,
		Response:         result,
	}

	cachePath := TraceCachePath(cacheDir, patterns, rgFlags, absPath)
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache dir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache entry: %w", err)
	}

	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		return fmt.Errorf("write cache %s: %w", cachePath, err)
	}

	slog.Debug("trace cache stored", "path", cachePath, "matches", len(result.Matches))
	return nil
}

// Load reads a cached TraceResponse for the given patterns and file.
// Returns (response, true, nil) on cache hit, (nil, false, nil) on cache miss,
// or (nil, false, err) on unexpected errors.
//
// The cache is automatically invalidated if the source file's mtime or size
// has changed since the result was cached.
func Load(
	cacheDir string,
	patterns []string,
	rgFlags []string,
	path string,
) (*models.TraceResponse, bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, false, nil
	}

	cachePath := TraceCachePath(cacheDir, patterns, rgFlags, absPath)

	data, err := os.ReadFile(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read cache %s: %w", cachePath, err)
	}

	var entry traceCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		slog.Debug("cache unmarshal failed, treating as miss", "path", cachePath, "error", err)
		return nil, false, nil
	}

	// Version check.
	if entry.Version != TraceCacheVersion {
		slog.Debug("cache version mismatch", "cached", entry.Version, "expected", TraceCacheVersion)
		return nil, false, nil
	}

	// Validate that the source file hasn't changed.
	stat, err := os.Stat(absPath)
	if err != nil {
		return nil, false, nil
	}

	currentMtime := stat.ModTime().Format(time.RFC3339Nano)
	if entry.SourceModifiedAt != currentMtime || entry.SourceSizeBytes != stat.Size() {
		slog.Debug("cache invalidated, file changed",
			"path", absPath,
			"cached_mtime", entry.SourceModifiedAt,
			"current_mtime", currentMtime)
		return nil, false, nil
	}

	// Validate patterns hash.
	expectedHash := PatternsHash(patterns, rgFlags)
	if entry.PatternsHash != expectedHash {
		slog.Debug("cache patterns hash mismatch", "path", absPath)
		return nil, false, nil
	}

	if entry.Response != nil {
		slog.Debug("trace cache hit", "path", absPath, "matches", len(entry.Response.Matches))
		return entry.Response, true, nil
	}

	// Fallback: reconstruct a minimal response from cached matches.
	resp := models.NewTraceResponse("", []string{absPath})
	resp.Matches = entry.Matches
	slog.Debug("trace cache hit (reconstructed)", "path", absPath, "matches", len(entry.Matches))
	return &resp, true, nil
}

// extractMatchingFlags filters rg flags to only those that affect matching semantics.
func extractMatchingFlags(rgFlags []string) []string {
	var result []string
	for _, f := range rgFlags {
		if matchingFlags[f] {
			result = append(result, f)
		}
	}
	sort.Strings(result)
	return result
}

// hashString computes SHA256(s)[:16] — the first 16 hex characters.
func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}
