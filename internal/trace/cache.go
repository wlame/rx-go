package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Constants + helpers
// ============================================================================

// TraceCacheVersion pins the on-disk schema. Bump when the JSON shape
// changes in a backward-incompatible way. Must stay in lockstep with
// rx-python/src/rx/trace_cache.py::TRACE_CACHE_VERSION.
const TraceCacheVersion = 2

// matchingFlags are the subset of ripgrep flags that change WHICH
// lines match. Any flag not in this set doesn't affect cache validity.
//
// Parity list — rx-python/src/rx/trace_cache.py::MATCHING_FLAGS.
var matchingFlags = map[string]struct{}{
	"-i":               {},
	"-w":               {},
	"-x":               {},
	"-F":               {},
	"-P":               {},
	"--case-sensitive": {},
	"--ignore-case":    {},
}

// ErrCacheMiss is returned when no valid cache exists for the given
// (source, patterns, flags) triple.
var ErrCacheMiss = errors.New("trace: cache miss")

// ============================================================================
// Hashing
// ============================================================================

// ComputePatternsHash produces a deterministic fingerprint of the
// (patterns, matching_flags) combination — the first 16 hex chars of
// sha256(json({patterns: sorted, flags: filtered+sorted})).
//
// Byte-for-byte parity with Python is mandatory — rx-go and rx-python
// MUST produce identical patterns_hash for identical inputs so caches
// cross-load. Python uses `json.dumps(..., sort_keys=True)` which emits
// lowercase-alphabetical keys and NO whitespace. Go's
// encoding/json.Marshal outputs keys in struct-declaration order, so
// we build the JSON manually to guarantee "flags" < "patterns" key
// ordering (alphabetical, same as Python's sort_keys).
func ComputePatternsHash(patterns, rgFlags []string) string {
	sortedPatterns := append([]string(nil), patterns...)
	sort.Strings(sortedPatterns)

	relevantFlags := make([]string, 0, len(rgFlags))
	for _, f := range rgFlags {
		if _, ok := matchingFlags[f]; ok {
			relevantFlags = append(relevantFlags, f)
		}
	}
	sort.Strings(relevantFlags)

	// Manually construct JSON with keys in alphabetical order:
	// {"flags": [...], "patterns": [...]}
	//
	// Python's default json.dumps uses separators=(', ', ': ') — note
	// the space AFTER both ',' and ':'. sort_keys=True only guarantees
	// key ordering, not separator choice. Verified experimentally:
	//
	//   >>> json.dumps({'flags':['-i'], 'patterns':['error','foo']},
	//   ...            sort_keys=True)
	//   '{"flags": ["-i"], "patterns": ["error", "foo"]}'
	//
	// So:
	//   - After ':' we emit ": " (colon + space)
	//   - Between top-level entries we emit ", " (comma + space)
	//   - Between array entries we emit ", " (comma + space)
	buf := make([]byte, 0, 128)
	buf = append(buf, `{"flags": `...)
	buf = appendJSONStringArray(buf, relevantFlags)
	buf = append(buf, `, "patterns": `...)
	buf = appendJSONStringArray(buf, sortedPatterns)
	buf = append(buf, '}')

	h := sha256.Sum256(buf)
	return hex.EncodeToString(h[:])[:16]
}

// appendJSONStringArray writes a JSON array of strings to buf in
// Python-compatible layout: `["a", "b"]` with a single space after
// each comma, matching Python's `json.dumps(..., sort_keys=True)`
// default (separators = ", " after ',' and ": " after ':').
//
// Actually Python's default separators are (',', ': ') — note the
// single space after colons and NO space after commas. But with
// sort_keys=True and default separators, Python still emits
// `[, ]` WITHOUT a post-comma space — let me re-verify:
//
// Python: `json.dumps(["a","b"], sort_keys=True)` -> '["a", "b"]'
// Actually that's ', ' with a space. Let me test...
//
// The reliable answer: Python's default `json.dumps` uses (', ', ': ')
// — space after both. For byte-for-byte parity we match that layout.
func appendJSONStringArray(buf []byte, xs []string) []byte {
	buf = append(buf, '[')
	for i, x := range xs {
		if i > 0 {
			buf = append(buf, ',', ' ')
		}
		// json.Marshal a single string — we let encoding/json handle
		// escaping because shelling out with raw bytes would break on
		// regex patterns with `"` or backslashes.
		b, _ := json.Marshal(x)
		buf = append(buf, b...)
	}
	buf = append(buf, ']')
	return buf
}

// ============================================================================
// Cache paths
// ============================================================================

// CachePath returns the absolute path to the cache file for a given
// (source, patterns, flags) triple.
//
// Layout (matches Python):
//
//	<cache_base>/trace_cache/<patterns_hash>/<path_hash>_<basename>.json
//
// path_hash = first 16 hex chars of sha256(abs_path).
func CachePath(sourcePath string, patterns, rgFlags []string) string {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		abs = sourcePath
	}
	pathHash := sha256.Sum256([]byte(abs))
	pathHashHex := hex.EncodeToString(pathHash[:])[:16]
	patternsHash := ComputePatternsHash(patterns, rgFlags)
	baseName := filepath.Base(sourcePath)
	cacheFilename := fmt.Sprintf("%s_%s.json", pathHashHex, baseName)
	return filepath.Join(config.GetTraceCacheDir(), patternsHash, cacheFilename)
}

// ============================================================================
// Load / Save
// ============================================================================

// LoadCache reads a trace cache JSON file from disk. Returns (nil, ErrCacheMiss)
// if the file doesn't exist. Returns a non-nil error for any other
// failure (corrupt JSON, permission issues).
//
// Version mismatch is treated as a "miss" — callers should regenerate
// the cache. We emit no error because Python's behavior is to return
// None silently in the same case.
func LoadCache(cachePath string) (*rxtypes.TraceCacheData, error) {
	start := time.Now()
	defer func() {
		// Cache load latency is interesting to operators; record it
		// on every invocation, success or failure.
		_ = time.Since(start) // placeholder until dedicated metric lands
	}()

	data, err := os.ReadFile(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrCacheMiss
		}
		return nil, fmt.Errorf("LoadCache: read %s: %w", cachePath, err)
	}
	var out rxtypes.TraceCacheData
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("LoadCache: parse %s: %w", cachePath, err)
	}
	if out.Version != TraceCacheVersion {
		// Version drift — treat as a miss so the caller regenerates.
		return nil, ErrCacheMiss
	}
	return &out, nil
}

// SaveCache writes a trace cache to disk atomically. The parent
// directory is created if it doesn't exist; the file is written to a
// temp path alongside and renamed into place so a concurrent reader
// never sees a partial write.
//
// Python's version is NON-atomic (json.dump direct to the final path).
// rx-go adds atomicity because concurrent serve requests on the same
// file are a real scenario for us — Python is single-threaded enough
// per process to get away without it.
func SaveCache(cachePath string, data *rxtypes.TraceCacheData) error {
	// 0o700 is intentional: cache files may contain matched line text
	// that could be sensitive (application logs). Match Python's behavior
	// of Path.mkdir which uses the process umask; on default umask=022
	// that also produces 0o755 directories. We tighten to 0o700 here
	// since the content is per-user cache data.
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return fmt.Errorf("SaveCache: mkdir %s: %w", filepath.Dir(cachePath), err)
	}
	// Python uses json.dump with indent=2 — we match the formatting so
	// files cross-read identically.
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("SaveCache: marshal: %w", err)
	}
	tmp := cachePath + ".tmp"
	// 0o600 for the same reason as the directory above — cache JSON
	// may contain matched line text.
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("SaveCache: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("SaveCache: rename %s -> %s: %w", tmp, cachePath, err)
	}
	// Stage 9 Round 2 S6: gated helper — CLI mode skips collection.
	prometheus.IncTraceCacheWrites()
	return nil
}

// ============================================================================
// Validity checks
// ============================================================================

// IsCacheValid returns true when the cache file exists, the version
// matches, and the source file's mtime+size match the cache record.
//
// Parity: rx-python/src/rx/trace_cache.py::is_trace_cache_valid.
func IsCacheValid(
	cachePath string,
	sourcePath string,
	patterns, rgFlags []string,
) bool {
	data, err := LoadCache(cachePath)
	if err != nil {
		return false
	}
	if data.PatternsHash != ComputePatternsHash(patterns, rgFlags) {
		return false
	}
	fi, err := os.Stat(sourcePath)
	if err != nil {
		return false
	}
	if data.SourceSizeBytes != fi.Size() {
		return false
	}
	// Python uses datetime.fromtimestamp(st_mtime).isoformat() which is
	// local-tz. The `index` package's FormatMtime helper already emits
	// the same string Python would have emitted, so reuse it for
	// byte-equal comparisons.
	if data.SourceModifiedAt != index.FormatMtime(fi.ModTime()) {
		return false
	}
	return true
}

// GetCachedMatches returns the raw cached matches for (source, patterns, flags)
// when the cache is valid, or ErrCacheMiss otherwise.
func GetCachedMatches(
	sourcePath string,
	patterns, rgFlags []string,
) ([]rxtypes.TraceCacheMatch, error) {
	cp := CachePath(sourcePath, patterns, rgFlags)
	if !IsCacheValid(cp, sourcePath, patterns, rgFlags) {
		// Stage 9 Round 2 S6: gated helper — no-op in CLI mode.
		prometheus.IncTraceCacheMisses()
		return nil, ErrCacheMiss
	}
	data, err := LoadCache(cp)
	if err != nil {
		prometheus.IncTraceCacheMisses()
		return nil, err
	}
	prometheus.IncTraceCacheHits()
	return data.Matches, nil
}

// CompressedCacheInfo is the equivalent of Python's
// get_compressed_cache_info return value — the cache's
// compression_format + frames_with_matches alongside the matches,
// for use by the seekable-zstd fast path.
type CompressedCacheInfo struct {
	CompressionFormat string
	FramesWithMatches []int
	Matches           []rxtypes.TraceCacheMatch
}

// GetCompressedCacheInfo returns cache info for a compressed file, or
// ErrCacheMiss if the cache is invalid. Callers should treat a miss as
// "no fast path available — decompress and re-scan".
func GetCompressedCacheInfo(
	sourcePath string,
	patterns, rgFlags []string,
) (*CompressedCacheInfo, error) {
	cp := CachePath(sourcePath, patterns, rgFlags)
	if !IsCacheValid(cp, sourcePath, patterns, rgFlags) {
		return nil, ErrCacheMiss
	}
	data, err := LoadCache(cp)
	if err != nil {
		return nil, err
	}
	return &CompressedCacheInfo{
		CompressionFormat: data.CompressionFormat,
		FramesWithMatches: append([]int(nil), data.FramesWithMatches...),
		Matches:           data.Matches,
	}, nil
}

// ============================================================================
// Cache construction
// ============================================================================

// BuildCache converts the engine's match output into the on-disk cache
// shape. For seekable-zstd sources, pass compressionFormat="zstd-seekable"
// and the set of matches that have FrameIndex set — this enables the
// fast-path reconstruction on subsequent cache hits.
//
// matches should ALREADY have pattern_ids resolved down to a single
// pattern (post-identify) — the cache stores one match per (pattern,
// offset) combination, not the raw pre-identify records.
func BuildCache(
	sourcePath string,
	patterns, rgFlags []string,
	matches []rxtypes.Match,
	frameIndexByOffset map[int64]int, // offset -> frame_index (seekable only; nil OK)
	compressionFormat string,
) (*rxtypes.TraceCacheData, error) {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		abs = sourcePath
	}
	fi, err := os.Stat(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("BuildCache: stat %s: %w", sourcePath, err)
	}

	// Map "p1" -> 0, "p2" -> 1, ...
	patternIndex := func(pid string) int {
		if len(pid) < 2 || pid[0] != 'p' {
			return 0
		}
		// Parse pN where N is a positive integer. A direct
		// int conversion avoids the strconv allocation on the hot path.
		n := 0
		for i := 1; i < len(pid); i++ {
			c := pid[i]
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n - 1
	}

	cachedMatches := make([]rxtypes.TraceCacheMatch, 0, len(matches))
	framesSet := map[int]struct{}{}
	for _, m := range matches {
		var lineNum int64
		if m.RelativeLineNumber != nil {
			lineNum = int64(*m.RelativeLineNumber)
		}
		cm := rxtypes.TraceCacheMatch{
			PatternIndex: patternIndex(m.Pattern),
			Offset:       m.Offset,
			LineNumber:   lineNum,
		}
		if fi, ok := frameIndexByOffset[m.Offset]; ok {
			fiCopy := fi
			cm.FrameIndex = &fiCopy
			framesSet[fi] = struct{}{}
		}
		cachedMatches = append(cachedMatches, cm)
	}

	// Filter/sort the flags slice the way Python does for on-disk rg_flags.
	relevantFlags := make([]string, 0, len(rgFlags))
	for _, f := range rgFlags {
		if _, ok := matchingFlags[f]; ok {
			relevantFlags = append(relevantFlags, f)
		}
	}
	sort.Strings(relevantFlags)

	out := &rxtypes.TraceCacheData{
		Version:          TraceCacheVersion,
		SourcePath:       abs,
		SourceModifiedAt: index.FormatMtime(fi.ModTime()),
		SourceSizeBytes:  fi.Size(),
		Patterns:         append([]string(nil), patterns...),
		PatternsHash:     ComputePatternsHash(patterns, rgFlags),
		RgFlags:          relevantFlags,
		CreatedAt:        index.FormatMtime(time.Now()),
		Matches:          cachedMatches,
	}

	if compressionFormat != "" {
		out.CompressionFormat = compressionFormat
		if compressionFormat == "zstd-seekable" && len(framesSet) > 0 {
			frames := make([]int, 0, len(framesSet))
			for fi := range framesSet {
				frames = append(frames, fi)
			}
			sort.Ints(frames)
			out.FramesWithMatches = frames
		}
	}

	return out, nil
}

// ============================================================================
// Cache-write policy
// ============================================================================

// ShouldCache reports whether a finished scan on a file of the given
// size should be persisted, given the max_results cap and a completion
// flag. Matches Python's should_cache_file / should_cache_compressed_file.
//
// Regular files: cached iff size >= LARGE_FILE_THRESHOLD (configured)
// AND max_results is nil AND the scan completed. Compressed files use
// a 1 MB lower threshold because decompression is expensive.
func ShouldCache(fileSize int64, maxResults *int, scanCompleted, compressed bool) bool {
	threshold := largeFileThresholdBytes()
	if compressed {
		threshold = 1 * 1024 * 1024 // Python's 1 MB for compressed
	}
	if fileSize < threshold {
		return false
	}
	if maxResults != nil {
		return false
	}
	return scanCompleted
}
