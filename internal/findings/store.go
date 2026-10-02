package findings

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	// rangesName is the file of the file's masked ranges, one per
	// cache directory. It holds dots, so no id gives it.
	rangesName = "meta.ranges.json"
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
	// maxRangeBytes is one masked range in JSON: two numbers of at most
	// 19 digits, brackets and commas.
	maxRangeBytes = 48
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

// errUnsafeEntry reports a symbolic link, or a file of another kind,
// where the findings cache keeps a directory or a regular file. The
// cache never follows, writes through or deletes through such an entry:
// the call treats it as a miss and writes nothing there.
var errUnsafeEntry = errors.New("not a plain directory or regular file of the findings cache (a symbolic link?)")

// unsafeWarned is set once an unsafe cache entry has been logged, so a
// planted link gives one warning per process, not one per request.
var unsafeWarned atomic.Bool

// warnUnsafe logs the first unsafe cache entry of the process.
func warnUnsafe(path string) {
	// Go note: CompareAndSwap lets exactly one caller through.
	if unsafeWarned.CompareAndSwap(false, true) {
		slog.Default().Warn("findings_cache_unsafe_entry", "path", path,
			"note", "a symbolic link or special file in the findings cache is never followed; "+
				"that file's findings are not cached. Later cases are not logged")
	}
}

// maxIDBytes is the longest signature id the loader accepts.
const maxIDBytes = 64

// entryDirName is the name of the directory of the file at path inside
// the findings cache: "<safe_basename>_<sha256(abs path)[:16]>", named as
// the line index names its files (the same sanitizing, the base name cut
// so the name fits in index.MaxCacheFileNameBytes). It is one path
// component: the sanitizing leaves no separator.
func entryDirName(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	sum := sha256.Sum256([]byte(abs))
	hash16 := hex.EncodeToString(sum[:8])
	base := index.TrimCacheNamePart(index.SafeBasename(filepath.Base(abs)), index.MaxCacheFileNameBytes-len("_")-len(hash16))
	return base + "_" + hash16
}

// cacheDir is the path of the file's directory inside the findings
// cache dir.
func cacheDir(dir, path string) string {
	return filepath.Join(dir, entryDirName(path))
}

// entryName is the file name of a signature's entry, and false for an id
// that is not one the loader accepts (kebab-case [a-z0-9-], 1 to 64
// bytes): such an id is never cached, whatever reached this package.
func entryName(id string) (string, bool) {
	if !validID(id) {
		return "", false
	}
	if id+entrySuffix == metaName {
		return metaEntryName, true
	}
	return id + entrySuffix, true
}

// validID reports whether id is 1 to maxIDBytes bytes of a-z, 0-9 and
// '-': a name with no separator, no dot and no NUL.
func validID(id string) bool {
	if id == "" || len(id) > maxIDBytes {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// openEntryDir opens the directory name of the findings cache dir base
// as an *os.Root: every file operation of the cache goes through it, and
// a root refuses a path that leaves it. With create, the cache dir and
// the directory are made when missing.
//
// SECURITY: the directory must be a plain directory. A symbolic link in
// its place (another local user who can write a shared cache dir could
// plant one toward a directory of yours) is refused with errUnsafeEntry
// and logged, never followed: the cache would otherwise write its files
// there and remove the ones it does not know. The directory is checked
// by Lstat, opened, and the opened directory compared with the one
// Lstat saw, so a swap between the two is refused too.
//
// A directory that does not exist (without create) is errAbsent.
func openEntryDir(base, name string, create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(base, 0o750); err != nil {
			return nil, err
		}
	}
	baseRoot, err := os.OpenRoot(base)
	if err != nil {
		if index.IsNoCacheEntry(err) {
			return nil, errAbsent
		}
		return nil, err
	}
	// Go note: defer closes the cache dir's handle when openEntryDir
	// returns; the directory's own root stays open for the caller.
	defer func() { _ = baseRoot.Close() }()
	if create {
		// mkdirat never follows a link: an existing entry of any kind,
		// a link included, gives fs.ErrExist and is checked below.
		if mkErr := baseRoot.Mkdir(name, 0o750); mkErr != nil && !errors.Is(mkErr, fs.ErrExist) {
			return nil, mkErr
		}
	}
	seen, err := baseRoot.Lstat(name)
	if err != nil {
		if index.IsNoCacheEntry(err) {
			return nil, errAbsent
		}
		return nil, err
	}
	path := filepath.Join(base, name)
	if !seen.IsDir() { // the Lstat of a link is not a directory
		warnUnsafe(path)
		return nil, errUnsafeEntry
	}
	dir, err := baseRoot.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(seen, opened) {
		_ = dir.Close()
		warnUnsafe(path)
		return nil, errUnsafeEntry
	}
	return dir, nil
}

// openRegular opens name in the directory root when it is a regular
// file, with flag (os.O_RDONLY or os.O_RDWR).
//
// SECURITY: an *os.Root follows a symbolic link that stays inside it,
// even with O_NOFOLLOW, so the entry is checked by Lstat first: a link
// or a special file (a named pipe, whose open would wait for a writer)
// is refused with errUnsafeEntry and logged. O_NONBLOCK keeps the open
// from waiting if the entry is swapped for a pipe after the check, and
// the opened file must be the one Lstat saw.
//
// A writer replaces meta.json and the entries by renaming a new file
// over them, so a rename can land between the Lstat and the open: both
// are then regular files, but not the same one. That is an ordinary
// race, not an unsafe entry: openRegular checks again, up to openTries
// times, and a file replaced before every try is a miss (errReplaced)
// without a warning. Only a try that finds a link or a file of another
// kind warns.
func openRegular(root *os.Root, name string, flag int) (*os.File, error) {
	path := filepath.Join(root.Name(), name)
	for try := range openTries {
		seen, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !seen.Mode().IsRegular() {
			warnUnsafe(path)
			return nil, errUnsafeEntry
		}
		if beforeOpenRegular != nil {
			beforeOpenRegular(root, name, try)
		}
		f, err := root.OpenFile(name, flag|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		opened, err := f.Stat()
		switch {
		case err != nil:
			_ = f.Close()
			return nil, err
		case !opened.Mode().IsRegular():
			_ = f.Close()
			warnUnsafe(path)
			return nil, errUnsafeEntry
		case os.SameFile(seen, opened):
			return f, nil
		}
		// Two regular files: a writer renamed a new one into place
		// between the Lstat and the open. Check the new one.
		_ = f.Close()
	}
	return nil, errReplaced
}

// openTries bounds the checks of openRegular when writers keep renaming
// new files over the one it opens.
const openTries = 3

// errReplaced reports a cache file replaced by another writer before
// each of openRegular's tries: a miss, not an unsafe entry.
var errReplaced = errors.New("the cache file was replaced while it was opened")

// beforeOpenRegular, when set, runs between openRegular's Lstat and its
// open, with the try's number (tests: a rename in that instant).
var beforeOpenRegular func(root *os.Root, name string, try int)

// openCacheFile opens a cache file of the directory root for reading
// (openRegular): what scheduler.openCache does outside tests.
func openCacheFile(root *os.Root, name string) (io.ReadCloser, error) {
	return openRegular(root, name, os.O_RDONLY)
}

// metaFile is meta.json: the identity of the file the entries describe
// (named as the line index names these fields), one summary per
// signature, and the summary of the masked ranges file. It is the
// commit point: an entry it does not name is not part of the cache.
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
	MaskedRanges      rangesMeta           `json:"masked_ranges"`
}

// rangesMeta is meta.json's summary of the masked ranges file: its key
// (keyScope.rangesKey) and how many ranges it holds. A meta.json
// without it (an empty key) has no current entry.
type rangesMeta struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// rangesFile is meta.ranges.json: the file's masked ranges, as a scan
// with the key's redacting set and line view found them, and the end of
// the text that scan read (its line count and length in bytes).
type rangesFile struct {
	Key       string     `json:"key"`
	Ranges    [][2]int64 `json:"ranges"`
	TextLines int64      `json:"text_lines"`
	TextBytes int64      `json:"text_bytes"`
}

// scannedText is what an answer knows of the text its findings
// describe: the masked ranges, and where the text the scan read ends
// (sigscan.Result's MaskedRanges, TextLines and TextBytes). Every
// carrier of a finding's text masks the ranges and shows nothing past
// the end.
type scannedText struct {
	ranges       [][2]int64
	lines, bytes int64
}

// textOf is the scanned text of a scan's result.
func textOf(res *sigscan.Result) scannedText {
	return scannedText{ranges: res.MaskedRanges, lines: res.TextLines, bytes: res.TextBytes}
}

// fileOf is the ranges file that records text under key.
func (st scannedText) fileOf(key string) rangesFile {
	ranges := st.ranges
	if ranges == nil {
		ranges = [][2]int64{}
	}
	return rangesFile{Key: key, Ranges: ranges, TextLines: st.lines, TextBytes: st.bytes}
}

// text is the scanned text a ranges file records; ok is false when the
// file is not as a scan writes it: the ranges are not sorted and
// disjoint (validRanges), a range lies past the end it records, or that
// end has fewer bytes than lines (every line holds a byte at least).
func (f rangesFile) text() (scannedText, bool) {
	if !validRanges(f.Ranges) || f.TextLines < 0 || f.TextBytes < f.TextLines {
		return scannedText{}, false
	}
	if n := len(f.Ranges); n > 0 && f.Ranges[n-1][1] > f.TextLines {
		return scannedText{}, false
	}
	ranges := f.Ranges
	if ranges == nil {
		ranges = [][2]int64{}
	}
	return scannedText{ranges: ranges, lines: f.TextLines, bytes: f.TextBytes}, true
}

// maxRangesBytes bounds the masked ranges file of a catalogue with
// redactors redacting signatures: each keeps at most
// sigscan.MaxBodiesPerSignature bodies and one range past them, and
// joined ranges are never more than the bodies.
func maxRangesBytes(redactors int) int64 {
	return int64(redactors+1)*(sigscan.MaxBodiesPerSignature+1)*maxRangeBytes + maxEntryExtraBytes
}

// validRanges reports whether ranges are as a scan gives them: lines
// from 1, each range's first line at most its last, sorted, with a line
// between one range and the next. A file that holds anything else is
// not read.
func validRanges(ranges [][2]int64) bool {
	prev := int64(-1)
	for _, r := range ranges {
		if r[0] < 1 || r[0] > r[1] || r[0] <= prev+1 {
			return false
		}
		prev = r[1]
	}
	return true
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

// endsPast reports whether a finding of the entry ends past line lines,
// the end of the scanned text the ranges file records: the two do not
// describe one text, so the entry is not used (its text would be read
// without a bound).
func (e *entryFile) endsPast(lines int64) bool {
	for _, f := range e.Findings {
		if f.EndLine > lines {
			return true
		}
	}
	return false
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

// readJSON decodes the cache file name of the directory root into v,
// opened through open, reading at most limit bytes. A file that is not
// there is errAbsent; a larger one, or one that does not parse, is an
// error; a link or a special file is errUnsafeEntry.
func readJSON(open func(*os.Root, string) (io.ReadCloser, error), root *os.Root, name string, limit int64, v any) error {
	f, err := open(root, name)
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
		return fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

// tmpAttempts bounds the tries for a free temporary name (8 random
// bytes each, so a second try is already rare).
const tmpAttempts = 8

// writeJSONAtomic writes v as JSON to name in the directory root: a
// temporary file of its own (tmpPrefix and 16 random hex digits,
// created with O_EXCL and mode 0600, which never opens an existing file
// or link), then a rename over the name. A reader sees the old file or
// the new one, never part of one, and a rename replaces a link of that
// name rather than writing through it. A failure removes the temporary
// file.
func writeJSONAtomic(root *os.Root, name string, v any) (err error) {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	tmp, tmpName, err := createTemp(root)
	if err != nil {
		return err
	}
	// Go note: a deferred function can read and change the named result
	// err, so the temporary file is removed whenever writeJSONAtomic
	// fails after creating it.
	defer func() {
		if err != nil {
			_ = root.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err = root.Rename(tmpName, name); err != nil {
		return fmt.Errorf("rename %s: %w", tmpName, err)
	}
	return nil
}

// createTemp creates a new temporary file in the directory root.
func createTemp(root *os.Root) (*os.File, string, error) {
	var lastErr error
	for range tmpAttempts {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, "", err
		}
		name := tmpPrefix + hex.EncodeToString(b[:])
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
		lastErr = err
	}
	return nil, "", lastErr
}

// dirLock is a held lock on a cache directory.
type dirLock struct {
	f *os.File
	// held is false when the filesystem does not support the lock: the
	// writer then goes on without it, as a writer did before locks, and
	// removes no temporary file it did not write.
	held bool
}

// lockDirectory takes the advisory lock of the cache directory root
// (flock on its lockName file, created when missing and never removed,
// since removing a lock file lets two processes lock two files). The
// lock stops other processes, and other schedulers of this process,
// from writing the directory meanwhile; it is released when the process
// ends, whatever way it ends. A lock file that is a link or a special
// file is errUnsafeEntry: nothing is written.
//
// It polls without blocking, so it can stop when ctx is canceled and
// after lockWait.
func lockDirectory(ctx context.Context, root *os.Root) (*dirLock, error) {
	// O_CREATE with O_EXCL makes a new lock file and never opens an
	// existing entry; an existing one is opened only when it is a
	// regular file (openRegular).
	f, err := root.OpenFile(lockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		f, err = openRegular(root, lockName, os.O_RDWR)
	}
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

// removeUnnamed removes from the cache directory root every entry file
// that meta does not name and, when the writer holds the lock, every
// temporary file (one left by a crash: under the lock no other writer
// has one open). It leaves meta.json, the lock file and any name it
// does not know. A removal unlinks the name itself: a link is removed,
// never what it points at. Removal is best effort: a file that stays is
// never read, since meta.json does not name it.
func removeUnnamed(root *os.Root, meta *metaFile, lockHeld bool) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return
	}
	named := map[string]bool{metaName: true, lockName: true, rangesName: true}
	for id := range meta.Signatures {
		if name, ok := entryName(id); ok {
			named[name] = true
		}
	}
	for _, e := range entries {
		name := e.Name()
		temporary := strings.HasPrefix(name, tmpPrefix)
		orphan := strings.HasSuffix(name, entrySuffix) && !temporary && !named[name]
		if (temporary && lockHeld) || orphan {
			_ = root.Remove(name)
		}
	}
}
