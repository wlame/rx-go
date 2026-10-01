package findings

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/trace"
)

// errNoCacheDir reports a call that would use the cache without
// ScanOptions.Dir.
var errNoCacheDir = errors.New("findings: no cache directory (ScanOptions.Dir)")

// scheduler runs the scans of one process: it holds the queue of scans
// (at most SetMaxScans at once) and one lock per file, so one scan of a
// file runs at a time. The package functions use defaultScheduler; a
// test makes several to stand for several processes.
type scheduler struct {
	slots *slotLimiter
	files *fileLocks

	// Seams, set by tests only.
	// scanFile is the scan (sigscan.Scan).
	scanFile func(ctx context.Context, file trace.SearchFile, sigs []*signatures.Signature, opts sigscan.Options) (*sigscan.Result, error)
	// wrapSource, when set, wraps every read of the scanned file this
	// package makes itself (the identity's fingerprint).
	wrapSource func(io.ReaderAt) io.ReaderAt
	// afterScan, when set, runs after the scan read the file and before
	// the identity is checked again.
	afterScan func()
	// beforeMeta, when set, runs after the entry files are written and
	// before meta.json; an error stops the write there, as a crash would.
	beforeMeta func() error
	// openCache opens a cache file of a cache directory for reading
	// (openCacheFile, which refuses a link or a special file).
	openCache func(root *os.Root, name string) (io.ReadCloser, error)
	// now is the clock of scanned_at (time.Now).
	now func() time.Time
}

// newScheduler returns a scheduler that runs at most maxScans scans at
// once (clamped to 1–64).
func newScheduler(maxScans int) *scheduler {
	return &scheduler{
		slots: newSlotLimiter(maxScans), files: &fileLocks{},
		scanFile: sigscan.Scan, openCache: openCacheFile, now: time.Now,
	}
}

// defaultScheduler is the scheduler of the process, behind Scan, Read
// and Summary. It runs one scan at a time until SetMaxScans says
// otherwise.
var defaultScheduler = newScheduler(1)

// SetMaxScans sets how many scans read files at once in this process,
// clamped to 1–64; until it is called the limit is 1. An entry point
// that runs scans side by side (the HTTP server) calls it once at start
// with RX_MAX_SIGNATURE_SCANS, since this package reads no environment.
// Scans past the limit wait, first come first served; a larger limit
// lets waiting scans start at once.
func SetMaxScans(n int) {
	defaultScheduler.slots.setLimit(n)
}

// Scan answers the findings of sel in file: every signature with a
// current cache entry from the cache, the others from one scan of the
// file, whose entries it then writes. file is pinned and classified
// (trace.ClassifyForSearch); every read of it goes through its pin. cat
// is the loaded catalogue: its redacting signatures mask every excerpt
// (opts.Scan.Redactors is replaced by cat.Redactors()) and enter every
// key, and its default set decides the answer's State.
//
// opts.Force scans every selected signature; opts.NoCache and
// opts.Spooled read and write nothing. Within the process one scan of a
// file runs at a time: a second call waits for the first and then scans
// what is still missing. A call that has something to scan waits for a
// place in the queue first (SetMaxScans).
//
// The file's identity is taken before the cache is read and again after
// the scan. When the file changed meanwhile, the answer is given and
// nothing is written; when the file is shorter than it was, or its path
// leads to another file, Scan returns an error that names it, since the
// answer could hold offsets past the file's end.
//
// Canceling ctx stops the wait or the scan; Scan then returns ctx.Err()
// and writes nothing. A scan that fails returns its error, which names
// the file, and writes nothing. A cache that cannot be written does not
// fail the call: the answer is given, and the first such failure of the
// process is logged.
func Scan(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts ScanOptions) (*Answer, error) {
	return defaultScheduler.scan(ctx, file, cat, sel, opts)
}

// Read answers the findings of sel in file from the cache alone; it
// never scans. A selected signature without a current entry is in
// Missing. With opts.NoCache or opts.Spooled every one is missing. It
// reads meta.json, the selected entries, and the file's identity
// (through its pin: a stat and the two 64 KiB windows of the
// fingerprint), and does not wait for a scan that runs.
func Read(file trace.SearchFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts ScanOptions) (*Answer, error) {
	return defaultScheduler.read(file, cat, sel, opts)
}

// Summary says how much of cat's default set the file pinned by file
// has cached, for the file tree: nil when no entry is current. It reads
// meta.json and nothing else: the file's identity is compared by the
// stat the pin holds (size, mtime, inode, device, ctime), not by the
// fingerprint, which would read the file. Scan and Read compare the
// fingerprint too.
func Summary(file paths.Pinned, cat *signatures.Catalogue, opts ScanOptions) (*FileSummary, error) {
	return defaultScheduler.summary(file, cat, opts)
}

// scan is Scan on this scheduler.
func (s *scheduler) scan(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts ScanOptions) (*Answer, error) {
	sel = uniqueSignatures(sel)
	scanOpts := s.scanOptions(cat, opts)
	if !opts.usesCache() {
		res, err := s.scanQueued(ctx, file, sel, scanOpts)
		if err != nil {
			return nil, err
		}
		fresh := freshEntries(sel, res, s.scannedAt())
		return newAnswer(cat, sel, fresh, idSet(sel)), nil
	}
	if opts.Dir == "" {
		return nil, errNoCacheDir
	}
	dirName := entryDirName(file.Path())
	// One scan of a file at a time in this scheduler; the lock is taken
	// before the cache is read, so a caller that waited sees what the
	// one before it wrote and scans only what is still missing.
	unlock, err := s.files.lock(ctx, filepath.Join(opts.Dir, dirName))
	if err != nil {
		return nil, err
	}
	// Go note: defer releases the lock when scan returns, on every path.
	defer unlock()

	start, err := s.identityOf(file.Pinned())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file.Path(), err)
	}
	scope := newKeyScope(cat, opts)
	view := s.loadView(opts.Dir, dirName, start, scope, cat, sel, opts.Scan.Cap)
	missing := sel
	if !opts.Force {
		missing = view.missing(sel)
	}
	if len(missing) == 0 {
		return newAnswer(cat, sel, view.entries, view.current), nil
	}

	res, err := s.scanQueued(ctx, file, missing, scanOpts)
	if err != nil {
		return nil, err
	}
	if s.afterScan != nil {
		s.afterScan()
	}
	unchanged, err := s.unchangedSince(file, start)
	if err != nil {
		return nil, err
	}
	// A cancel that came after the scan ended still writes nothing.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	fresh := freshEntries(missing, res, s.scannedAt())
	if unchanged {
		// An unsafe directory or lock file was logged where it was
		// found; any other failure is logged here, once per process.
		if err := s.commit(ctx, opts.Dir, dirName, file.Path(), start, scope, missing, fresh); err != nil && !errors.Is(err, errUnsafeEntry) {
			warnWriteFailed(file.Path(), err)
		}
	}
	entries, current := view.entries, view.current
	for id, e := range fresh {
		entries[id] = e
		current[id] = true
	}
	return newAnswer(cat, sel, entries, current), nil
}

// read is Read on this scheduler.
func (s *scheduler) read(file trace.SearchFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts ScanOptions) (*Answer, error) {
	sel = uniqueSignatures(sel)
	if !opts.usesCache() {
		return newAnswer(cat, sel, nil, nil), nil
	}
	if opts.Dir == "" {
		return nil, errNoCacheDir
	}
	root, err := openEntryDir(opts.Dir, entryDirName(file.Path()), false)
	if err != nil {
		// Absent, unsafe (logged) or unreadable: a miss.
		return newAnswer(cat, sel, nil, nil), nil
	}
	// Go note: defer closes the directory when read returns.
	defer func() { _ = root.Close() }()
	scope := newKeyScope(cat, opts)
	meta, err := s.readMeta(root)
	if err != nil {
		// Absent, or unreadable (readMeta logged it): a miss.
		return newAnswer(cat, sel, nil, nil), nil
	}
	// The entries are read before the identity is checked. A writer
	// that scanned a changed file wrote its entries after the change,
	// so if any entry read here came from it, the check below sees the
	// change and drops them all.
	view := s.viewOf(root, meta, scope, cat, sel, opts.Scan.Cap)
	now, err := s.identityOf(file.Pinned())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file.Path(), err)
	}
	if !meta.identity().Equal(now) {
		return newAnswer(cat, sel, nil, nil), nil
	}
	return newAnswer(cat, sel, view.entries, view.current), nil
}

// summary is Summary on this scheduler.
func (s *scheduler) summary(file paths.Pinned, cat *signatures.Catalogue, opts ScanOptions) (*FileSummary, error) {
	if opts.Dir == "" {
		return nil, errNoCacheDir
	}
	if file.IsZero() {
		return nil, errors.New("findings: summary of no file")
	}
	root, err := openEntryDir(opts.Dir, entryDirName(file.Path()), false)
	if errors.Is(err, errAbsent) || errors.Is(err, errUnsafeEntry) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	meta, err := s.readMeta(root)
	if isQuietMiss(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !meta.identity().MatchesInfo(file.Info()) {
		return nil, nil
	}
	current := currentInMeta(meta, newKeyScope(cat, opts), cat)
	if len(current) == 0 {
		return nil, nil
	}
	var count int64
	for id := range current {
		count += meta.Signatures[id].Count
	}
	return &FileSummary{State: stateOf(cat, current), Count: count}, nil
}

// scanOptions are the sigscan options of a call: opts.Scan with the
// catalogue's redacting signatures (the set every key hashes) and, when
// set, opts.Progress.
func (s *scheduler) scanOptions(cat *signatures.Catalogue, opts ScanOptions) sigscan.Options {
	scanOpts := opts.Scan
	scanOpts.Redactors = cat.Redactors()
	if opts.Progress != nil {
		scanOpts.Progress = opts.Progress
	}
	return scanOpts
}

// scanQueued scans sigs once a place in the queue is free, and gives the
// place back when the scan ends.
func (s *scheduler) scanQueued(ctx context.Context, file trace.SearchFile, sigs []*signatures.Signature, opts sigscan.Options) (*sigscan.Result, error) {
	release, err := s.slots.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	res, err := s.scanFile(ctx, file, sigs, opts)
	if err != nil {
		return nil, err
	}
	for _, sig := range sigs {
		if res.BySignature[sig.ID] == nil {
			return nil, fmt.Errorf("%s: the scan returned no result for %s", file.Path(), sig.ID)
		}
	}
	return res, nil
}

// scannedAt is the time a scan's findings are stamped with: UTC, to the
// second, the precision meta.json keeps, so a fresh answer and a later
// read of it carry the same time.
func (s *scheduler) scannedAt() time.Time {
	return s.now().UTC().Truncate(time.Second)
}

// identityOf opens the pinned file, stats it and reads its fingerprint:
// the identity index.SourceIdentity checks, taken through the pin.
func (s *scheduler) identityOf(pin paths.Pinned) (index.SourceIdentity, error) {
	f, err := pin.Open()
	if err != nil {
		return index.SourceIdentity{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return index.SourceIdentity{}, err
	}
	var r io.ReaderAt = f
	if s.wrapSource != nil {
		r = s.wrapSource(r)
	}
	return index.IdentityFromReaderAt(r, info), nil
}

// unchangedSince checks the file after a scan against its identity at
// the start. It reports true when the file is unchanged, false when it
// changed but is at least as long (a log that grew: the answer, read
// from the bytes there at the start, is still true of the file), and an
// error naming the file when the path no longer leads to it or it is
// shorter (truncated, as by copytruncate): the answer could name
// offsets past its end.
func (s *scheduler) unchangedSince(file trace.SearchFile, start index.SourceIdentity) (bool, error) {
	now, err := s.identityOf(file.Pinned())
	if err != nil {
		return false, fmt.Errorf("%s: the file changed while it was scanned: %w", file.Path(), err)
	}
	if now.Equal(start) {
		return true, nil
	}
	if now.SizeBytes < start.SizeBytes {
		return false, fmt.Errorf("%s: the file got shorter while it was scanned (%d bytes, was %d)",
			file.Path(), now.SizeBytes, start.SizeBytes)
	}
	return false, nil
}

// cachedEntry is one signature's findings in an answer, with when they
// were scanned.
type cachedEntry struct {
	result    *sigscan.SignatureResult
	scannedAt time.Time
}

// cacheView is what the cache holds for a file: the selected entries it
// could read, and the ids of every current entry (selected or not).
type cacheView struct {
	// meta is meta.json as read; nil when it is absent, unreadable, of
	// another version or of another file state.
	meta *metaFile
	// entries are the selected signatures whose entry is current and
	// whose entry file could be read.
	entries map[string]cachedEntry
	// current are the ids of every current entry: the selected ones in
	// entries, and every other one meta.json names with a current key.
	current map[string]bool
}

// missing returns the signatures of sel without a usable entry.
func (v cacheView) missing(sel []*signatures.Signature) []*signatures.Signature {
	var out []*signatures.Signature
	for _, sig := range sel {
		if _, ok := v.entries[sig.ID]; !ok {
			out = append(out, sig)
		}
	}
	return out
}

// loadView reads the cache of a file whose identity is start.
func (s *scheduler) loadView(base, dirName string, start index.SourceIdentity, scope keyScope, cat *signatures.Catalogue,
	sel []*signatures.Signature, capSetting int) cacheView {
	empty := cacheView{entries: map[string]cachedEntry{}, current: map[string]bool{}}
	root, err := openEntryDir(base, dirName, false)
	if err != nil {
		return empty
	}
	defer func() { _ = root.Close() }()
	meta, err := s.readMeta(root)
	if err != nil || !meta.identity().Equal(start) {
		return empty
	}
	return s.viewOf(root, meta, scope, cat, sel, capSetting)
}

// viewOf is the view of a meta.json already read: the current ids, and
// the entry file of each selected signature that is current, checked
// against meta.json (its key, and as many findings as meta.json says it
// stores). An entry file that is missing, cannot be parsed or does not
// agree counts as missing, never as an error: a writer may be replacing
// it, and a scan writes it again.
func (s *scheduler) viewOf(root *os.Root, meta *metaFile, scope keyScope, cat *signatures.Catalogue,
	sel []*signatures.Signature, capSetting int) cacheView {
	view := cacheView{meta: meta, entries: map[string]cachedEntry{}, current: currentInMeta(meta, scope, cat)}
	for _, sig := range sel {
		summary, named := meta.Signatures[sig.ID]
		name, cacheable := entryName(sig.ID)
		if !named || !cacheable || summary.Key != scope.key(sig) {
			delete(view.current, sig.ID)
			continue
		}
		var entry entryFile
		err := readJSON(s.openCache, root, name, maxEntryBytes(capSetting), &entry)
		if err != nil || entry.Key != summary.Key || len(entry.Findings) != summary.Stored || int64(summary.Stored) > summary.Count {
			delete(view.current, sig.ID)
			continue
		}
		view.entries[sig.ID] = cachedEntry{result: entry.result(summary.Count), scannedAt: summary.ScannedAt}
		view.current[sig.ID] = true
	}
	return view
}

// currentInMeta returns the ids meta.json names whose key is current
// for the signature of that id in cat. An id cat does not hold is not
// current.
func currentInMeta(meta *metaFile, scope keyScope, cat *signatures.Catalogue) map[string]bool {
	current := map[string]bool{}
	for id, summary := range meta.Signatures {
		if sig, ok := cat.Lookup(id); ok && summary.Key == scope.key(sig) {
			current[id] = true
		}
	}
	return current
}

// readMeta reads meta.json of the cache directory root. A file that is
// absent or of another version is errAbsent; a link or a special file is
// errUnsafeEntry (logged where it was found); one another writer kept
// replacing while it was opened is errReplaced (a race, not logged); one
// that cannot be read or parsed is logged and returned as an error.
func (s *scheduler) readMeta(root *os.Root) (*metaFile, error) {
	var meta metaFile
	path := filepath.Join(root.Name(), metaName)
	err := readJSON(s.openCache, root, metaName, maxMetaBytes, &meta)
	if isQuietMiss(err) {
		return nil, err
	}
	if err != nil {
		slog.Default().Warn("findings_cache_unreadable", "path", path, "error", err.Error())
		return nil, err
	}
	if meta.Version != CacheVersion {
		return nil, errAbsent
	}
	if meta.Signatures == nil {
		meta.Signatures = map[string]metaEntry{}
	}
	return &meta, nil
}

// isQuietMiss reports a cache read that found nothing to use and has
// nothing more to log: the file is absent, unsafe (logged where it was
// found), or kept being replaced by another writer while it was opened.
func isQuietMiss(err error) bool {
	return errors.Is(err, errAbsent) || errors.Is(err, errUnsafeEntry) || errors.Is(err, errReplaced)
}

// commit writes the entries of a scan of the file at sourcePath, whose
// identity was start, into dir.
//
// The order is what keeps the cache consistent:
//
//  1. take the directory's lock, so no other process writes meanwhile;
//  2. write each new <id>.json (a temporary file, then a rename);
//  3. read meta.json again and keep every entry of it, when it still
//     describes the same file: another process may have added
//     signatures since this scan read it;
//  4. write meta.json (a temporary file, then a rename): the commit
//     point, after which readers see the new entries;
//  5. remove the entry files meta.json does not name (left by a crash
//     between 2 and 4, or by an entry that went stale).
//
// A crash before 4 leaves the old meta.json, which names only entry
// files that agree with it (an entry file rewritten in 2 has the same
// key and findings, or a key meta.json does not name, which a reader
// counts as missing). Entries of another process that this process sees
// as stale (other settings) are kept as they are: they are overwritten
// when this process scans that signature.
func (s *scheduler) commit(ctx context.Context, base, dirName, sourcePath string, start index.SourceIdentity, scope keyScope,
	scanned []*signatures.Signature, fresh map[string]cachedEntry) error {
	// Every operation below goes through root, the file's directory,
	// checked to be a plain directory and not a link (openEntryDir).
	root, err := openEntryDir(base, dirName, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	lock, err := lockDirectory(ctx, root)
	if err != nil {
		return err
	}
	defer lock.release()

	meta := newMeta(sourcePath, start)
	for _, sig := range scanned {
		name, cacheable := entryName(sig.ID)
		if !cacheable {
			continue // an id no loader gives; its findings are answered, not cached
		}
		e := fresh[sig.ID]
		key := scope.key(sig)
		if err := writeJSONAtomic(root, name, entryOf(key, e.result)); err != nil {
			return err
		}
		meta.Signatures[sig.ID] = metaEntry{Key: key, Count: e.result.Count, Stored: len(e.result.Findings), ScannedAt: e.scannedAt}
	}
	if s.beforeMeta != nil {
		if err := s.beforeMeta(); err != nil {
			return err
		}
	}
	if other, err := s.readMeta(root); err == nil && other.identity().Equal(start) {
		for id, summary := range other.Signatures {
			if _, replaced := meta.Signatures[id]; !replaced {
				meta.Signatures[id] = summary
			}
		}
	}
	if err := writeJSONAtomic(root, metaName, meta); err != nil {
		return err
	}
	removeUnnamed(root, meta, lock.held)
	return nil
}

// writeFailedWarned is set once a failed cache write has been logged,
// so a full disk gives one warning per process, not one per scan.
var writeFailedWarned atomic.Bool

// warnWriteFailed logs the first failed cache write of the process.
func warnWriteFailed(path string, err error) {
	// Go note: CompareAndSwap lets exactly one caller through, however
	// many fail at the same moment.
	if writeFailedWarned.CompareAndSwap(false, true) {
		slog.Default().Warn("findings_cache_write_failed", "path", path, "error", err.Error(),
			"note", "later failures are not logged; scans still answer, uncached")
	}
}

// freshEntries are the entries of the signatures a scan just read.
func freshEntries(sigs []*signatures.Signature, res *sigscan.Result, at time.Time) map[string]cachedEntry {
	out := make(map[string]cachedEntry, len(sigs))
	for _, sig := range sigs {
		out[sig.ID] = cachedEntry{result: res.BySignature[sig.ID], scannedAt: at}
	}
	return out
}

// newAnswer builds the answer of sel from the entries known, and the
// state from the ids of every current entry.
func newAnswer(cat *signatures.Catalogue, sel []*signatures.Signature, entries map[string]cachedEntry, current map[string]bool) *Answer {
	ans := &Answer{
		State: stateOf(cat, current), Checked: []string{}, Missing: []string{},
		BySignature: map[string]*sigscan.SignatureResult{}, ScannedAt: map[string]time.Time{},
	}
	for _, sig := range sel {
		e, ok := entries[sig.ID]
		if !ok {
			ans.Missing = append(ans.Missing, sig.ID)
			continue
		}
		ans.Checked = append(ans.Checked, sig.ID)
		ans.BySignature[sig.ID] = e.result
		ans.ScannedAt[sig.ID] = e.scannedAt
	}
	return ans
}

// stateOf measures the current ids against cat's default set: none
// without a current id, full when every default signature is current,
// partial otherwise. A missing opt-in signature never keeps a file
// from full; a file with only opt-in entries is partial (it was scanned,
// not with the default set).
func stateOf(cat *signatures.Catalogue, current map[string]bool) State {
	if len(current) == 0 {
		return StateNone
	}
	for _, sig := range cat.DefaultSet() {
		if !current[sig.ID] {
			return StatePartial
		}
	}
	return StateFull
}

// uniqueSignatures returns sel with each id once, in first-seen order.
func uniqueSignatures(sel []*signatures.Signature) []*signatures.Signature {
	seen := make(map[string]bool, len(sel))
	out := make([]*signatures.Signature, 0, len(sel))
	for _, sig := range sel {
		if !seen[sig.ID] {
			seen[sig.ID] = true
			out = append(out, sig)
		}
	}
	return out
}

// idSet is the set of the ids of sigs.
func idSet(sigs []*signatures.Signature) map[string]bool {
	out := make(map[string]bool, len(sigs))
	for _, sig := range sigs {
		out[sig.ID] = true
	}
	return out
}
