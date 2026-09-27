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
	"github.com/wlame/rx-go/internal/index"
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
// Left out of the search, each named in skipped_files with its reason:
// the other encodings of a part (ReasonDuplicatePart), a part the
// listing could not read (its read error, as a trace words it), and
// whatever a trace skips. A chain of more than MaxParts parts is not
// read as one text: its files are searched as files of their own, and
// its entry in the answer has no parts.
//
// Whatever reaches them twice — a path given twice, however spelled, a
// directory and a handle or a file in it, a link to a directory — each
// path is resolved once, each chain is described once and has one
// entry, and each file is searched once, under one file id, so each
// match comes once. A part named on its own as well as through its
// chain is a part of the chain, whichever comes first.
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
	// partOf maps the file id of each part searched to its place.
	partOf map[string]partPlace
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
	// chainKeys holds the key (chainKey) of each chain added, so a chain
	// reached twice — through a directory and its handle, or through a
	// link to its directory — is described once and has one entry.
	chainKeys map[string]bool
	// planned holds what addFile made of each file met so far, by the
	// path it leads to with every link resolved (fileKey), so a file
	// reached twice is searched once, under one file id.
	planned map[string]plannedFile
}

// newSearchResolver is the resolver of one search, with its maps made.
func newSearchResolver(req SearchRequest) *searchResolver {
	return &searchResolver{
		req:       req,
		partOf:    map[string]partPlace{},
		files:     map[string]classified{},
		dirs:      map[string]*listedDir{},
		resolved:  map[string]bool{},
		chainKeys: map[string]bool{},
		planned:   map[string]plannedFile{},
	}
}

// plannedFile is what addFile made of a file: the file id it is
// searched under, or ok false when it was skipped.
type plannedFile struct {
	id string
	ok bool
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

// searchChain is one chain a search found: its description and the
// file ids of its parts searched, in its order.
type searchChain struct {
	id    string
	desc  *Description
	files []string
}

// listedDir is the listing of one directory a handle named, and the
// chain each name a handle named there gives; err when it could not be
// listed.
type listedDir struct {
	entries []Entry
	// names maps each entry's name to its path as listed.
	names map[string]string
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
	return &r.plan, nil
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
	if err := r.addChain(ctx, c, listed.names); err != nil {
		return true, err
	}
	if c.TooManyParts {
		// Not read as one text: each file of the chain's name is
		// searched as a file of its own, in the listing's order.
		for _, e := range listed.chainEntries(c.Name) {
			if isCandidateFile(e) {
				r.addFile(e.File)
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
	listed.names = pathsByName(listed.entries)
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
	walked, err := paths.WalkPinned(pinned, !r.req.Options.NoRecursive)
	if err != nil {
		// The directory itself cannot be listed: named, as a trace
		// names it, rather than answered as empty.
		r.skip(p, trace.SkipReason(err))
		return nil
	}
	var files []Entry
	byDir := map[string][]Entry{}
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
		}
	}

	// Each directory the walk listed is grouped on its own: a chain
	// lives in one directory. owner maps the path of each file of a
	// chain (a part, or another encoding of one) to its chain.
	owner := map[string]*walkChain{}
	for dir, entries := range byDir {
		names := pathsByName(entries)
		chains := Group(dir, entries, r.classify)
		if len(chains) == 0 {
			continue
		}
		dirInfo := directoryInfo(dir)
		for _, c := range chains {
			c.DirInfo = dirInfo
			wc := &walkChain{candidate: c, names: names}
			for _, part := range c.Parts {
				owner[part.Path] = wc
				for _, dup := range part.Duplicates {
					owner[names[dup]] = wc
				}
			}
		}
	}

	for _, e := range files {
		wc := owner[e.Path]
		if wc == nil {
			r.addFile(e.File)
			continue
		}
		if !wc.found {
			wc.found = true
			if err := r.addChain(ctx, wc.candidate, wc.names); err != nil {
				return err
			}
		}
		if wc.candidate.TooManyParts {
			// Not read as one text: each of its files is searched where
			// the walk lists it, as a file of its own.
			r.addFile(e.File)
		}
	}
	return nil
}

// walkChain is a chain a directory walk found, and whether the walk has
// met its first file yet.
type walkChain struct {
	candidate Candidate
	// names maps the name of each file of the chain's directory to its
	// path as the walk spelled it.
	names map[string]string
	found bool
}

// pathsByName maps each entry's name to its path as listed.
func pathsByName(entries []Entry) map[string]string {
	names := make(map[string]string, len(entries))
	for _, e := range entries {
		names[e.Name] = e.Path
	}
	return names
}

// directoryInfo is the stat of the directory dir as a pin finds it,
// every link on the way resolved, for the key of the chains a walk
// found in it (chainKey); nil when it cannot be pinned, and those
// chains are keyed by their handles.
//
// SECURITY: dir is a directory the walk listed, so the pin checks
// again what the walk checked (the search roots, hidden entries); it
// states the directory and reads nothing in it. It runs once per
// directory that holds a chain.
func directoryInfo(dir string) os.FileInfo {
	pinned, err := paths.Pin(dir)
	if err != nil || !pinned.Info().IsDir() {
		return nil
	}
	return pinned.Info()
}

// chainKey names a chain by the device and inode of its directory and
// by its name, as the index tasks of chains are keyed, so every path
// that leads to one directory — the same path spelled twice, a handle
// and a walk, a link to the directory — gives one key. Without the
// directory's stat (Candidate.DirInfo) or an inode the key is the
// handle, made absolute.
func chainKey(c Candidate) string {
	if c.DirInfo != nil {
		if inode, device, ok := index.InodeAndDevice(c.DirInfo); ok {
			return strconv.FormatUint(device, 10) + ":" + strconv.FormatUint(inode, 10) + "/" + c.Name
		}
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
// encoding of a part is skipped with ReasonDuplicatePart. A chain of
// more than MaxParts parts gets its entry and adds no part: its caller
// searches its files on their own. A chain added already (chainKey)
// adds nothing: it keeps its one entry, and its parts their places.
//
// The error is Describe's: ErrPartChanged, or the context's.
func (r *searchResolver) addChain(ctx context.Context, c Candidate, names map[string]string) error {
	key := chainKey(c)
	if r.chainKeys[key] {
		return nil
	}
	r.chainKeys[key] = true
	// No Scan: a part without a line index is not read to describe the
	// chain (see Search), so the description costs index loads, an open
	// of each such part and the active file's head and tail.
	d, err := Describe(ctx, c, Options{})
	if err != nil {
		return err
	}
	ch := &searchChain{id: "c" + strconv.Itoa(len(r.chains)+1), desc: d, files: []string{}}
	index := len(r.chains)
	r.chains = append(r.chains, ch)
	for order, part := range d.Parts() {
		if part.ReadError != nil {
			r.skip(part.Path, trace.SkipReason(part.ReadError))
			continue
		}
		id, ok := r.addFile(part.File)
		// INVARIANT: a file is a part of one chain at most, the first to
		// claim it, so each of its matches is placed once. A file
		// planned on its own before (a part's own path named first) is
		// claimed by its chain, under the file id it already has.
		if _, claimed := r.partOf[id]; ok && !claimed {
			ch.files = append(ch.files, id)
			r.partOf[id] = partPlace{chain: index, order: order}
		}
		for _, dup := range part.Duplicates {
			r.skip(names[dup], duplicateReason(part.Name))
		}
	}
	return nil
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
	r.addFile(pinned)
	return nil
}

// addFile adds a file to the search, in the order called, and returns
// its file id; ok is false for a file that is skipped instead: one that
// cannot be read or is not text, named with the reason a trace gives. A
// file met before, under any spelling (fileKey), is not added again: the
// answer is the one it got then.
func (r *searchResolver) addFile(src paths.Pinned) (id string, ok bool) {
	key := fileKey(src)
	if planned, seen := r.planned[key]; seen {
		return planned.id, planned.ok
	}
	id, ok = r.planFile(src)
	r.planned[key] = plannedFile{id: id, ok: ok}
	return id, ok
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
func (r *searchResolver) planFile(src paths.Pinned) (id string, ok bool) {
	c := r.classified(src)
	switch {
	case c.err != nil:
		r.skip(src.Path(), trace.SkipReason(c.err))
		return "", false
	case !c.file.Kind().IsText():
		r.skip(src.Path(), c.file.Kind().NotText)
		return "", false
	}
	r.plan.Files = append(r.plan.Files, c.file)
	// The engine answers plan.Files[i] as file id f<i+1>.
	return "f" + strconv.Itoa(len(r.plan.Files)), true
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
	place, ok := r.partOf[m.File]
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
