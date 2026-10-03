// Package cachefill fills rx's caches for many files at once: the line
// index of every file (`rx index`, POST /v1/index on a directory) and
// the signature findings of every file (`rx scan`, POST /v1/findings on
// a directory).
//
// The commands and the HTTP folder tasks go through the same three
// steps, so a folder task and the command its cli_command names do the
// same thing to the same files:
//
//   - List lists a directory by the tree's rules (paths.WalkPinned:
//     hidden entries only with --hidden, a link only where naming its
//     target would be allowed, each directory once), in the walk's
//     stable order. What the walk passes over is kept as a skip with
//     its reason.
//   - Each takes the entries one at a time, in that order, fills each
//     one through the step it is given (IndexFile or ScanFile), counts
//     the files done for a status request to read (Progress), and stops
//     between files once its context is canceled.
//   - IndexFile and ScanFile decide one file: pass it over with a
//     reason, find its cache entry current, fill it, or fail with the
//     error.
//
// A file is pinned again when it is reached (paths.Pin), as `rx index`
// always pinned it: a folder task can reach a file minutes after the
// walk listed it, and the file is judged by what it is then, under the
// same checks the walk made.
package cachefill

import (
	"context"
	"sync/atomic"

	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Entry is one file to fill: a path a walk listed, a path the caller
// named, or an entry the walk passed over (Skip).
type Entry struct {
	// Path is the file as the walk reached it (the walked directory
	// followed by entry names, links not resolved) or as the caller
	// named it.
	Path string
	// Skip is non-empty for an entry the walk did not follow (a refused
	// link, a subdirectory it could not list), and says why. Nothing is
	// read for it.
	Skip string
	// Named says the caller named the file rather than a walk finding
	// it: a named file that cannot be reached fails, a walked one is
	// passed over with the reason, as `rx index` and `rx trace` treat
	// them.
	Named bool
}

// List lists the files under dir, which the caller pinned, by the
// tree's rules (paths.WalkPinned): with recursive, every file below dir;
// without, the files directly inside it. The order is the walk's: real
// directories depth first and by name, then the directories reached
// through links. An entry the walk refused, and a subdirectory it could
// not list, are entries with Skip set.
//
// The error is non-nil only when dir itself cannot be listed.
func List(dir paths.Pinned, recursive bool) ([]Entry, error) {
	walk, err := paths.WalkPinned(dir, recursive)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(walk))
	for _, item := range walk {
		switch {
		case item.ReadErr != nil:
			// A subdirectory that cannot be listed is passed over with
			// the fixed wording of its failure, never the error's own
			// text, which names what a link leads to.
			entries = append(entries, Entry{Path: item.Path, Skip: paths.FailureReason(item.ReadErr)})
		case item.Refused != "":
			entries = append(entries, Entry{Path: item.Path, Skip: item.Refused})
		default:
			entries = append(entries, Entry{Path: item.Path})
		}
	}
	return entries, nil
}

// Outcome is what filling the cache did with one file.
type Outcome string

// The four outcomes of a file.
const (
	// Filled: the file's index was built and stored, or its findings
	// scanned and cached, by this run.
	Filled Outcome = "filled"
	// Current: the cache already held a current entry for the file (its
	// index, or the findings of every selected signature), so nothing
	// was done. Never the outcome of a forced run.
	Current Outcome = "current"
	// Skipped: the file was passed over, for Reason: the walk refused
	// it, its text is not text, it is below the index threshold, rx
	// refuses to decompress it, or a walk found it and it cannot be
	// reached.
	Skipped Outcome = "skipped"
	// Failed: the work on the file failed (Err), or the caller named it
	// and it cannot be reached (Unreadable).
	Failed Outcome = "failed"
)

// FileResult is what happened to one file.
type FileResult struct {
	// Path is the entry's path.
	Path    string
	Outcome Outcome
	// Reason says why a Skipped file was passed over, and is the error's
	// message for a Failed one.
	Reason string
	// Err is the error of a Failed file.
	Err error
	// Unreadable says a Failed file is one the caller named that could
	// not be pinned, opened or classified; Err is that failure. A
	// command words it and picks its exit code by the error (not found,
	// permission denied, not a regular file).
	Unreadable bool
	// Index and IndexPath are the line index an index run built or found
	// current, and where it is stored; nil and "" otherwise.
	Index     *rxtypes.UnifiedFileIndex
	IndexPath string
	// Answer is the findings of a scan run, scanned or found current;
	// nil otherwise.
	Answer *findings.Answer
}

// skipped is the result of a file passed over for reason.
func skipped(path, reason string) FileResult {
	return FileResult{Path: path, Outcome: Skipped, Reason: reason}
}

// failed is the result of a file whose work failed with err.
func failed(path string, err error) FileResult {
	return FileResult{Path: path, Outcome: Failed, Reason: err.Error(), Err: err}
}

// unreachable is the result of a file that cannot be pinned, opened or
// classified: a failure for a file the caller named, a skip with the
// fixed wording of the failure (paths.FailureReason) for one a walk
// found. The walk's wording never quotes the error, whose text can name
// what a link leads to.
func unreachable(e Entry, err error) FileResult {
	if !e.Named {
		return skipped(e.Path, paths.FailureReason(err))
	}
	return FileResult{Path: e.Path, Outcome: Failed, Reason: paths.FailureReason(err), Err: err, Unreadable: true}
}

// Each fills every entry through fill, one at a time and in order, and
// hands each result to record. An entry the walk passed over is
// recorded as Skipped without calling fill. progress counts the
// entries as they end (files_done of files_total) and, between files,
// forgets the bytes of the file before.
//
// Canceling ctx stops the run between files: Each checks ctx before each
// entry and returns ctx.Err() at once. A fill that ends while ctx is
// canceled (a scan stopped inside the file, or a build whose result was
// not stored) is not recorded, so a canceled run records only the files
// that ended before the cancel.
func Each(ctx context.Context, entries []Entry, progress *Progress, fill func(context.Context, Entry) FileResult, record func(FileResult)) error {
	progress.filesTotal.Store(int64(len(entries)))
	progress.started.Store(true)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress.startFile()
		if entry.Skip != "" {
			record(skipped(entry.Path, entry.Skip))
			progress.filesDone.Add(1)
			continue
		}
		result := fill(ctx, entry)
		if err := ctx.Err(); err != nil {
			return err
		}
		record(result)
		progress.filesDone.Add(1)
	}
	progress.startFile()
	return nil
}

// Progress is how far a run of Each has got: the files ended of the
// files listed, and how far the current file has been read. Each and the
// fill step write it while other goroutines (status requests) read it,
// so every member is atomic and no lock is taken.
//
// The zero Progress is ready to use and reports nothing until Each
// starts.
type Progress struct {
	filesDone, filesTotal atomic.Int64
	// started says Each has set filesTotal.
	started atomic.Bool
	// read and total are the bytes of the current file a scan has read,
	// as stored on disk (FileBytes); 0 and 0 until it reports any.
	read, total atomic.Int64
	// build is the progress of the current file's index build, nil when
	// the current file is not being built (TrackBuild).
	//
	// Go note: atomic.Pointer stores and loads a pointer as one step, so
	// a status request never sees half of a pointer the run is writing.
	build atomic.Pointer[index.Progress]
}

// Files returns the entries ended and the entries listed.
func (p *Progress) Files() (done, total int64) {
	return p.filesDone.Load(), p.filesTotal.Load()
}

// Bytes returns the bytes of the current file a scan has read and the
// file's size, both as stored on disk, or 0 and 0 while it has read
// none.
func (p *Progress) Bytes() (read, total int64) {
	return p.read.Load(), p.total.Load()
}

// FileBytes records how far the current file has been read: a scan's
// Progress callback (findings.ScanOptions.Progress) calls it as it
// reads.
func (p *Progress) FileBytes(read, total int64) {
	p.read.Store(read)
	p.total.Store(total)
}

// TrackBuild makes the current file's share read come from build, the
// progress of its index build.
func (p *Progress) TrackBuild(build *index.Progress) {
	p.build.Store(build)
}

// Fraction returns the share of the run done, from 0 to 1: the files
// ended plus the share of the current file read, over the files
// listed. It is false before Each starts. A run with no entry is done.
func (p *Progress) Fraction() (float64, bool) {
	if !p.started.Load() {
		return 0, false
	}
	done, total := p.Files()
	if total == 0 {
		return 1, true
	}
	return min((float64(done)+p.currentShare())/float64(total), 1), true
}

// currentShare is the share of the current file read: from its index
// build when one is tracked, otherwise from the bytes a scan reported.
func (p *Progress) currentShare() float64 {
	if build := p.build.Load(); build != nil {
		share, _ := build.Fraction()
		return share
	}
	read, total := p.Bytes()
	if total <= 0 {
		return 0
	}
	return min(float64(read)/float64(total), 1)
}

// startFile forgets what the file before reported.
func (p *Progress) startFile() {
	p.build.Store(nil)
	p.FileBytes(0, 0)
}

// Counts are the totals of a run: every entry is in exactly one of
// Done, Skipped and Failed, and a file whose cache was current is in
// Skipped and in Current too.
type Counts struct {
	// FilesTotal is the entries counted.
	FilesTotal int `json:"files_total"`
	// Done is the files filled by the run (Filled).
	Done int `json:"done"`
	// Skipped is the files passed over: Current and Skipped outcomes.
	Skipped int `json:"skipped"`
	// Current is how many of Skipped were passed over because their
	// cache entry was current.
	Current int `json:"current"`
	// Failed is the files whose work failed.
	Failed int `json:"failed"`
}

// countsOf is the bucket each outcome adds to, beyond FilesTotal.
var countsOf = map[Outcome]func(*Counts){
	Filled:  func(c *Counts) { c.Done++ },
	Current: func(c *Counts) { c.Skipped++; c.Current++ },
	Skipped: func(c *Counts) { c.Skipped++ },
	Failed:  func(c *Counts) { c.Failed++ },
}

// Add counts one file's result.
func (c *Counts) Add(r FileResult) {
	c.FilesTotal++
	if add, ok := countsOf[r.Outcome]; ok {
		add(c)
	}
}
