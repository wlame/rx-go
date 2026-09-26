package logchain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// partFacts is what describing a chain read about one part.
type partFacts struct {
	// indexed says that a current stored line index describes the part.
	indexed bool
	// unreadable says why the part cannot be read; empty when it can.
	unreadable string
	// lines is the part's line count, nil when not known: a frozen part
	// without an index (and without Options.Scan), or the active part
	// without one.
	lines *int64
	// timesKnown says that the part's times were read: a frozen part's
	// from its index, the active part's from its head and tail (or its
	// index). It is true for a part without timestamps too.
	timesKnown bool
	// noTimestamps says that the part has text and no line of it has a
	// timestamp rx recognizes.
	noTimestamps bool
	// format is the part's timestamp format, nil without one.
	format *rxtypes.SamplesTimeFormat
	// firstMs, lastMs and maxMs are the part's first, last and highest
	// timestamp as UTC instants; maxIsBound marks a maxMs that is an
	// upper bound (samples.IndexedTimes).
	firstMs, lastMs, maxMs *int64
	maxIsBound             bool
	// stamp is what a frozen part's index file looked like before it was
	// read, and what the index recorded of the part; nil for the active
	// part, and for a frozen part whose index file could not be stated.
	stamp *indexStamp
}

// indexStamp lets a cache hit learn, without reading an index again,
// that the facts read from it still hold: the index file is as it was
// (still absent, or the same file, unchanged), and the index still
// records the part as the listing sees it now.
type indexStamp struct {
	// present says that the index file existed.
	present bool
	// size, mtimeNs, inode and device identify the index file: a
	// rebuild writes a new file (another inode) or changes its size or
	// mtime.
	size, mtimeNs int64
	inode, device uint64
	// source is the identity the index recorded of the part; nil when
	// no index was read.
	source *index.SourceIdentity
}

// stampIndexFile stats the stored index file of part (a file in rx's
// cache directory, not one of the user's). A missing file is a stamp
// too; any other failure gives nil, and the part's facts are not kept.
func stampIndexFile(part Part) *indexStamp {
	info, err := os.Stat(index.GetCachePath(part.Path))
	if errors.Is(err, fs.ErrNotExist) {
		return &indexStamp{}
	}
	if err != nil {
		return nil
	}
	inode, device, _ := index.InodeAndDevice(info)
	return &indexStamp{present: true, size: info.Size(), mtimeNs: info.ModTime().UnixNano(), inode: inode, device: device}
}

// withSource is the stamp with the identity idx recorded of the part,
// or the stamp as it is when idx is nil. A nil stamp stays nil.
func (s *indexStamp) withSource(idx *rxtypes.UnifiedFileIndex) *indexStamp {
	if s == nil || idx == nil {
		return s
	}
	recorded := index.RecordedIdentity(idx)
	s.source = &recorded
	return s
}

// holds reports whether the stamp still holds for part, as the listing
// of this describe sees it: one stat of the index file, which must be
// as it was, and a comparison of the identity the index recorded with
// the part's stat (size, mtime, inode, device and ctime,
// index.SourceIdentity.MatchesInfo). No index and no byte of the part
// is read.
func (s *indexStamp) holds(part Part) bool {
	now := stampIndexFile(part)
	if now == nil || now.present != s.present || now.size != s.size || now.mtimeNs != s.mtimeNs ||
		now.inode != s.inode || now.device != s.device {
		return false
	}
	return s.source == nil || s.source.MatchesInfo(part.Info)
}

// stampsHold reports whether the stamp of every frozen part's cached
// facts still holds.
func stampsHold(c Candidate, facts []partFacts) bool {
	for i, part := range c.Parts {
		if part.IsActive {
			continue
		}
		if facts[i].stamp == nil || !facts[i].stamp.holds(part) {
			return false
		}
	}
	return true
}

// cacheable reports whether a frozen part's facts may be kept: they came
// from a stored index whose file was stamped, or the part is empty
// (0 bytes), whose facts follow from its size, which the cache key
// holds; its stamp still records whether it has an index.
func cacheable(part Part, f partFacts) bool {
	return f.stamp != nil && (part.Info.Size() == 0 || f.indexed)
}

// The seams describing reads a part through. Tests replace them to count
// what a describe reads; each is the production function otherwise.
var (
	// loadPartIndex loads the current stored line index of a part.
	loadPartIndex = index.LoadForPinned
	// openPart opens a part through its pin.
	openPart = func(p paths.Pinned) (*os.File, error) { return p.Open() }
	// buildPartIndex builds a part's line index in memory, by the
	// options every stored index is built with, so it equals what a
	// later `rx index` stores.
	buildPartIndex = func(path string) (*rxtypes.UnifiedFileIndex, error) {
		return index.Build(path, index.BuildOptions{})
	}
	// readTimeRange reads the active part's time range.
	readTimeRange = samples.TimeRange
)

// readFacts reads the facts of every part of c, in the candidate's
// order: the frozen parts' from the memory cache when it holds them and
// their stamps still hold, the active part's always.
func readFacts(ctx context.Context, c Candidate, opts Options) ([]partFacts, error) {
	key := cacheKey(c, opts.FileZone)
	facts, cached := descriptions.get(key, len(c.Parts))
	if cached && !stampsHold(c, facts) {
		// An index the facts came from was removed, rebuilt, or no
		// longer describes its part: the entry is dropped and the parts
		// are read again, as on a miss.
		descriptions.drop(key)
		cached = false
	}
	if !cached {
		facts = make([]partFacts, len(c.Parts))
	}
	keep := true
	for i, part := range c.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		switch {
		case part.IsActive:
			facts[i], err = activeFacts(ctx, part, opts)
		case cached:
			continue
		default:
			facts[i], err = frozenFacts(part, opts)
			keep = keep && cacheable(part, facts[i])
		}
		if err != nil {
			return nil, err
		}
	}
	if !cached && keep {
		descriptions.put(key, facts)
	}
	return facts, nil
}

// frozenFacts reads a frozen part: its stored index, or, without one, a
// check that it opens and, with opts.Scan, an index built in memory. A
// part the listing could not read is not read again: failedRead makes
// its error the reason unreadable, or a changed part when its name led
// to another file or to none.
func frozenFacts(part Part, opts Options) (partFacts, error) {
	if part.ReadError != nil {
		return failedRead(context.Background(), part, part.ReadError)
	}
	// The index file is stamped before it is read, so a change after
	// the stamp, even one during the read, shows on the next cache hit.
	stamp := stampIndexFile(part)
	stored := storedIndex(part)
	if part.Info.Size() == 0 {
		// An empty file holds no line, whatever its index says, and is
		// not opened.
		return partFacts{indexed: stored != nil, lines: new(int64), timesKnown: true, stamp: stamp.withSource(stored)}, nil
	}
	if stored != nil {
		facts := factsOfIndex(stored, opts.FileZone)
		facts.indexed = true
		facts.stamp = stamp.withSource(stored)
		return facts, nil
	}
	if facts, err := checkReadable(part); err != nil || facts.unreadable != "" {
		return facts, err
	}
	if !opts.Scan {
		return partFacts{}, nil
	}
	built, facts, err := buildInMemory(part)
	if err != nil || built == nil {
		return facts, err
	}
	return factsOfIndex(built, opts.FileZone), nil
}

// activeFacts reads the active part: its first and last timestamp from
// samples.TimeRange (from its index when one is current or built, else
// from the head of its text and a bounded read back from its end), and
// its line count and highest timestamp from that index. An active file
// the listing could not read is not read again.
func activeFacts(ctx context.Context, part Part, opts Options) (partFacts, error) {
	if part.ReadError != nil {
		return failedRead(ctx, part, part.ReadError)
	}
	stored := storedIndex(part)
	if part.Info.Size() == 0 {
		return partFacts{indexed: stored != nil, lines: new(int64), timesKnown: true}, nil
	}
	facts := partFacts{indexed: stored != nil}
	idx := stored
	if idx == nil && opts.Scan {
		built, failed, err := buildInMemory(part)
		if err != nil || built == nil {
			return failed, err
		}
		idx = built
	}
	if idx != nil {
		lines := *idx.LineCount
		facts.lines = &lines
		times := samples.TimesOfIndex(idx.TimeIndex, opts.FileZone)
		facts.maxMs, facts.maxIsBound = times.MaxMs, times.MaxIsBound
	}
	// The index found above, or none: TimeRange reads the head and the
	// tail of the text only when there is none.
	loader := func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }
	tr, err := readTimeRange(ctx, samples.Request{
		Path: part.Path, Source: part.File, FileZone: opts.FileZone, IndexLoader: loader,
	})
	if err != nil {
		return failedRead(ctx, part, err)
	}
	if tr.Format == nil {
		facts.timesKnown = true
		facts.noTimestamps = facts.lines == nil || *facts.lines > 0
		return facts, nil
	}
	detected := timestamps.Format{Family: timestamps.Family(*tr.Format), DayFirst: tr.DayFirst}
	if tr.HasZone != nil {
		detected.HasZone = *tr.HasZone
	}
	facts.format = samples.TimeFormatOf(detected, opts.FileZone)
	facts.firstMs, facts.lastMs = tr.FirstMs, tr.LastMs
	// A gzip, bzip2, xz or plain zstd active part without an index has
	// no known first timestamp (TimeRange answers source none): the
	// chain waits for its index, as it waits for a frozen part's.
	facts.timesKnown = tr.FirstMs != nil
	return facts, nil
}

// storedIndex is the part's current stored line index, or nil. An index
// that cannot be loaded, or one that records no line count, is absent:
// an index only makes the answer faster.
func storedIndex(part Part) *rxtypes.UnifiedFileIndex {
	idx, err := loadPartIndex(part.File)
	if err != nil || idx == nil || idx.LineCount == nil {
		return nil
	}
	return idx
}

// factsOfIndex is what a part's line index says about it.
func factsOfIndex(idx *rxtypes.UnifiedFileIndex, zone config.Zone) partFacts {
	lines := *idx.LineCount
	times := samples.TimesOfIndex(idx.TimeIndex, zone)
	return partFacts{
		lines: &lines, timesKnown: true,
		noTimestamps: lines > 0 && times.FirstMs == nil,
		format:       times.Format,
		firstMs:      times.FirstMs, lastMs: times.LastMs, maxMs: times.MaxMs, maxIsBound: times.MaxIsBound,
	}
}

// checkReadable opens a part through its pin and closes it, to learn
// that it can be read before the chain waits for its index: a part that
// cannot be opened gets the facts of an unreadable part, and one whose
// name leads to another file by now gives ErrPartChanged.
func checkReadable(part Part) (partFacts, error) {
	f, err := openPart(part.File)
	if err != nil {
		return failedRead(context.Background(), part, err)
	}
	_ = f.Close()
	return partFacts{}, nil
}

// buildInMemory builds a part's line index in memory, for Options.Scan.
// It returns the index, or nil with the facts of an unreadable part, or
// ErrPartChanged.
//
// SECURITY: index.Build pins the part's path again, so the file it
// read is the one that path led to at that moment. The index records
// that file's inode and device, and DescribesPinned holds them to the
// listing's pin: a name that leads to another file by now (a rotation
// renamed it, or a link was retargeted) is a changed part, never read
// as the listed one.
func buildInMemory(part Part) (*rxtypes.UnifiedFileIndex, partFacts, error) {
	built, err := buildPartIndex(part.Path)
	if err != nil {
		facts, failure := failedRead(context.Background(), part, err)
		return nil, facts, failure
	}
	if built.LineCount == nil || !index.DescribesPinned(built, part.File) {
		return nil, partFacts{}, fmt.Errorf("%w: %w: %s", ErrPartChanged, paths.ErrFileChanged, part.Path)
	}
	return built, partFacts{}, nil
}

// failedRead turns the error of a read of part into what describing
// makes of it: the context's error as it is; ErrPartChanged when the
// part's name leads to another file or to none (it changed after the
// listing); and otherwise the facts of an unreadable part, which make
// the chain invalid.
func failedRead(ctx context.Context, part Part, err error) (partFacts, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return partFacts{}, ctxErr
	}
	if errors.Is(err, paths.ErrFileChanged) || errors.Is(err, fs.ErrNotExist) {
		return partFacts{}, fmt.Errorf("%w: %w", ErrPartChanged, err)
	}
	return partFacts{unreadable: err.Error()}, nil
}
