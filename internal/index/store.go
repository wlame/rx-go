// Package index implements unified file indexes for the rx-go trace
// engine: index building, on-disk persistence, and cache-validity
// checking.
//
// The package provides:
//   - Build (builder.go), which walks a source file and produces its
//     UnifiedFileIndex.
//   - Cache path scheme matching Python's unified_index.py exactly.
//   - Save + Load using pkg/rxtypes.UnifiedFileIndex as the wire type.
//   - IsValidForSource: the index is invalidated when the source's
//     size or mtime differs from the recorded values, or its inode,
//     device, ctime or content fingerprint no longer match.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Version is the schema version of UnifiedFileIndex. Bump when the
// on-disk JSON changes shape in a way that old readers can't handle.
//
// Version 3: checkpoints in a compressed file's index name the line that
// starts at the recorded offset. rx-python's version 2 named the line
// before it, so every lookup in such an index landed one line late.
//
// Version 4: the index records the source inode and ctime, so a file
// rewritten with the same size and mtime no longer looks unchanged.
//
// Version 5: an analyzed index records the window and the detector set
// it was analyzed with, so a cached analysis is reused only for a
// request that would produce the same one.
//
// Version 6: in a seekable-zstd index, a frame that holds no line break
// (inside a line longer than a frame, or empty) holds zero lines, and
// the next frame starts on the same line. Version 5 counted it as
// holding one, which numbered every later frame and checkpoint one line
// too high for each such frame.
//
// Version 7: the index records the mtime and the ctime as nanoseconds
// since the Unix epoch (source_mtime_ns, source_ctime_ns) and the
// device beside the inode (source_device), and validation compares
// those. Version 6 compared the local-time text of the two times, so
// an index built under one TZ was stale under another, and an mtime
// moved by an hour inside the hour a daylight-saving change repeats
// went unseen.
//
// Version 8: every index records a time section (time_index): the
// timestamp format of the file's lines, the first and last timestamped
// line, how often the timestamps step back, and for each checkpoint the
// latest timestamp before it (max_before), which a search by time uses
// to skip to the right checkpoint. It is null for a file with no
// timestamp format. rx-python writes no such field.
//
// An index stamped with any other version is refused by LoadFromPath.
// That refusal is the point of the constant: before it existed, a
// version 2 index was read with version 3 rules and answered one line
// off. rx-python's UNIFIED_INDEX_VERSION is still 4, so each backend
// treats the other's indexes as absent and builds its own.
const Version = 8

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
// the cache cross-invalidates on every re-open..
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

// ErrIndexNotFound is returned (or wrapped) by Load when there is no
// usable index: the cache file does not exist, is of another format
// version, or cannot be read or parsed. Callers branch on it with
// errors.Is and build from scratch or answer without an index.
var ErrIndexNotFound = errors.New("index not found in cache")

// ErrIndexUnreadable is wrapped, beside ErrIndexNotFound, by the error
// for an index file that exists but cannot be read or parsed. A caller
// that only needs "is there a usable index" checks ErrIndexNotFound; one
// that should replace a damaged file checks this one.
var ErrIndexUnreadable = errors.New("index file cannot be read")

// GetCachePath returns the cache path for the given source file.
//
// Scheme: <base>/indexes/<safe_basename>_<hash16>.json where
//
//	safe_basename = basename with non-[A-Za-z0-9._-] → '_', cut to
//	                233 bytes so the whole name fits in 255
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

// cacheFilename builds the "<safe>_<hash>.json" component. The safe
// base name is cut so the component is at most MaxCacheFileNameBytes
// long: a longer name cannot be created, and every build of the file
// would be thrown away. The hash is of the whole path, so two names
// that differ only after the cut still get two files.
func cacheFilename(absPath string) string {
	sum := sha256.Sum256([]byte(absPath))
	hash16 := hex.EncodeToString(sum[:8]) // 16 hex chars
	safe := TrimCacheNamePart(safeBasename(filepath.Base(absPath)), maxCacheNamePartBytes)
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
		return "", fmt.Errorf("create the index cache directory %s: %w", filepath.Dir(cachePath), err)
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

// Load reads and parses the cache file for sourcePath. Returns an error
// wrapping ErrIndexNotFound when there is no usable index (see
// LoadFromPath); the caller typically reacts by building a fresh index.
func Load(sourcePath string) (*rxtypes.UnifiedFileIndex, error) {
	cachePath := GetCachePath(sourcePath)
	return LoadFromPath(cachePath)
}

// LoadFromPath reads the file at cachePath. Useful for tests that want
// to hand-place a cache file at a known location.
//
// Every way of not getting a usable index ends in an error that wraps
// ErrIndexNotFound, so callers need one errors.Is check to treat it as
// absent: they rebuild, or answer without an index, and never fail
// because of it. An index only makes an answer faster.
//
// A missing file (IsNoCacheEntry) and one of another format version are
// ordinary misses.
// A file that cannot be read (its permissions, an I/O error) or parsed
// (cut short by a power loss or a full disk) is a miss too, but it also
// logs one "index_unreadable" warning naming the file, the way the
// trace cache logs "trace_cache_unreadable": the cost is a rebuild or a
// slower answer, and the operator should know why.
func LoadFromPath(cachePath string) (*rxtypes.UnifiedFileIndex, error) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		// No file can be at cachePath (none was written, or the cache
		// directory is under a regular file): an ordinary miss, not a
		// damaged index, so nothing is logged.
		if IsNoCacheEntry(err) {
			return nil, ErrIndexNotFound
		}
		return nil, unreadableIndex(cachePath, fmt.Errorf("read %s: %w", cachePath, err))
	}
	var idx rxtypes.UnifiedFileIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, unreadableIndex(cachePath, fmt.Errorf("unmarshal %s: %w", cachePath, err))
	}
	// An index whose schema we do not know is not a usable index, so
	// report it the same way as a missing one. Wrapping ErrIndexNotFound
	// keeps errors.Is working, which is what every caller branches on:
	// they rebuild, or fall back to a linear scan, instead of failing.
	// Reading it anyway is what produced off-by-one line numbers from
	// caches left behind by an older rx.
	if idx.Version != Version {
		return nil, fmt.Errorf("%w: %s has index version %d, want %d",
			ErrIndexNotFound, cachePath, idx.Version, Version)
	}
	// A time section a search cannot trust (damaged, or edited by hand)
	// makes the whole index damaged: a search by time would skip to a
	// checkpoint on its word.
	if err := validTimeIndex(&idx); err != nil {
		return nil, unreadableIndex(cachePath, fmt.Errorf("%s: %w", cachePath, err))
	}
	return &idx, nil
}

// unreadableIndex logs the warning for an index file at cachePath that
// exists but cannot be read or parsed, and returns cause wrapped so
// that errors.Is(err, ErrIndexNotFound) holds: the caller treats the
// file as absent. errors.Is(err, ErrIndexUnreadable) holds too, for a
// caller that replaces the damaged file. The returned error still
// carries cause's text.
func unreadableIndex(cachePath string, cause error) error {
	slog.Default().Warn("index_unreadable",
		"path", cachePath,
		"error", cause.Error(),
	)
	// fmt.Errorf with several %w verbs makes an error that matches each
	// target under errors.Is: ErrIndexNotFound for the caller's branch,
	// ErrIndexUnreadable for one that rebuilds, and the original cause
	// (fs.ErrPermission, a *json.SyntaxError …) for anyone who needs to
	// tell the reasons apart.
	return fmt.Errorf("%w: %w: %w", ErrIndexNotFound, ErrIndexUnreadable, cause)
}

// IsValidForSource reports whether idx is a faithful description of
// sourcePath's current state on disk. Returns false (no error) on
// stat failures — a missing/unreadable source is treated as "index
// is stale".
//
// Invalidation has no TTL and no size cap. The identity the index
// records (size, mtime, inode, ctime and fingerprint) is compared with
// the file through SourceIdentity.MatchesFile, the same check the trace
// cache uses.
func IsValidForSource(idx *rxtypes.UnifiedFileIndex, sourcePath string) bool {
	return recordedIdentity(idx).MatchesFile(sourcePath)
}

// recordedIdentity gathers the identity fields an index carries.
func recordedIdentity(idx *rxtypes.UnifiedFileIndex) SourceIdentity {
	return SourceIdentity{
		SizeBytes:   idx.SourceSizeBytes,
		ModifiedAt:  idx.SourceModifiedAt,
		ModifiedNs:  idx.SourceMtimeNs,
		Inode:       idx.SourceInode,
		Device:      idx.SourceDevice,
		ChangedAt:   idx.SourceChangedAt,
		ChangedNs:   idx.SourceCtimeNs,
		Fingerprint: idx.SourceFingerprint,
	}
}

// stampInto writes id into the identity fields of idx.
func (id SourceIdentity) stampInto(idx *rxtypes.UnifiedFileIndex) {
	idx.SourceModifiedAt = id.ModifiedAt
	idx.SourceMtimeNs = id.ModifiedNs
	idx.SourceSizeBytes = id.SizeBytes
	idx.SourceInode = id.Inode
	idx.SourceDevice = id.Device
	idx.SourceChangedAt = id.ChangedAt
	idx.SourceCtimeNs = id.ChangedNs
	idx.SourceFingerprint = id.Fingerprint
}

// FormatMtime exposes the mtime-to-string conversion so the trace
// cache and tests can stamp the same format. Local time, NOT UTC —
// Python parity.
func FormatMtime(t time.Time) string { return formatMtime(t) }

// formatMtime converts t to the Python-compatible ISO layout in LOCAL
// time. Using t.Local() instead of t.UTC() is deliberate: Python's
// datetime.fromtimestamp(os.stat(...).st_mtime) returns a naive local
// datetime, and its isoformat() drops tzinfo — so the stored string
// reflects wall-clock at the host, not UTC.
//
// The text depends on the time zone: one mtime reads differently under
// another TZ, and two mtimes an hour apart read the same in the hour a
// daylight-saving change repeats. That is why no identity check
// compares it. Caches compare the nanosecond fields of SourceIdentity,
// and keep this text for a person reading the cache file.
//
// FRACTIONAL-SECOND PARITY:
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
// helper most callers want. Returns (nil, err) with err wrapping
// ErrIndexNotFound if the cache file is absent, of another version,
// unreadable or truncated. Returns (idx, nil) iff the cache is present
// and valid. Returns (nil, nil) if the cache is present but stale —
// the caller should rebuild.
//
// Each call is one index cache lookup in the metrics: a hit when it
// returns an index, a miss otherwise, and the time taken whenever an
// index file was read. A caller that only reports whether an index
// exists, without using it, calls PeekForSource instead.
func LoadForSource(sourcePath string) (*rxtypes.UnifiedFileIndex, error) {
	start := time.Now()
	idx, err := PeekForSource(sourcePath)
	if err == nil {
		prometheus.RecordIndexLoadDuration(time.Since(start))
	}
	if idx == nil {
		prometheus.IncIndexCacheMisses()
	} else {
		prometheus.IncIndexCacheHits()
	}
	return idx, err
}

// PeekForSource answers exactly as LoadForSource does without counting
// as an index cache lookup, for a listing that shows whether each file
// has an index: listing a directory is not a use of its indexes.
func PeekForSource(sourcePath string) (*rxtypes.UnifiedFileIndex, error) {
	idx, err := Load(sourcePath)
	if err != nil {
		return nil, err
	}
	if !IsValidForSource(idx, sourcePath) {
		return nil, nil
	}
	return idx, nil
}
