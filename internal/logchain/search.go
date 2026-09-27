package logchain

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
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
	// Scan describes each chain the way `rx logs show` does (Options.Scan
	// of Describe): a part without a current line index is indexed in
	// memory, so the chain is ready, or invalid, and every match in it
	// that the trace numbers gets its chain line. Without it, as over
	// HTTP, a chain with a part not indexed yet is pending, and its
	// matches have chain_line -1.
	Scan bool
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
// The patterns are checked before any path is walked. The errors are
// *SearchPathError, ErrPartChanged (a part was renamed or replaced
// between the listing and its description), trace.ErrInvalidPattern,
// and the context's error.
func Search(ctx context.Context, engine *trace.Engine, req SearchRequest) (*SearchResult, error) {
	r := &searchResolver{
		req:    req,
		partOf: map[string]partPlace{},
		files:  map[string]classified{},
		dirs:   map[string]*listedDir{},
	}
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
	// handles in one directory list it and group its chains once.
	dirs map[string]*listedDir
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
// chains among its entries by name; err when it could not be listed.
type listedDir struct {
	entries []Entry
	// names maps each entry's name to its path as listed.
	names  map[string]string
	chains map[string]Candidate
	err    error
}

// resolve is the trace.Resolver of the search: it resolves every path
// and returns the plan. The engine calls it once, after the patterns
// are checked.
func (r *searchResolver) resolve(ctx context.Context) (*trace.SearchPlan, error) {
	r.plan.Paths = r.req.Paths
	for _, p := range r.req.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
// its directory, listed and grouped once per search, holds a chain of
// that name. found is false, with no error, for a path that names no
// chain: one that is not a bare name in a directory, a directory that
// cannot be listed or holds no such chain. The error is Describe's.
func (r *searchResolver) addHandle(ctx context.Context, p string) (found bool, err error) {
	dir, name, err := splitHandle(p)
	if err != nil {
		return false, nil
	}
	listed := r.listDir(dir)
	if listed.err != nil {
		return false, nil
	}
	c, ok := listed.chains[name]
	if !ok {
		return false, nil
	}
	if err := r.addChain(ctx, c, listed.names); err != nil {
		return true, err
	}
	if c.TooManyParts {
		// Not read as one text: each file of the chain's name is
		// searched as a file of its own, in the listing's order.
		for _, e := range listed.entries {
			if isCandidateFile(e) && namesChain(e.Name, c.Name) {
				r.addFile(e.File)
			}
		}
	}
	return true, nil
}

// namesChain reports whether a file name belongs to the chain named
// chain by its name alone: the active file's name, or a name a template
// reads as a part of it.
func namesChain(name, chain string) bool {
	if name == chain {
		return true
	}
	m, ok := matchName(name)
	return ok && m.chain == chain
}

// listDir lists the directory dir once per search, through its pin, and
// groups its entries into chains (Group), classifying them as the
// search reads them.
//
// SECURITY: dir is checked against the search roots and pinned before
// it is listed, as Resolve does; the listing refuses a link that leads
// out of the roots or into a hidden entry, and every entry keeps its
// pin.
func (r *searchResolver) listDir(dir string) *listedDir {
	if listed, ok := r.dirs[dir]; ok {
		return listed
	}
	listed := &listedDir{chains: map[string]Candidate{}}
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
	listed.entries = EntriesOf(entries)
	listed.names = pathsByName(listed.entries)
	for _, c := range Group(dir, listed.entries, r.classify) {
		listed.chains[c.Name] = c
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
		for _, c := range Group(dir, entries, r.classify) {
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

// addChain describes a chain, names it in the answer, and adds its
// parts to the search in the chain's order. A part the listing could
// not read is not searched; it is skipped with its read error. Another
// encoding of a part is skipped with ReasonDuplicatePart. A chain of
// more than MaxParts parts gets its entry and adds no part: its caller
// searches its files on their own.
//
// The error is Describe's: ErrPartChanged, or the context's.
func (r *searchResolver) addChain(ctx context.Context, c Candidate, names map[string]string) error {
	d, err := Describe(ctx, c, Options{Scan: r.req.Scan})
	if err != nil {
		return err
	}
	ch := &searchChain{id: "c" + strconv.Itoa(len(r.chains)+1), desc: d, files: []string{}}
	index := len(r.chains)
	r.chains = append(r.chains, ch)
	for order, part := range d.Parts() {
		if part.ReadError != nil {
			r.skip(part.Path, trace.SkipReason(part.ReadError))
		} else if id, ok := r.addFile(part.File); ok {
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
// cannot be read or is not text, named with the reason a trace gives.
func (r *searchResolver) addFile(src paths.Pinned) (id string, ok bool) {
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
