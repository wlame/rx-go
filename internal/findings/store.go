package findings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/sigscan"
)

// The names inside a file's cache directory. A signature id is
// kebab-case ([a-z0-9-]), so "<id>.json" never starts with a dot and
// never holds a second dot: no entry name can be lockName or a
// temporary file. An id may be "meta", though, so an entry is named
// "<id>.json" only after metaName is ruled out: entryName gives "meta"
// a name of its own.
const (
	metaName    = "meta.json"
	lockName    = ".lock"
	tmpPrefix   = ".tmp-"
	entrySuffix = ".json"
	// metaEntryName is the entry file of a signature whose id is
	// "meta", whose plain name would be meta.json. Ids never hold a
	// dot, so no other id gives this name.
	metaEntryName = "meta.signature.json"
)

// The bounds of what a read of the cache takes into memory. meta.json
// holds one short summary per signature: at most a few hundred bytes
// for each of the at most a few thousand loaded signatures. An entry
// holds at most Cap findings of at most about 1.5 KiB each in JSON (a
// 200-byte excerpt whose every byte JSON may escape as six) and a
// density series of at most 65,536 buckets.
const (
	maxMetaBytes       = 16 << 20
	maxFindingBytes    = 2 << 10
	maxEntryExtraBytes = 4 << 20
)

// The directory lock. A writer polls a held lock every lockPoll and
// gives up after lockWait: writers hold it only to write a few files,
// so a lock held that long belongs to a process that is stuck, and the
// scan's answer is then given without being cached.
const (
	lockPoll = 5 * time.Millisecond
	lockWait = 10 * time.Second
)

// errLockTimeout reports a directory lock another process held for
// longer than lockWait.
var errLockTimeout = errors.New("another process holds the findings cache directory")

// cacheDir is the directory of the file at path inside the findings
// cache dir: "<safe_basename>_<sha256(abs path)[:16]>", named as the
// line index names its files (the same sanitizing, the base name cut so
// the name fits in index.MaxCacheFileNameBytes).
func cacheDir(dir, path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	sum := sha256.Sum256([]byte(abs))
	hash16 := hex.EncodeToString(sum[:8])
	base := index.TrimCacheNamePart(index.SafeBasename(filepath.Base(abs)), index.MaxCacheFileNameBytes-len("_")-len(hash16))
	return filepath.Join(dir, base+"_"+hash16)
}

// entryName is the file name of a signature's entry.
func entryName(id string) string {
	if id+entrySuffix == metaName {
		return metaEntryName
	}
	return id + entrySuffix
}

// metaFile is meta.json: the identity of the file the entries describe
// (named as the line index names these fields) and one summary per
// signature. It is the commit point: an entry it does not name is not
// part of the cache.
type metaFile struct {
	Version           int                  `json:"version"`
	SourcePath        string               `json:"source_path"`
	SourceSizeBytes   int64                `json:"source_size_bytes"`
	SourceMtimeNs     int64                `json:"source_mtime_ns"`
	SourceInode       *uint64              `json:"source_inode"`
	SourceDevice      *uint64              `json:"source_device"`
	SourceCtimeNs     *int64               `json:"source_ctime_ns"`
	SourceFingerprint *string              `json:"source_fingerprint"`
	Signatures        map[string]metaEntry `json:"signatures"`
}

// metaEntry is meta.json's summary of one signature's entry.
type metaEntry struct {
	Key string `json:"key"`
	// Count is the number of findings in the file, past the cap too;
	// Stored is how many the entry file holds.
	Count     int64     `json:"count"`
	Stored    int       `json:"stored"`
	ScannedAt time.Time `json:"scanned_at"`
}

// newMeta starts a meta.json for the file at sourcePath as id describes
// it, with no signature.
func newMeta(sourcePath string, id index.SourceIdentity) *metaFile {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		abs = sourcePath
	}
	return &metaFile{
		Version: CacheVersion, SourcePath: abs,
		SourceSizeBytes: id.SizeBytes, SourceMtimeNs: id.ModifiedNs,
		SourceInode: id.Inode, SourceDevice: id.Device, SourceCtimeNs: id.ChangedNs,
		SourceFingerprint: id.Fingerprint,
		Signatures:        map[string]metaEntry{},
	}
}

// identity is the identity meta.json records. The text times, which no
// check compares, are left empty.
func (m *metaFile) identity() index.SourceIdentity {
	return index.SourceIdentity{
		SizeBytes: m.SourceSizeBytes, ModifiedNs: m.SourceMtimeNs,
		Inode: m.SourceInode, Device: m.SourceDevice, ChangedNs: m.SourceCtimeNs,
		Fingerprint: m.SourceFingerprint,
	}
}

// entryFile is "<id>.json": the findings of one signature in one file.
// Every field is written, null when empty, so a finding read back is
// equal to the one the scan returned, field by field (a nil list stays
// nil, an empty one empty).
type entryFile struct {
	Key      string        `json:"key"`
	Findings []findingJSON `json:"findings"`
	Series   *seriesJSON   `json:"series"`
}

// findingJSON is one finding as the cache stores it.
type findingJSON struct {
	StartLine   int64  `json:"start_line"`
	EndLine     int64  `json:"end_line"`
	StartOffset int64  `json:"start_offset"`
	EndOffset   int64  `json:"end_offset"`
	TimeMS      *int64 `json:"time_ms"`
	Excerpt     string `json:"excerpt"`
	ValueStart  *int   `json:"value_start"`
	ValueEnd    *int   `json:"value_end"`
	Masked      bool   `json:"masked"`
}

// seriesJSON is a density measure's bucket series.
type seriesJSON struct {
	BucketMS int64   `json:"bucket_ms"`
	StartMS  int64   `json:"start_ms"`
	Counts   []int64 `json:"counts"`
}

// entryOf is the entry file of one scanned signature.
func entryOf(key string, r *sigscan.SignatureResult) *entryFile {
	e := &entryFile{Key: key}
	if r.Findings != nil {
		e.Findings = make([]findingJSON, len(r.Findings))
		for i, f := range r.Findings {
			e.Findings[i] = findingJSON{StartLine: f.StartLine, EndLine: f.EndLine, StartOffset: f.StartOffset,
				EndOffset: f.EndOffset, TimeMS: f.TimeMS, Excerpt: f.Excerpt, ValueStart: f.ValueStart,
				ValueEnd: f.ValueEnd, Masked: f.Masked}
		}
	}
	if r.Series != nil {
		e.Series = &seriesJSON{BucketMS: r.Series.BucketMS, StartMS: r.Series.StartMS, Counts: r.Series.Counts}
	}
	return e
}

// result turns an entry file back into the scan's result, with the
// count meta.json records.
func (e *entryFile) result(count int64) *sigscan.SignatureResult {
	r := &sigscan.SignatureResult{Count: count}
	if e.Findings != nil {
		r.Findings = make([]sigscan.Finding, len(e.Findings))
		for i, f := range e.Findings {
			r.Findings[i] = sigscan.Finding{StartLine: f.StartLine, EndLine: f.EndLine, StartOffset: f.StartOffset,
				EndOffset: f.EndOffset, TimeMS: f.TimeMS, Excerpt: f.Excerpt, ValueStart: f.ValueStart,
				ValueEnd: f.ValueEnd, Masked: f.Masked}
		}
	}
	if e.Series != nil {
		r.Series = &sigscan.Series{BucketMS: e.Series.BucketMS, StartMS: e.Series.StartMS, Counts: e.Series.Counts}
	}
	return r
}

// maxEntryBytes bounds the entry file of a scan whose cap is capSetting
// (0 or less: the scan's default).
func maxEntryBytes(capSetting int) int64 {
	if capSetting <= 0 {
		capSetting = sigscan.DefaultCap
	}
	return int64(capSetting)*maxFindingBytes + maxEntryExtraBytes
}

// errAbsent reports a cache file that is not there (or a cache
// directory that cannot hold one): an ordinary miss.
var errAbsent = errors.New("no cache file")

// readJSON decodes the JSON file at path into v, through open, reading
// at most limit bytes. A file that is not there is errAbsent; a larger
// one, or one that does not parse, is an error.
func readJSON(open func(string) (*os.File, error), path string, limit int64, v any) error {
	f, err := open(path)
	if err != nil {
		if index.IsNoCacheEntry(err) {
			return errAbsent
		}
		return err
	}
	// Go note: defer runs when readJSON returns, so the file is closed
	// on every path below.
	defer func() { _ = f.Close() }()
	// One byte more than the limit tells a file of exactly the limit
	// from a longer one.
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// writeJSONAtomic writes v as JSON to name in dir: a temporary file of
// its own in dir (tmpPrefix, created with O_EXCL and mode 0600), then a
// rename over the name, so a reader sees the old file or the new one,
// never part of one. A failure removes the temporary file.
func writeJSONAtomic(dir, name string, v any) (err error) {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	// Go note: a deferred function can read and change the named result
	// err, so the temporary file is removed whenever writeJSONAtomic
	// fails after creating it.
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err = os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("rename %s: %w", tmp.Name(), err)
	}
	return nil
}

// dirLock is a held lock on a cache directory.
type dirLock struct {
	f *os.File
	// held is false when the filesystem does not support the lock: the
	// writer then goes on without it, as a writer did before locks, and
	// removes no temporary file it did not write.
	held bool
}

// lockDirectory takes the advisory lock of a cache directory (flock on
// its lockName file, created when missing and never removed, since
// removing a lock file lets two processes lock two files). The lock
// stops other processes, and other schedulers of this process, from
// writing the directory meanwhile; it is released when the process
// ends, whatever way it ends.
//
// It polls without blocking, so it can stop when ctx is canceled and
// after lockWait.
func lockDirectory(ctx context.Context, dir string) (*dirLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	// Go note: Fd returns the descriptor the syscall package works on;
	// flock locks belong to the open file, so two opens of the lock file
	// (two processes, or two schedulers of one process) exclude each
	// other.
	fd := int(f.Fd()) // #nosec G115 -- a file descriptor fits in an int
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return &dirLock{f: f, held: true}, nil
		case errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR):
			// Held by another writer (or interrupted): poll again.
		default:
			// The filesystem cannot lock (some network filesystems):
			// write without the lock.
			return &dirLock{f: f}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errLockTimeout
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// release lets the lock go. Closing the file releases a flock too; the
// explicit unlock says so.
func (l *dirLock) release() {
	if l.held {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) // #nosec G115 -- a file descriptor fits in an int
	}
	_ = l.f.Close()
}

// removeUnnamed removes from a cache directory every entry file that
// meta does not name and, when the writer holds the lock, every
// temporary file (one left by a crash: under the lock no other writer
// has one open). It leaves meta.json, the lock file and any name it
// does not know. Removal is best effort: a file that stays is never
// read, since meta.json does not name it.
func removeUnnamed(dir string, meta *metaFile, lockHeld bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	named := map[string]bool{metaName: true, lockName: true}
	for id := range meta.Signatures {
		named[entryName(id)] = true
	}
	for _, e := range entries {
		name := e.Name()
		temporary := strings.HasPrefix(name, tmpPrefix)
		orphan := strings.HasSuffix(name, entrySuffix) && !temporary && !named[name]
		if (temporary && lockHeld) || orphan {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
