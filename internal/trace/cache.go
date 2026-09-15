package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
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
// changes in a backward-incompatible way, or when the meaning of a
// field changes. A cache of any other version is treated as absent by
// LoadCache, so the bump is what keeps an old cache from being read
// back with today's rules.
//
// Version 3: line_number is the line's number in the file rather than
// in the chunk that found it.
//
// Version 4: the cache records the source's inode, ctime and
// fingerprint, and every identity field describes the file as it was
// when the scan was planned. It also records the scan's chunk count,
// so a cache hit reports the file_chunks the scan reported. Version 3 caches were stamped from a stat
// taken after the scan, so a log that grew during the scan got a cache
// claiming its new size with matches only up to the old one; such a
// cache cannot be told apart from a good one and is discarded.
//
// Version 5: a full scan of a seekable-zstd file numbers every match.
// A frame that holds no line break broke the frame-by-frame count in
// version 4, and the matches after it were stored with their line
// number inside their own frame. Every match is also stored at the
// offset of its whole line: version 4 scanned a file whose frames cut
// lines frame by frame, so a match could be stored at the offset of a
// line fragment, and a match a frame boundary cut in two was missing.
//
// rx-python writes version 3, so each backend treats the other's trace
// caches as absent.
const TraceCacheVersion = 5

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
// cross-load. Python uses `json.dumps(..., sort_keys=True)`, which
// emits the keys in alphabetical order with ", " and ": " as
// separators. Go's encoding/json.Marshal writes no spaces, so the JSON
// is built by hand: "flags" before "patterns", and the same separators.
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

// appendJSONStringArray writes a JSON array of strings to buf in the
// layout Python's default `json.dumps` produces: `["a", "b"]`, with one
// space after each comma. Python's default separators are (", ", ": "),
// so `json.dumps(["a", "b"])` gives '["a", "b"]'; the patterns hash
// depends on matching it byte for byte.
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
	data, err := os.ReadFile(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrCacheMiss
		}
		return nil, fmt.Errorf("LoadCache: read %s: %w", cachePath, err)
	}
	// A cache file was read, so its read-and-parse time is reported
	// whatever the parse finds. A missing file is not a load.
	defer func() { prometheus.RecordTraceCacheLoadDuration(time.Since(start)) }()
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
// temporary file of this call's own alongside and renamed into place
// (writeFileAtomically), so a concurrent reader never sees a partial
// write and two concurrent writers never mix their bytes.
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
	if err := writeFileAtomically(cachePath, body); err != nil {
		return fmt.Errorf("SaveCache: %w", err)
	}
	// gated helper — CLI mode skips collection.
	prometheus.IncTraceCacheWrites()
	return nil
}

// writeFileAtomically puts body at path through a temporary file of its
// own in the same directory, renamed into place. os.CreateTemp gives
// every call a unique name, so two traces that write the same entry at
// once never share a temporary file: each rename installs one writer's
// whole body, and the last rename wins. A rename within one directory
// is atomic, so a reader sees the old entry or a new one, never a part.
//
// os.CreateTemp creates the file with mode 0o600, which suits cache
// JSON that may hold matched line text. On any error the temporary file
// is removed.
func writeFileAtomically(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	// After a successful rename the temporary name no longer exists and
	// this Remove fails harmlessly; after any failure it cleans up.
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close() // the write error is the one worth reporting
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp.Name(), path, err)
	}
	return nil
}

// ============================================================================
// Validity checks
// ============================================================================

// IsCacheValid returns true when the cache file exists, the version
// and the patterns hash match, the cache records the scan's chunk
// count, and the source file is still the file
// the cache was built from: the same size, mtime, inode, ctime and
// fingerprint, compared by index.SourceIdentity.MatchesFile exactly as
// the line index compares them.
func IsCacheValid(
	cachePath string,
	sourcePath string,
	patterns, rgFlags []string,
) bool {
	return loadValidCache(cachePath, sourcePath, patterns, rgFlags) != nil
}

// loadValidCache reads the cache at cachePath and returns it when
// IsCacheValid accepts it, nil otherwise. One read serves both the
// check and the caller, so a cache hit loads the file once.
func loadValidCache(
	cachePath string,
	sourcePath string,
	patterns, rgFlags []string,
) *rxtypes.TraceCacheData {
	data, err := LoadCache(cachePath)
	if err != nil {
		// A missing entry or one of another version is an ordinary
		// miss. Anything else (truncated JSON, a permission error) is
		// treated as a miss too, so the trace scans and a complete scan
		// replaces the entry, but the operator hears about it.
		if !errors.Is(err, ErrCacheMiss) {
			slog.Default().Warn("trace_cache_unreadable",
				"path", cachePath,
				"error", err.Error(),
			)
		}
		return nil
	}
	if data.PatternsHash != ComputePatternsHash(patterns, rgFlags) {
		return nil
	}
	// Every scan has at least one chunk. A cache without the count
	// cannot report the scan's file_chunks, so it is not used.
	if data.ChunkCount < 1 {
		return nil
	}
	if !recordedSource(data).MatchesFile(sourcePath) {
		return nil
	}
	return data
}

// lookupCache is loadValidCache counted as one trace cache lookup: a
// hit when it returns a cache, a miss otherwise.
func lookupCache(sourcePath string, patterns, rgFlags []string) *rxtypes.TraceCacheData {
	data := loadValidCache(CachePath(sourcePath, patterns, rgFlags), sourcePath, patterns, rgFlags)
	// gated helpers — no-op in CLI mode.
	if data == nil {
		prometheus.IncTraceCacheMisses()
	} else {
		prometheus.IncTraceCacheHits()
	}
	return data
}

// recordedSource gathers the identity fields a trace cache carries.
func recordedSource(data *rxtypes.TraceCacheData) index.SourceIdentity {
	return index.SourceIdentity{
		SizeBytes:   data.SourceSizeBytes,
		ModifiedAt:  data.SourceModifiedAt,
		Inode:       data.SourceInode,
		ChangedAt:   data.SourceChangedAt,
		Fingerprint: data.SourceFingerprint,
	}
}

// GetCachedScan returns the cached scan for (source, patterns, flags)
// when the cache is valid, or ErrCacheMiss otherwise. The caller
// reconstructs the answer from its matches and reports its chunk count.
func GetCachedScan(
	sourcePath string,
	patterns, rgFlags []string,
) (*rxtypes.TraceCacheData, error) {
	data := lookupCache(sourcePath, patterns, rgFlags)
	if data == nil {
		return nil, ErrCacheMiss
	}
	return data, nil
}

// CompressedCacheInfo is the equivalent of Python's
// get_compressed_cache_info return value — the cache's
// compression_format + frames_with_matches alongside the matches,
// for use by the seekable-zstd fast path.
type CompressedCacheInfo struct {
	CompressionFormat string
	FramesWithMatches []int
	Matches           []rxtypes.TraceCacheMatch
	// ChunkCount is the frame count the scan that wrote the cache
	// reported as file_chunks.
	ChunkCount int
}

// GetCompressedCacheInfo returns cache info for a compressed file, or
// ErrCacheMiss if the cache is invalid. Callers should treat a miss as
// "no fast path available — decompress and re-scan".
func GetCompressedCacheInfo(
	sourcePath string,
	patterns, rgFlags []string,
) (*CompressedCacheInfo, error) {
	data := lookupCache(sourcePath, patterns, rgFlags)
	if data == nil {
		return nil, ErrCacheMiss
	}
	return &CompressedCacheInfo{
		CompressionFormat: data.CompressionFormat,
		FramesWithMatches: append([]int(nil), data.FramesWithMatches...),
		Matches:           data.Matches,
		ChunkCount:        data.ChunkCount,
	}, nil
}

// ============================================================================
// Cache construction
// ============================================================================

// ScannedFile is what one completed scan of a file contributes to its
// trace cache.
type ScannedFile struct {
	// Path is the file as the caller named it.
	Path string
	// Source is the file's identity taken before the scan was planned.
	// The cache is stamped with it, so the cache describes exactly the
	// bytes the scan covered.
	Source index.SourceIdentity
	// Matches are the scan's matches with pattern IDs already resolved
	// to a single pattern each (post-identify): the cache stores one
	// match per (pattern, offset) combination.
	Matches []rxtypes.Match
	// FrameIndexByOffset maps a match offset to its frame, for a
	// seekable-zstd source; nil otherwise.
	FrameIndexByOffset map[int64]int
	// CompressionFormat is "zstd-seekable" for a seekable-zstd source
	// and empty for a plain file.
	CompressionFormat string
	// Chunks is the file_chunks value the scan reported: chunks of a
	// plain file, frames of a seekable-zstd one.
	Chunks int
}

// BuildCache converts a scan's output into the on-disk cache shape. For
// a seekable-zstd source the matches that have a frame index recorded
// produce frames_with_matches, which enables the fast-path
// reconstruction on later cache hits.
func BuildCache(scan ScannedFile, patterns, rgFlags []string) *rxtypes.TraceCacheData {
	abs, err := filepath.Abs(scan.Path)
	if err != nil {
		abs = scan.Path
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

	cachedMatches := make([]rxtypes.TraceCacheMatch, 0, len(scan.Matches))
	framesSet := map[int]struct{}{}
	for _, m := range scan.Matches {
		var lineNum int64
		if m.RelativeLineNumber != nil {
			lineNum = int64(*m.RelativeLineNumber)
		}
		cm := rxtypes.TraceCacheMatch{
			PatternIndex: patternIndex(m.Pattern),
			Offset:       m.Offset,
			LineNumber:   lineNum,
		}
		if fi, ok := scan.FrameIndexByOffset[m.Offset]; ok {
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
		Version:           TraceCacheVersion,
		SourcePath:        abs,
		SourceModifiedAt:  scan.Source.ModifiedAt,
		SourceSizeBytes:   scan.Source.SizeBytes,
		SourceInode:       scan.Source.Inode,
		SourceChangedAt:   scan.Source.ChangedAt,
		SourceFingerprint: scan.Source.Fingerprint,
		Patterns:          append([]string(nil), patterns...),
		PatternsHash:      ComputePatternsHash(patterns, rgFlags),
		RgFlags:           relevantFlags,
		CreatedAt:         index.FormatMtime(time.Now()),
		ChunkCount:        scan.Chunks,
		Matches:           cachedMatches,
	}

	if scan.CompressionFormat != "" {
		out.CompressionFormat = scan.CompressionFormat
		if scan.CompressionFormat == "zstd-seekable" && len(framesSet) > 0 {
			frames := make([]int, 0, len(framesSet))
			for fi := range framesSet {
				frames = append(frames, fi)
			}
			sort.Ints(frames)
			out.FramesWithMatches = frames
		}
	}

	return out
}

// cacheWriteWarned is set once a failed trace cache write has been
// logged, so a full disk produces one warning rather than one per file.
var cacheWriteWarned atomic.Bool

// SaveScannedFile writes the trace cache for one completed scan, unless
// the file on disk is no longer the file the scan was planned on.
//
// The scan read the bytes that were there when its chunks were planned.
// If the file changed since (a live log grew, or the file was
// replaced), a cache written now would describe a file the scan never
// read, so nothing is written and the next trace scans again. The cache
// is stamped with the planned identity in any case, so even a change
// that lands after this check makes the next read see a mismatch.
//
// Errors are not returned: a cache that cannot be written only means
// the next trace scans again. The first failure in a process is logged
// as a warning, because one that repeats (a full disk, a cache
// directory that cannot be created) silently turns the cache off.
func SaveScannedFile(scan ScannedFile, patterns, rgFlags []string) {
	if !scan.Source.MatchesFile(scan.Path) {
		return
	}
	err := SaveCache(CachePath(scan.Path, patterns, rgFlags), BuildCache(scan, patterns, rgFlags))
	if err != nil && cacheWriteWarned.CompareAndSwap(false, true) {
		// CompareAndSwap lets exactly one goroutine through, however
		// many scans fail at the same moment.
		slog.Default().Warn("trace_cache_write_failed",
			"path", scan.Path, "error", err.Error(),
			"note", "later failures are not logged; traces still work, uncached")
	}
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
