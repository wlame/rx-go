package logchain

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ReasonDuplicatePart starts the reason a search of log chains gives for
// a file it does not read because it is another encoding of a part it
// reads (`syslog.3` beside `syslog.3.gz`): one generation of a chain is
// one part, and its other encodings hold the same lines.
const ReasonDuplicatePart = "duplicate_part"

// SearchRequest is one search of log chains: what GET /v1/logs/trace
// and `rx logs trace` ask.
type SearchRequest struct {
	// Paths are what to search, each a directory, a chain's handle or a
	// file, in the order given. The caller has checked them against the
	// search roots; every one is pinned again before it is read.
	Paths []string
	// Patterns are the regular expressions, as a trace takes them.
	Patterns []string
	// Options are the trace's options: the cap, the context, the cache
	// and index switches, the hooks. NoRecursive also limits the walk
	// that finds a directory's chains to its own files.
	Options trace.Options
}

// SearchResult is the answer of a search of log chains.
type SearchResult struct {
	// Answer is the body of GET /v1/logs/trace and of
	// `rx logs trace --json`.
	Answer *rxtypes.ChainTraceResponse
	// Trace is the trace engine's answer Answer was made from: the same
	// files, matches, context and skips, without the chains. The
	// trace_complete webhook reports it, as it reports a trace.
	Trace *rxtypes.TraceResponse
}

// SearchPathError reports a path given to Search that cannot be
// searched: one that does not exist and names no chain, one outside the
// search roots or into a hidden entry, or a file named on its own that
// cannot be opened. Err is the cause; errors.Is and errors.As see it.
type SearchPathError struct {
	Path string
	Err  error
}

// Error words the failure with the path.
func (e *SearchPathError) Error() string { return e.Path + ": " + e.Err.Error() }

// Unwrap returns the cause, for errors.Is and errors.As.
func (e *SearchPathError) Unwrap() error { return e.Err }

// Search searches log chains: GET /v1/logs/trace and `rx logs trace`.
//
// Each path is resolved in this order: a directory is walked as a trace
// walks it (paths.WalkPinned) and the files of each directory it lists
// are grouped into chains (Group); a chain's handle is that chain; any
// other path is a file, which a part's own path is. Each chain is
// described (Describe), and its parts are searched in its order: by
// time when it is ready or invalid, in the provisional order before.
//
// A chain is described from what is stored alone, never by reading a
// part: each frozen part's current line index (a part without one is
// only opened and closed, to learn that it can be read) and the head
// and tail of the active file. So a search capped by max_results reads
// what a trace of the same files reads, however large the parts. A
// chain with a frozen part not indexed yet is pending: its parts are
// searched in the provisional order, and its matches have chain_line
// -1, which the -1 rule allows ("not computed"); once the parts are
// indexed (`rx logs index`, POST /v1/logs/index) the same search gives
// every chain line. GET /v1/logs/trace and `rx logs trace` both search
// this way.
// The search itself is the trace engine's, over the resolved files in
// that order, so the file ids, the order of the matches and the cut to
// max_results follow the parts' order, and every trace rule (context,
// the trace cache, the line indexes, the hooks) applies to each part as
// to one file. Context therefore never crosses a part's edge.
//
// Left out of the search, each named in skipped_files once with its
// reason: the other encodings of a part (ReasonDuplicatePart), a part
// the listing could not read (its read error, as a trace words it), and
// whatever a trace skips. Another encoding of a part is left out also
// when it is named on its own beside its chain, whichever comes first:
// the chain reads the encoding it described, so each line comes once,
// in the chain, and no file is both searched and skipped. A chain of
// more than MaxParts parts is not read as one text: its files are
// searched as files of their own, and its entry in the answer has no
// parts.
//
// Whatever reaches them twice — a path given twice, however spelled, a
// directory and a handle or a file in it, a link to a directory, a
// directory under another spelling of its path (another case on a
// case-insensitive disk, a bind mount) — each path is resolved once,
// each chain is described once and has one entry, and each file is
// searched once, under one file id, so each match comes once. A part
// named on its own as well as through its chain is a part of the
// chain, whichever comes first. A chain is known again by its
// directory's device and inode, from the stat the listing or the walk
// checked the directory with, and by its name (chainKey); its parts
// then by their own device and inode (planAsPartOf), since another
// spelling of the directory gives them other paths. A directory without
// an inode (inode 0, as some filesystems give every one) is known by
// its path, and its chain also by its parts' paths with every link
// resolved (chainReachedAgain). Two chains that give one key all the
// same (an inode reused within one request) lose nothing: the second's
// parts that are not the first's files are searched as files of their
// own. Another encoding of a part is known by its device and inode
// too, so it is skipped under whatever path it is named. Any other file
// is known by its path with every link resolved (fileKey): two hard
// links are two files.
//
// The patterns are checked before any path is walked. The errors are
// *SearchPathError, ErrPartChanged (a part was renamed or replaced
// between the listing and its description), trace.ErrInvalidPattern,
// and the context's error.
func Search(ctx context.Context, engine *trace.Engine, req SearchRequest) (*SearchResult, error) {
	r := newSearchResolver(req)
	resp, err := engine.RunResolved(ctx, r.resolve, req.Patterns, req.Options)
	if err != nil {
		return nil, err
	}
	return &SearchResult{Answer: r.answer(resp), Trace: resp}, nil
}

// searchResolver turns the paths of a search into the files the trace
// engine reads, in order, and remembers which chain each file is a part
// of, to number the matches in the chain once the engine has answered.
//
// It lives for one search and is used by one goroutine: the engine
// calls resolve once, then Search calls answer.
type searchResolver struct {
	req  SearchRequest
	plan trace.SearchPlan
	// chains are the chains found, in the order found: chains[i] is
	// answered as c<i+1>.
	chains []*searchChain
	// partOf maps the slot of each part searched (its index in
	// plan.Files while the paths are resolved) to its place.
	partOf map[int]partPlace
	// files holds what each file classified so far is, by its path as
	// reported, so no file is opened twice for its kind: Group asks for
	// the parts' kinds, and the engine reads the same files.
	files map[string]classified
	// dirs holds the listing of each directory a handle named, so many
	// handles in one directory list it once, and each name a handle
	// names there is grouped once.
	dirs map[string]*listedDir
	// resolved holds each path already resolved, cleaned and made
	// absolute (absPath), so a path given twice is walked, described
	// and searched once.
	resolved map[string]bool
	// chainKeys maps the key (chainKey) of each chain added to the chain
	// that holds it, so a chain reached twice — through a directory and
	// its handle, through a link to its directory, or through another
	// spelling of its directory — is described once and has one entry.
	// The value is nil for a key that names a chain whose parts were all
	// skipped (chainReachedAgain).
	chainKeys map[string]*searchChain
	// planned holds what the search made of each file met so far, by
	// the path it leads to with every link resolved (fileKey): the slot
	// it is searched in, or skipped. So a file reached twice is searched
	// once, under one file id, or skipped once.
	planned map[string]plannedFile
	// byIdentity lists, for the identity (fileIdentity) of each file in
	// planned, the keys it is planned under: one per spelling of the
	// file the search met (another case of its directory, a bind mount,
	// a hard link). skipOtherEncoding looks a file up here, so another
	// encoding of a part is never searched under any spelling. Files
	// without an identity are not listed.
	byIdentity map[fileID][]string
	// encodings holds the identity of each file skipped as another
	// encoding of a part (skipOtherEncoding), so addOwnFile skips that
	// file under a path met after it too, and skipOtherEncoding looks at
	// the other paths of one file (byIdentity) once per search.
	encodings map[fileID]bool
	// withdrawn holds the slots taken back before the search runs
	// (skipOtherEncoding): another encoding of a part, planned as a file
	// of its own before its chain was found. finish leaves them out of
	// the plan.
	withdrawn map[int]bool
	// placeByID maps the file id of each part searched to its place in
	// its chain. finish fills it, once the file ids are given.
	placeByID map[string]partPlace
}

// newSearchResolver is the resolver of one search, with its maps made.
func newSearchResolver(req SearchRequest) *searchResolver {
	return &searchResolver{
		req:        req,
		partOf:     map[int]partPlace{},
		files:      map[string]classified{},
		dirs:       map[string]*listedDir{},
		resolved:   map[string]bool{},
		chainKeys:  map[string]*searchChain{},
		planned:    map[string]plannedFile{},
		byIdentity: map[fileID][]string{},
		encodings:  map[fileID]bool{},
		withdrawn:  map[int]bool{},
		placeByID:  map[string]partPlace{},
	}
}

// fileID names a file by the device and inode its stat records: the
// pair os.SameFile compares on the platforms rx builds for. Two paths
// with one fileID lead to one file, whether they spell one path two
// ways (another case of a directory on a case-insensitive disk, a bind
// mount) or are two hard links.
//
// Go note: a struct whose fields are all comparable is comparable
// itself, so it is a map key as it is, with nothing built per look-up.
type fileID struct {
	device, inode uint64
}

// fileIdentity is the identity of the file src pins, by the rule
// DirectoryIdentity applies to a directory, which holds for any file:
// ok is false when the stat gives none — no stat, a platform without
// inodes, or inode 0, which a filesystem that numbers no file gives
// every file, so that every file would be the same. A file without an
// identity is never taken for another one; it is known by its path
// alone (fileKey).
func fileIdentity(src paths.Pinned) (fileID, bool) {
	device, inode, ok := DirectoryIdentity(src.Info())
	return fileID{device: device, inode: inode}, ok
}

// plannedFile is what the search made of a file: the slot it is
// searched in (its index in plan.Files while the paths are resolved),
// or ok false when it is skipped.
type plannedFile struct {
	slot int
	ok   bool
}

// classified is what a file is, as trace.ClassifyForSearch tells it, or
// why that could not be told.
type classified struct {
	file trace.SearchFile
	err  error
}

// partPlace is where a searched part sits: its chain (an index of
// searchResolver.chains) and its place in that chain's order.
type partPlace struct {
	chain, order int
}

// searchChain is one chain a search found: its description and its
// parts searched, in its order: their slots while the paths are
// resolved, their file ids once finish has given them.
type searchChain struct {
	id    string
	desc  *Description
	slots []int
	files []string
	// partKeys maps the identity (fileIdentity) of each part addChain
	// added to the key (fileKey) it is planned under. A chain that gives
	// this chain's key again looks its parts up here (planAsPartOf): a
	// part that is the same file is this chain's part, whatever path
	// spelled it. Parts without an identity are not listed.
	partKeys map[fileID]string
}

// listedDir is the listing of one directory a handle named, and the
// chain each name a handle named there gives; err when it could not be
// listed.
type listedDir struct {
	entries []Entry
	// byName maps each entry's name to the entry.
	byName map[string]Entry
	// place maps each entry's name to its place in entries.
	place map[string]int
	// members maps a chain name to the places in entries, in the
	// listing's order, of the files whose names a template reads as
	// parts of that chain. One pass of the templates over the listing
	// fills it, so a handle's chain is grouped from its own files.
	members map[string][]int
	// chains holds what each name a handle named gives, so each name is
	// grouped once per search, a name that names no chain too.
	chains map[string]namedChain
	// info is the directory's stat as its pin found it, every link on
	// the way resolved; each chain found here carries it
	// (Candidate.DirInfo) for its key (chainKey).
	info os.FileInfo
	err  error
}

// namedChain is the chain a name gives in its directory; found is false
// when the directory holds no chain of that name.
type namedChain struct {
	candidate Candidate
	found     bool
}

// resolve is the trace.Resolver of the search: it resolves every path
// and returns the plan. The engine calls it once, after the patterns
// are checked.
func (r *searchResolver) resolve(ctx context.Context) (*trace.SearchPlan, error) {
	// The answer's path lists the paths as the request gave them, a
	// repeated one too; the plan holds what they lead to once.
	r.plan.Paths = r.req.Paths
	for _, p := range r.req.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := absPath(p)
		if r.resolved[key] {
			continue
		}
		r.resolved[key] = true
		if err := r.addPath(ctx, p); err != nil {
			return nil, err
		}
	}
	r.finish()
	return &r.plan, nil
}

// finish gives the files of the plan their ids, once no path can change
// the plan any more: the engine answers plan.Files[i] as f<i+1>, so a
// file taken back while the paths were resolved (withdrawn) is left out
// here, and the files after it move up. Each chain's parts, and each
// part's place, are then named by file id.
//
// INVARIANT: a slot that holds a part of a chain is never withdrawn
// (skipOtherEncoding takes back only a file of its own), so every part
// keeps an id.
func (r *searchResolver) finish() {
	ids := make([]string, len(r.plan.Files))
	// Go note: kept shares plan.Files' array; each file is written at an
	// index no later than the one it is read from, so the filter needs
	// no second slice.
	kept := r.plan.Files[:0]
	for slot, f := range r.plan.Files {
		if r.withdrawn[slot] {
			continue
		}
		kept = append(kept, f)
		ids[slot] = "f" + strconv.Itoa(len(kept))
	}
	r.plan.Files = kept
	for slot, place := range r.partOf {
		r.placeByID[ids[slot]] = place
	}
	for _, ch := range r.chains {
		for _, slot := range ch.slots {
			ch.files = append(ch.files, ids[slot])
		}
	}
}

// addPath resolves one path: a directory, else a chain's handle, else a
// file. A handle need not exist as a file (a chain of dated parts only
// has no active file), so a path that does not exist is still looked
// up as a handle before it is refused.
func (r *searchResolver) addPath(ctx context.Context, p string) error {
	pinned, err := paths.Pin(p)
	switch {
	case err == nil && pinned.Info().IsDir():
		return r.addDirectory(ctx, p, pinned)
	case err == nil || errors.Is(err, fs.ErrNotExist):
		found, chainErr := r.addHandle(ctx, p)
		if chainErr != nil || found {
			return chainErr
		}
		if err != nil {
			return &SearchPathError{Path: p, Err: err}
		}
		return r.addNamedFile(p, pinned)
	default:
		return &SearchPathError{Path: p, Err: err}
	}
}

// addHandle searches the chain the handle p names, when it names one:
// its directory, listed once per search, holds a chain of that name,
// grouped from that name's files alone (chainNamed). found is false,
// with no error, for a path that names no chain: one that is not a bare
// name in a directory, a directory that cannot be listed or holds no
// such chain. A file named on its own takes this path first, so naming
// a file classifies nothing but the files of a chain of its name. The
// error is Describe's.
func (r *searchResolver) addHandle(ctx context.Context, p string) (found bool, err error) {
	dir, name, err := splitHandle(p)
	if err != nil {
		return false, nil
	}
	listed := r.listDir(dir)
	if listed.err != nil {
		return false, nil
	}
	c, ok := r.chainNamed(dir, listed, name)
	if !ok {
		return false, nil
	}
	if err := r.addChain(ctx, c, listed.byName); err != nil {
		return true, err
	}
	if c.TooManyParts {
		// Not read as one text: each file of the chain's name is
		// searched as a file of its own, in the listing's order.
		for _, e := range listed.chainEntries(c.Name) {
			if isCandidateFile(e) {
				r.addOwnFile(e.File)
			}
		}
	}
	return true, nil
}

// chainNamed is the chain named name in the listed directory dir, or
// found false when it holds none. It is grouped as Resolve groups a
// handle's chain (groupNamed), from the files that can belong to it
// alone: only they are classified, never the rest of the directory, so
// a file named beside thousands of another chain's parts costs one
// classification, its own. Each name is grouped once per search.
func (r *searchResolver) chainNamed(dir string, listed *listedDir, name string) (c Candidate, found bool) {
	if named, ok := listed.chains[name]; ok {
		return named.candidate, named.found
	}
	var named namedChain
	for _, candidate := range groupNamed(dir, listed.chainEntries(name), r.classify, name) {
		if candidate.Name == name {
			candidate.DirInfo = listed.info
			named = namedChain{candidate: candidate, found: true}
		}
	}
	listed.chains[name] = named
	return named.candidate, named.found
}

// chainEntries are the entries of the listing that can belong to the
// chain named name, in the listing's order: the file named like the
// chain (its active file) and the files a template reads as its parts.
// The work is a map look-up and the chain's own files, whatever the
// size of the directory.
func (l *listedDir) chainEntries(name string) []Entry {
	places := slices.Clone(l.members[name])
	if i, ok := l.place[name]; ok {
		places = append(places, i)
		slices.Sort(places)
	}
	entries := make([]Entry, 0, len(places))
	for _, i := range places {
		entries = append(entries, l.entries[i])
	}
	return entries
}

// listDir lists the directory dir once per search, through its pin, and
// reads the names of its entries once with the templates (matchName),
// to know which files can belong to which chain. No entry is opened:
// chainNamed classifies a chain's files when a handle names it.
//
// SECURITY: dir is checked against the search roots and pinned before
// it is listed, as Resolve does; the listing refuses a link that leads
// out of the roots or into a hidden entry, and every entry keeps its
// pin.
func (r *searchResolver) listDir(dir string) *listedDir {
	if listed, ok := r.dirs[dir]; ok {
		return listed
	}
	listed := &listedDir{chains: map[string]namedChain{}}
	r.dirs[dir] = listed
	if _, err := validateDir(dir); err != nil {
		listed.err = err
		return listed
	}
	pinned, err := paths.Pin(dir)
	if err == nil && !pinned.Info().IsDir() {
		err = ErrNotAChain
	}
	if err != nil {
		listed.err = err
		return listed
	}
	entries, err := paths.ListDir(pinned)
	if err != nil {
		listed.err = err
		return listed
	}
	listed.info = pinned.Info()
	listed.entries = EntriesOf(entries)
	listed.byName = entriesByName(listed.entries)
	listed.place = make(map[string]int, len(listed.entries))
	listed.members = map[string][]int{}
	for i, e := range listed.entries {
		listed.place[e.Name] = i
		if !isCandidateFile(e) {
			continue
		}
		if m, ok := matchName(e.Name); ok {
			listed.members[m.chain] = append(listed.members[m.chain], i)
		}
	}
	return listed
}

// addDirectory searches a directory: its walk, as a trace walks it,
// with the files of each directory listed grouped into chains. A chain
// takes the place of the first of its files the walk lists, and its
// parts follow there in the chain's order; every other file keeps its
// place.
func (r *searchResolver) addDirectory(ctx context.Context, p string, pinned paths.Pinned) error {
	r.plan.ScannedDirs = append(r.plan.ScannedDirs, p)
	walked, err := walkSearchedDirectory(pinned, !r.req.Options.NoRecursive)
	if err != nil {
		// The directory itself cannot be listed: named, as a trace
		// names it, rather than answered as empty.
		r.skip(p, trace.SkipReason(err))
		return nil
	}
	var files []Entry
	byDir := map[string][]Entry{}
	// dirInfo holds, for each directory the walk listed, the stat the
	// walk checked its listing against (WalkEntry.Dir): the identity
	// its chains are keyed by.
	dirInfo := map[string]os.FileInfo{}
	for _, we := range walked {
		switch {
		case we.ReadErr != nil:
			r.skip(we.Path, trace.SkipReason(we.ReadErr))
		case we.Refused != "":
			r.skip(we.Path, we.Refused)
		default:
			e := Entry{Name: filepath.Base(we.Path), Path: we.Path, Info: we.File.Info(), File: we.File}
			files = append(files, e)
			dir := filepath.Dir(we.Path)
			byDir[dir] = append(byDir[dir], e)
			// Every file of one listing carries the same directory pin.
			dirInfo[dir] = we.Dir.Info()
		}
	}

	// Each directory the walk listed is grouped on its own: a chain
	// lives in one directory. owner maps the path of each file of a
	// chain (a part, or another encoding of one) to its chain.
	owner := map[string]*walkChain{}
	for dir, entries := range byDir {
		byName := entriesByName(entries)
		chains := Group(dir, entries, r.classify)
		if len(chains) == 0 {
			continue
		}
		for _, c := range chains {
			// SECURITY: the key comes from the pin the walk listed the
			// directory through, never from pinning its path again: by
			// then the path may lead to another directory (swapped for a
			// link to another chain's), whose key would hide this chain.
			c.DirInfo = dirInfo[dir]
			wc := &walkChain{candidate: c, byName: byName}
			for _, part := range c.Parts {
				owner[part.Path] = wc
				for _, dup := range part.Duplicates {
					owner[byName[dup].Path] = wc
				}
			}
		}
	}

	for _, e := range files {
		wc := owner[e.Path]
		if wc == nil {
			r.addOwnFile(e.File)
			continue
		}
		if !wc.found {
			wc.found = true
			if err := r.addChain(ctx, wc.candidate, wc.byName); err != nil {
				return err
			}
		}
		if wc.candidate.TooManyParts {
			// Not read as one text: each of its files is searched where
			// the walk lists it, as a file of its own.
			r.addOwnFile(e.File)
		}
	}
	return nil
}

// walkSearchedDirectory is the walk addDirectory searches a directory
// with: paths.WalkPinned. A test replaces it to change the tree right
// after the walk has listed it (a directory swapped for a link), which
// no tree on disk can be made to do on demand.
var walkSearchedDirectory = paths.WalkPinned

// walkChain is a chain a directory walk found, and whether the walk has
// met its first file yet.
type walkChain struct {
	candidate Candidate
	// byName maps the name of each file of the chain's directory to its
	// entry, under the walk's spelling.
	byName map[string]Entry
	found  bool
}

// entriesByName maps each entry's name to the entry.
func entriesByName(entries []Entry) map[string]Entry {
	byName := make(map[string]Entry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	return byName
}

// chainKey names a chain by the device and inode of its directory and
// by its name, as the index tasks of chains are keyed, so every path
// that leads to one directory — the same path spelled twice, a handle
// and a walk, a link to the directory — gives one key. Without an
// identity of the directory (DirectoryIdentity: no stat, a platform
// without inodes, or inode 0) the key is the handle, made absolute.
//
// One key is not always one chain read one way. The same directory
// under two spellings (another case on a case-insensitive disk, a bind
// mount) gives one key from parts whose paths differ; addChain then
// knows each part by its identity (planAsPartOf), so each is searched
// once, in the chain. Two different chains can also give one key (an
// inode reused within one request); addChain then searches the
// second's parts as files of their own, so a clash costs the chain
// fields of those matches, never the matches. A directory without an
// identity is keyed by its handle, so a link to it gives a second key;
// chainReachedAgain then knows the chain by its parts' paths.
func chainKey(c Candidate) string {
	if device, inode, ok := DirectoryIdentity(c.DirInfo); ok {
		return strconv.FormatUint(device, 10) + ":" + strconv.FormatUint(inode, 10) + "/" + c.Name
	}
	return "path:" + absPath(c.Handle())
}

// absPath is p cleaned and made absolute, or only cleaned when the
// working directory cannot be read.
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// addChain describes a chain, names it in the answer, and adds its
// parts to the search in the chain's order. A part the listing could
// not read is not searched; it is skipped with its read error. Another
// encoding of a part is skipped with ReasonDuplicatePart
// (skipOtherEncoding). A chain of more than MaxParts parts gets its
// entry and adds no part: its caller searches its files on their own.
//
// A chain whose key (chainKey) a chain added before holds is not
// described again and gets no entry; its parts go to addPartsAsFiles.
// For the same chain reached a second way that adds nothing: a part the
// holder of the key has, as the same file (planAsPartOf) or under the
// same path (addFile), is searched once already, under its file id and
// in its chain — so one directory under two spellings is one chain.
// For another chain that gives the same key it searches that chain's
// parts with no chain, rather than losing them.
//
// A chain in a directory without an identity is keyed by its handle,
// so the same chain reached through a link to its directory gives
// another key; it is known by its parts' paths instead
// (chainReachedAgain), and gets no second, empty entry.
//
// byName holds the entries the chain was grouped from, by name: the
// other encodings of its parts are looked up there.
//
// The error is Describe's: ErrPartChanged, or the context's.
func (r *searchResolver) addChain(ctx context.Context, c Candidate, byName map[string]Entry) error {
	key := chainKey(c)
	if holder, held := r.chainKeys[key]; held {
		r.addPartsAsFiles(c, holder, byName)
		return nil
	}
	if holder, again := r.chainReachedAgain(c); again {
		r.chainKeys[key] = holder
		return nil
	}
	// No Scan: a part without a line index is not read to describe the
	// chain (see Search), so the description costs index loads, an open
	// of each such part and the active file's head and tail.
	d, err := Describe(ctx, c, Options{})
	if err != nil {
		return err
	}
	ch := &searchChain{
		id: "c" + strconv.Itoa(len(r.chains)+1), desc: d, files: []string{},
		partKeys: make(map[fileID]string, len(d.Order)),
	}
	r.chainKeys[key] = ch
	index := len(r.chains)
	r.chains = append(r.chains, ch)
	for order, part := range d.Parts() {
		// A part the listing could not read was classified with its
		// error (r.classify, which Group asked), so addFile skips it with
		// that error's reason, once.
		slot, ok := r.addFile(part.File)
		// INVARIANT: a file is a part of one chain at most, the first to
		// claim it, so each of its matches is placed once. A file
		// planned on its own before (a part's own path named first) is
		// claimed by its chain, in the slot it already has.
		if _, claimed := r.partOf[slot]; ok && !claimed {
			ch.slots = append(ch.slots, slot)
			r.partOf[slot] = partPlace{chain: index, order: order}
		}
		if id, hasID := fileIdentity(part.File); hasID {
			ch.partKeys[id] = fileKey(part.File)
		}
		for _, name := range part.Duplicates {
			// byName holds every entry the chain was grouped from, its
			// parts' other encodings too.
			r.skipOtherEncoding(byName[name].File, part.Name)
		}
	}
	return nil
}

// addPartsAsFiles adds the parts of the chain c, whose key the chain
// holder holds already, to the search, in c's provisional order, and
// skips their other encodings as addChain does; see addChain for when.
//
// A part that is the same file as a part of holder (planAsPartOf) is
// that part: searched already, in holder. Any other part is a file of
// its own (addFile), and a file planned or skipped already keeps what
// it got there. So for the chain holder reached a second way — under
// the same path, through a link, or under another spelling of its
// directory — this plans and names nothing new. A chain of more than
// MaxParts parts adds nothing here: its caller searches every file of
// its name.
//
// holder is nil when the key names a chain none of whose parts is
// searched (chainReachedAgain); every part then goes to addOwnFile.
func (r *searchResolver) addPartsAsFiles(c Candidate, holder *searchChain, byName map[string]Entry) {
	if c.TooManyParts {
		return
	}
	for _, part := range c.Parts {
		if !r.planAsPartOf(holder, part.File) {
			r.addOwnFile(part.File)
		}
		for _, name := range part.Duplicates {
			r.skipOtherEncoding(byName[name].File, part.Name)
		}
	}
}

// planAsPartOf reports whether src is the same file as a part of the
// chain holder: the same device and inode (os.SameFile's test), the
// inode not 0. Then src's path is recorded with what the search made
// of that part, so src is searched once, as that part, under the
// part's file id, and a later look-up of src's path finds it.
//
// Holder and src's chain give one key (the same directory and chain
// name), so a part that is the same file is the same part reached
// through another spelling of the directory: another case of it on a
// case-insensitive disk, or a bind mount of it. Their paths differ even
// with every link resolved, so fileKey alone cannot tell.
//
// SECURITY: the comparison is by the identities the pins recorded when
// they were checked, never by a new look-up of either path. A part is
// read only through its own pin, so taking it for holder's part reads
// nothing more and nothing else.
func (r *searchResolver) planAsPartOf(holder *searchChain, src paths.Pinned) bool {
	if holder == nil {
		return false
	}
	id, ok := fileIdentity(src)
	if !ok {
		return false
	}
	partKey, same := holder.partKeys[id]
	if !same {
		return false
	}
	if key := fileKey(src); key != partKey {
		if _, seen := r.planned[key]; !seen {
			r.record(src, key, r.planned[partKey])
		}
	}
	return true
}

// chainReachedAgain reports whether c, a chain in a directory without
// an identity (DirectoryIdentity: inode 0, or none), is a chain the
// search added already, reached again through a link to its directory:
// every one of its parts is planned already under the path it leads to
// with every link resolved (fileKey), as a part of a chain or skipped,
// so c would add an entry with no part. holder is the chain that holds
// the first of those parts, nil when every part was skipped.
//
// A directory with an identity is known again by its key (chainKey),
// so it is not looked at here; neither is a chain one of whose parts is
// not planned yet, or is planned as a file of its own, which c then
// claims as its part.
func (r *searchResolver) chainReachedAgain(c Candidate) (holder *searchChain, again bool) {
	if _, _, ok := DirectoryIdentity(c.DirInfo); ok || len(c.Parts) == 0 {
		return nil, false
	}
	for _, part := range c.Parts {
		planned, seen := r.planned[fileKey(part.File)]
		if !seen {
			return nil, false
		}
		if !planned.ok {
			continue
		}
		place, claimed := r.partOf[planned.slot]
		if !claimed {
			return nil, false
		}
		if holder == nil {
			holder = r.chains[place.chain]
		}
	}
	return holder, true
}

// skipOtherEncoding keeps another encoding of the part named part out of
// the search and names it once in skipped_files, with
// ReasonDuplicatePart: it holds the lines of the part its chain reads,
// so searching it too would give each of them twice.
//
// The search may have met the file before its chain. Under its own
// path: skipped already (as another encoding, or because it cannot be
// read), it is not named twice; claimed as a part of another chain (a
// link into that chain's directory), it stays that chain's part;
// planned as a file of its own (named on its own, as `app.log.1.gz`
// beside the handle `app.log`), it is taken back (withdrawn) before the
// search runs, so the file is skipped rather than both searched and
// skipped. Under another path that leads to the same file (byIdentity:
// another case of its directory, a bind mount, a hard link), each path
// it is planned under as a file of its own is withdrawn too, and the
// file is named once: not again when one of those paths, or an earlier
// call for the same file, named it already. A path that holds a part of
// another chain is left as it is: a chain is searched with every part
// it describes (addFile). Met after its chain, under any path, the file
// is skipped already, and addOwnFile does not plan it (encodings).
//
// Each file is looked up under its other paths once per search: a later
// call for the same file finds it in encodings and records its own path
// alone, so the cost is one pass over the paths of each file however
// many chains name it as another encoding.
func (r *searchResolver) skipOtherEncoding(other paths.Pinned, part string) {
	key := fileKey(other)
	if planned, seen := r.planned[key]; seen {
		if !planned.ok {
			return
		}
		if _, claimed := r.partOf[planned.slot]; claimed {
			return
		}
		r.withdrawn[planned.slot] = true
	}
	id, hasID := fileIdentity(other)
	if hasID && r.encodings[id] {
		// Skipped as another encoding under another path already, and
		// named then.
		r.record(other, key, plannedFile{})
		return
	}
	named := false
	if hasID {
		for _, k := range r.byIdentity[id] {
			planned := r.planned[k]
			switch _, claimed := r.partOf[planned.slot]; {
			case k == key:
				// other's own path, handled above.
			case !planned.ok:
				named = true
			case !claimed:
				r.withdrawn[planned.slot] = true
				r.record(other, k, plannedFile{})
			}
		}
		r.encodings[id] = true
	}
	r.record(other, key, plannedFile{})
	if !named {
		r.skip(other.Path(), duplicateReason(part))
	}
}

// duplicateReason is the reason given for another encoding of the part
// named part.
func duplicateReason(part string) string {
	return ReasonDuplicatePart + ": the same part of its log chain as " + part + ", which is searched"
}

// addNamedFile searches a file named on its own. Like a trace, it
// refuses a named file that cannot be opened, rather than answer "no
// match" for a file nobody could read, and skips one that is not text.
func (r *searchResolver) addNamedFile(p string, pinned paths.Pinned) error {
	f, err := pinned.Open()
	if err != nil {
		return &SearchPathError{Path: p, Err: err}
	}
	_ = f.Close()
	r.addOwnFile(pinned)
	return nil
}

// addFile adds a file to the search, in the order called, and returns
// its slot (its index in plan.Files until finish gives the file ids);
// ok is false for a file that is skipped instead: one that cannot be
// read or is not text, named with the reason a trace gives, or another
// encoding of a part already skipped (skipOtherEncoding). A file met
// before, under any spelling of its path (fileKey), is not added again:
// the answer is the one it got then. A file is known here by its path
// alone, so two hard links are two files: two chains whose parts are
// hard links of each other's are both searched in full.
func (r *searchResolver) addFile(src paths.Pinned) (slot int, ok bool) {
	key := fileKey(src)
	if planned, seen := r.planned[key]; seen {
		return planned.slot, planned.ok
	}
	slot, ok = r.planFile(src)
	r.record(src, key, plannedFile{slot: slot, ok: ok})
	return slot, ok
}

// addOwnFile adds a file of its own to the search: one named on its
// own, one a walk lists outside any chain, a file of a chain of more
// than MaxParts parts, or a part of a chain whose key another holds.
// It is addFile, except that the same file as another encoding of a
// part skipped already (encodings), met under another path — another
// case of its directory, a bind mount, a hard link — is skipped too,
// and not named again: its lines are the part's, which its chain
// searches, so searching it would give each of them twice.
//
// A part of a chain goes to addFile instead: a chain is searched with
// every part it describes, whatever other file one of them is.
func (r *searchResolver) addOwnFile(src paths.Pinned) {
	key := fileKey(src)
	if _, seen := r.planned[key]; !seen {
		if id, hasID := fileIdentity(src); hasID && r.encodings[id] {
			r.record(src, key, plannedFile{})
			return
		}
	}
	r.addFile(src)
}

// record sets what the search made of the file src, known by key (its
// fileKey), and lists key under src's identity (byIdentity) the first
// time key is recorded, so skipOtherEncoding finds the file under every
// path the search met it by. Every write to planned goes through here,
// so the two maps never disagree.
func (r *searchResolver) record(src paths.Pinned, key string, p plannedFile) {
	if _, seen := r.planned[key]; !seen {
		if id, ok := fileIdentity(src); ok {
			r.byIdentity[id] = append(r.byIdentity[id], key)
		}
	}
	r.planned[key] = p
}

// fileKey names the file a pin leads to by its path with every link
// resolved (paths.Pinned.Canonical): the same for every spelling of one
// path and through a link to its directory, and different for two hard
// links, which are two names a person can mean apart.
func fileKey(src paths.Pinned) string {
	if canonical := src.Canonical(); canonical != "" {
		return canonical
	}
	return absPath(src.Path())
}

// planFile classifies a file and adds it to the plan, or skips it with
// its reason; see addFile.
func (r *searchResolver) planFile(src paths.Pinned) (slot int, ok bool) {
	c := r.classified(src)
	switch {
	case c.err != nil:
		r.skip(src.Path(), trace.SkipReason(c.err))
		return 0, false
	case !c.file.Kind().IsText():
		r.skip(src.Path(), c.file.Kind().NotText)
		return 0, false
	}
	r.plan.Files = append(r.plan.Files, c.file)
	return len(r.plan.Files) - 1, true
}

// classify is the Classify a search groups with: the file's kind as the
// search reads it, from classified.
func (r *searchResolver) classify(e Entry) (filekind.Kind, error) {
	c := r.classified(e.File)
	return c.file.Kind(), c.err
}

// classified classifies the file src leads to once per search, through
// its pin (trace.ClassifyForSearch), and returns what that said.
func (r *searchResolver) classified(src paths.Pinned) classified {
	if c, ok := r.files[src.Path()]; ok {
		return c
	}
	file, err := trace.ClassifyForSearch(src)
	c := classified{file: file, err: err}
	r.files[src.Path()] = c
	return c
}

// skip names a path the search passes over, with the reason.
func (r *searchResolver) skip(path, reason string) {
	r.plan.Skipped = append(r.plan.Skipped, rxtypes.SkippedFile{Path: path, Reason: reason})
}

// answer makes the chain answer from the engine's: the same fields,
// each match with its chain and chain line, and the chains found.
func (r *searchResolver) answer(resp *rxtypes.TraceResponse) *rxtypes.ChainTraceResponse {
	out := &rxtypes.ChainTraceResponse{
		RequestID:     resp.RequestID,
		Path:          resp.Path,
		Time:          resp.Time,
		Patterns:      resp.Patterns,
		Files:         resp.Files,
		Matches:       make([]rxtypes.ChainMatch, 0, len(resp.Matches)),
		ScannedFiles:  resp.ScannedFiles,
		SkippedFiles:  resp.SkippedFiles,
		SkipReasons:   resp.SkipReasons,
		MaxResults:    resp.MaxResults,
		FileChunks:    resp.FileChunks,
		ContextLines:  resp.ContextLines,
		BeforeContext: resp.BeforeContext,
		AfterContext:  resp.AfterContext,
		CLICommand:    resp.CLICommand,
		Chains:        make(map[string]rxtypes.ChainRef, len(r.chains)),
	}
	for _, m := range resp.Matches {
		out.Matches = append(out.Matches, r.chainMatch(m))
	}
	for _, ch := range r.chains {
		out.Chains[ch.id] = rxtypes.ChainRef{
			Path:        ch.desc.Response.Path,
			Name:        ch.desc.Response.Name,
			Parts:       ch.files,
			Fingerprint: ch.desc.Response.Fingerprint,
			State:       ch.desc.Response.State,
			Reasons:     ch.desc.Response.Reasons,
		}
	}
	return out
}

// chainMatch is a match with its chain and its chain line: the id of
// the chain whose part it is in, or nil for a file of its own; and its
// global line, the part's start plus its line in the part less one, or
// -1 when that is not known (the chain is not ready, or the trace left
// the line unnumbered).
func (r *searchResolver) chainMatch(m rxtypes.Match) rxtypes.ChainMatch {
	place, ok := r.placeByID[m.File]
	if !ok {
		return rxtypes.ChainMatchOf(m, nil, -1)
	}
	ch := r.chains[place.chain]
	line := int64(-1)
	// Starts holds each part's global start (its first line's global
	// number), and only once the chain is ready.
	if starts := ch.desc.Starts; starts != nil && m.AbsoluteLineNumber >= 1 {
		line = starts[place.order] + int64(m.AbsoluteLineNumber) - 1
	}
	id := ch.id
	return rxtypes.ChainMatchOf(m, &id, line)
}
