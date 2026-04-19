// Package index implements unified file indexes for the rx-go trace
// engine. It handles on-disk persistence, cache-validity checking, and
// (in later milestones) index building.
//
// At M2 the module ships with:
//   - Cache path scheme matching Python's unified_index.py exactly.
//   - Save + Load using pkg/rxtypes.UnifiedFileIndex as the wire type.
//   - IsValidForSource: mtime + size mtime-based validation per user
//     decision 6.9.1 — the index is invalidated when source.mtime
//     exceeds the recorded mtime OR sizes differ.
//
// The index builder itself (Build()) is a stub at M2; it's fleshed out
// in M3 when the trace engine comes online.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Version is the schema version of UnifiedFileIndex. Bump when the
// on-disk JSON changes shape in a way that old readers can't handle.
const Version = 2

// Python's isoformat() produces "2006-01-02T15:04:05.123456" in local
// time (NOT UTC). rx-python reads file mtime via datetime.fromtimestamp
// which is local-tz. To stay compatible, the Go port emits the SAME
// timestamp Python would have emitted for a given mtime.
//
// Python format: YYYY-MM-DDTHH:MM:SS.ffffff (local time, no suffix)
// WHEN microseconds are non-zero. When microseconds are zero, Python's
// datetime.isoformat() drops the fractional-second suffix entirely and
// emits YYYY-MM-DDTHH:MM:SS with no trailing ".000000".
//
// This asymmetry matters for cache parity: on filesystems with
// whole-second mtime precision (tmpfs+relatime, FAT32, many network
// mounts, any file touched by `touch --date=... no microseconds`),
// Python writes "2024-01-01T10:00:00" and Go — before this fix — wrote
// "2024-01-01T10:00:00.000000". The two strings don't compare equal so
// the cache cross-invalidates on every re-open. See Stage 8 Blocker.
//
// The two layouts below encode the two output shapes.
const (
	// mtimeLayoutSeconds is Python's isoformat() output when
	// microseconds == 0. No fractional suffix.
	mtimeLayoutSeconds = "2006-01-02T15:04:05"
	// mtimeLayoutMicros is Python's isoformat() output when
	// microseconds > 0. Six-digit fractional suffix, zero-padded.
	mtimeLayoutMicros = "2006-01-02T15:04:05.000000"
)

// ErrIndexNotFound is returned by Load when the cache file doesn't
// exist. Distinct from other errors so callers can decide whether to
// build from scratch.
var ErrIndexNotFound = errors.New("index not found in cache")

// GetCachePath returns the cache path for the given source file.
//
// Scheme: <base>/indexes/<safe_basename>_<hash16>.json where
//
//	safe_basename = basename with non-[A-Za-z0-9._-] → '_'
//	hash16        = sha256(abs_path)[:16] in hex
//
// Uses filepath.Clean but NOT EvalSymlinks — Python hashes the abs
// path as provided (os.path.abspath), not the resolved form. Matching
// Python means the SAME cache file is shared between two sessions
// that pass the same path string.
func GetCachePath(sourcePath string) string {
	abs, _ := filepath.Abs(sourcePath)
	return filepath.Join(config.GetIndexCacheDir(), cacheFilename(abs))
}

// cacheFilename builds the "<safe>_<hash>.json" component.
func cacheFilename(absPath string) string {
	sum := sha256.Sum256([]byte(absPath))
	hash16 := hex.EncodeToString(sum[:8]) // 16 hex chars
	safe := safeBasename(filepath.Base(absPath))
	return fmt.Sprintf("%s_%s.json", safe, hash16)
}

// safeBasename replicates Python's sanitization:
//
//	''.join(c if c.isalnum() or c in '._-' else '_' for c in basename)
//
// c.isalnum() in Python recognizes unicode letters and digits; Go's
// unicode.IsLetter / unicode.IsDigit do the same. We pass bytes through
// unicode.IsLetter via rune iteration.
func safeBasename(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Save writes idx to its canonical cache path. Creates the parent
// directory with 0755 mode if missing. Uses an atomic temp-file +
// rename to avoid readers observing a partial write.
//
// Returns the path the index was written to.
func Save(idx *rxtypes.UnifiedFileIndex) (string, error) {
	cachePath := GetCachePath(idx.SourcePath)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o750); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	// Marshal to JSON.
	// json.MarshalIndent would match Python's json.dumps(indent=2);
	// but Python's cache writer uses json.dump(f, sort_keys=False)
	// with no indent — a compact representation. We match that.
	data, err := json.Marshal(idx)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	// Atomic write: temp file in same dir, then rename.
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	defer func() {
		// Best-effort cleanup if the rename failed.
		_ = os.Remove(tmp.Name())
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // error irrelevant; we're about to unlink anyway
		return "", fmt.Errorf("write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp.Name(), cachePath); err != nil {
		return "", fmt.Errorf("rename to %s: %w", cachePath, err)
	}
	return cachePath, nil
}

// Load reads and parses the cache file for sourcePath. Returns
// ErrIndexNotFound if the cache file doesn't exist (the caller typically
// reacts by building a fresh index).
func Load(sourcePath string) (*rxtypes.UnifiedFileIndex, error) {
	cachePath := GetCachePath(sourcePath)
	return LoadFromPath(cachePath)
}

// LoadFromPath reads the file at cachePath. Useful for tests that want
// to hand-place a cache file at a known location.
func LoadFromPath(cachePath string) (*rxtypes.UnifiedFileIndex, error) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrIndexNotFound
		}
		return nil, fmt.Errorf("read %s: %w", cachePath, err)
	}
	var idx rxtypes.UnifiedFileIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", cachePath, err)
	}
	return &idx, nil
}

// IsValidForSource reports whether idx is a faithful description of
// sourcePath's current state on disk. Returns false (no error) on
// stat failures — a missing/unreadable source is treated as "index
// is stale".
//
// Per user decision 6.9.1: invalidation is mtime-based, no TTL, no
// size-cap. The field SourceModifiedAt carries the Python-style ISO
// timestamp; we compare it byte-for-byte with the current mtime
// formatted the same way. Size check is exact.
func IsValidForSource(idx *rxtypes.UnifiedFileIndex, sourcePath string) bool {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return false
	}
	if info.Size() != idx.SourceSizeBytes {
		return false
	}
	current := formatMtime(info.ModTime())
	return current == idx.SourceModifiedAt
}

// FormatMtime exposes the mtime-to-string conversion so tests (and the
// index builder when it lands in M3) can stamp the same format. Local
// time, NOT UTC — Python parity.
func FormatMtime(t time.Time) string { return formatMtime(t) }

// formatMtime converts t to the Python-compatible ISO layout in LOCAL
// time. Using t.Local() instead of t.UTC() is deliberate: Python's
// datetime.fromtimestamp(os.stat(...).st_mtime) returns a naive local
// datetime, and its isoformat() drops tzinfo — so the stored string
// reflects wall-clock at the host, not UTC.
//
// TIMEZONE-DEPENDENCE CAVEAT (see Stage 8 Reviewer 1 High #2 /
// Finding 3):
//
// Because we use t.Local(), the same mtime produces DIFFERENT output
// strings on hosts with different TZ settings. A Docker container
// defaulting to UTC and a host in Europe/Berlin both faithfully match
// Python's behavior — this is a Python quirk we replicate, not a Go
// bug. However, a cache built in one TZ and re-read in another will
// be treated as stale and rebuilt. Document this for operators;
// see docs/MIGRATION.md.
//
// FRACTIONAL-SECOND PARITY (see Stage 8 Blocker / Finding 1):
//
// Python's datetime.isoformat() omits the ".ffffff" suffix when
// microseconds == 0 and emits it otherwise. We reproduce that
// behavior by branching on t.Nanosecond(). Without this branch,
// whole-second mtimes (common on tmpfs+relatime, FAT32, etc.)
// cause cross-language cache invalidation because our "...00.000000"
// never equals Python's "...00".
func formatMtime(t time.Time) string {
	local := t.Local()
	// Nanosecond() returns 0..999_999_999. If zero, Python's
	// datetime.isoformat() would have dropped the fractional suffix.
	if local.Nanosecond() == 0 {
		return local.Format(mtimeLayoutSeconds)
	}
	return local.Format(mtimeLayoutMicros)
}

// LoadForSource is the one-shot "read cache if valid, else report stale"
// helper most callers want. Returns (nil, ErrIndexNotFound) if the
// cache file is absent. Returns (idx, nil) iff the cache is present
// and valid. Returns (nil, nil) if the cache is present but stale —
// the caller should rebuild.
func LoadForSource(sourcePath string) (*rxtypes.UnifiedFileIndex, error) {
	idx, err := Load(sourcePath)
	if err != nil {
		return nil, err
	}
	if !IsValidForSource(idx, sourcePath) {
		return nil, nil
	}
	return idx, nil
}
