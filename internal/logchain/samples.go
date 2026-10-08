package logchain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrNotReady reports a request by global line or by time on a chain
// that is not ready: its global line numbers and its times wait for the
// line indexes of its frozen parts. A request addressed to one part
// (SamplesRequest.Part) reads that part before the chain is ready.
var ErrNotReady = errors.New("the chain is not ready: its global line numbers and times wait for the line indexes of its parts")

// ErrChainInvalid reports a samples request on an invalid chain. The
// routes answer it with 422; the CLI exits 6.
var ErrChainInvalid = errors.New("the chain is invalid")

// ErrNotAPart reports a SamplesRequest.Part that names no part of the
// chain. The routes answer it with 400; the CLI exits 2.
var ErrNotAPart = errors.New("not a part of the chain")

// ErrSamplesRequest reports a samples request whose positions do not go
// together: none, both lines and times, a part with times, or a negative
// context. The routes answer it with 400; the CLI exits 2.
var ErrSamplesRequest = errors.New("invalid chain samples request")

// SamplesRequest is a samples request on a chain: lines by global
// number, lines of one part by the part's own numbers, or times.
type SamplesRequest struct {
	// Lines are line numbers and ranges, as samples.ParseCSV gives them:
	// global numbers, or the numbers of Part when it is set. A single
	// line -N counts back from the end (of the chain, or of Part).
	Lines []samples.OffsetOrRange
	// Part is the bare name of the part Lines number, as a description
	// lists it; "" for global numbers.
	Part string
	// Timestamps are time queries, each answered with the first line in
	// the chain's order whose own timestamp is at or after its time,
	// with context, or a range's lines (samples.Request.Timestamps).
	Timestamps []string
	// BeforeContext and AfterContext are the lines of context around
	// each single line or time; a range has none.
	BeforeContext, AfterContext int
	// MaxLines and MaxBytes bound the whole answer, summed over its
	// pieces, as samples.Request bounds one file's (0: no limit).
	MaxLines int
	MaxBytes int64
	// IndexLoader is the line index loader every part is read with:
	// samples.StoredIndex, or samples.NoIndex under RX_NO_INDEX.
	IndexLoader samples.IndexLoader
}

// PartReader answers a samples request on one part of a chain: req
// names the part's pin, its kind and its zone. samples.Resolve is one;
// the HTTP API passes one that may answer from the head of the part or
// wait for its index build, as GET /v1/samples does for one file. Its
// errors are returned by Samples as they are.
type PartReader func(ctx context.Context, part Part, req samples.Request) (*rxtypes.SamplesResponse, error)

// Samples answers a samples request on the chain d describes, reading
// each part through read.
//
// Every position becomes a window of lines of the chain: a line and its
// context, a range, or the lines a time names. The window is split
// into pieces, one per part it touches, and each part is read once for
// all the pieces it gives, with samples.Resolve's rules for one file
// (its index, its caps, its early answers). Context crosses from one
// part into the next in a ready chain, and the look back of
// line_timestamps crosses the part edge too (earlierParts), so every
// piece's lines and times are those of the parts read as one file.
//
// Before the chain is ready only a request addressed to one part is
// answered: it reads that part alone, its context and its look back
// stop at the part's edges, and its global numbers are -1.
//
// The errors are ErrChainInvalid, ErrNotReady, ErrNotAPart,
// ErrSamplesRequest, samples' errors (an invalid time, the answer's
// limits), read's errors, and the context's.
func Samples(ctx context.Context, d *Description, req SamplesRequest, read PartReader) (*rxtypes.ChainSamplesResponse, error) {
	if err := req.check(d); err != nil {
		return nil, err
	}
	s := &sampler{d: d, req: req, read: read, kinds: map[int]filekind.Kind{}}
	resp := &rxtypes.ChainSamplesResponse{
		Path: d.Response.Path, Name: d.Response.Name, State: d.Response.State,
		Fingerprint: d.Response.Fingerprint, Parts: d.Response.Parts,
		BeforeContext: req.BeforeContext, AfterContext: req.AfterContext,
		Lines: map[string]int64{}, Timestamps: map[string]int64{}, Samples: map[string][]rxtypes.ChainPiece{},
	}
	space, windows, err := s.plan(ctx)
	if err != nil {
		return nil, err
	}
	windows = uniqueKeys(windows)
	targets := resp.Lines
	if len(req.Timestamps) > 0 {
		targets = resp.Timestamps
	}
	if err := s.fill(ctx, space, windows, targets, resp.Samples); err != nil {
		return nil, err
	}
	return resp, nil
}

// check refuses a request d cannot answer before anything is read.
func (r SamplesRequest) check(d *Description) error {
	if d.Response.State == rxtypes.ChainStateInvalid {
		codes := make([]string, 0, len(d.Response.Reasons))
		for _, reason := range d.Response.Reasons {
			codes = append(codes, reason.Code+": "+reason.Message)
		}
		return fmt.Errorf("%w: %s", ErrChainInvalid, strings.Join(codes, "; "))
	}
	switch {
	case len(r.Lines) > 0 && len(r.Timestamps) > 0:
		return fmt.Errorf("%w: give lines or timestamps, not both", ErrSamplesRequest)
	case len(r.Lines) == 0 && len(r.Timestamps) == 0:
		return fmt.Errorf("%w: give lines or timestamps", ErrSamplesRequest)
	case r.Part != "" && len(r.Timestamps) > 0:
		return fmt.Errorf("%w: a part is addressed by its own line numbers; times are the chain's", ErrSamplesRequest)
	case r.BeforeContext < 0 || r.AfterContext < 0:
		return fmt.Errorf("%w: a context is 0 lines or more", ErrSamplesRequest)
	case len(r.Timestamps) > samples.MaxTimestampValues:
		return fmt.Errorf("%w: %d given, at most %d per request", samples.ErrTooManyTimestamps, len(r.Timestamps), samples.MaxTimestampValues)
	}
	if r.Part != "" {
		if _, ok := d.partIndex(r.Part); !ok {
			return fmt.Errorf("%w: %q is not a part of %s", ErrNotAPart, r.Part, d.Response.Path)
		}
		return nil
	}
	if d.Response.State != rxtypes.ChainStateReady {
		return ErrNotReady
	}
	return nil
}

// partIndex is the index in Response.Parts of the part named name.
func (d *Description) partIndex(name string) (int, bool) {
	for k, p := range d.Response.Parts {
		if p.Name == name {
			return k, true
		}
	}
	return 0, false
}

// sampler is one Samples call: the description, the request, and what
// it learned of the parts while it read them.
type sampler struct {
	d    *Description
	req  SamplesRequest
	read PartReader
	// kinds holds each part's kind (samples.Classify), decided once per
	// call through its pin, keyed by its index in Response.Parts.
	kinds map[int]filekind.Kind
	// lines and bytes are what the answer holds so far, summed over its
	// keys and pieces, against MaxLines and MaxBytes.
	lines int
	bytes int64
}

// span is the run of lines one part holds in the line space a request
// is answered in: from first on, count lines (unbounded when count is
// -1: the active part, which may grow).
type span struct {
	part  int
	first int64
	count int64
}

// lineSpace is the line space a request is answered in: the chain's
// global numbers (every part, in order), or one part's own numbers
// before the chain is ready.
type lineSpace struct {
	spans []span
	// global says that the space numbers the chain: a piece's
	// first_global_line and a key's target are its numbers. Otherwise
	// they are -1.
	global bool
}

// window is the lines one key of the answer asks for, in the request's
// line space.
type window struct {
	key string
	// first and last bound the lines, both included; last is
	// math.MaxInt64 for a window that runs to the end. A window whose
	// last comes before its first holds no line.
	first, last int64
	// target is the line the key names (the line asked for, a range's
	// first line, the line a time found); 0 for none.
	target int64
	// isRange says that the window is a range: its target is its first
	// line that exists.
	isRange bool
	// emptyList says that the window answers an empty list rather than
	// null when it holds no line: a range that ends at 0, as rx samples
	// answers 0-0 on one file.
	emptyList bool
}

// uniqueKeys keeps the first window of each key: two positions with one
// key (12 and 12, or -1 and the line it names) ask for the same lines,
// which the answer holds once.
func uniqueKeys(windows []window) []window {
	seen := make(map[string]bool, len(windows))
	out := windows[:0]
	for _, w := range windows {
		if !seen[w.key] {
			seen[w.key] = true
			out = append(out, w)
		}
	}
	return out
}

// plan turns the request into its line space and its windows.
func (s *sampler) plan(ctx context.Context) (lineSpace, []window, error) {
	d := s.d
	if s.req.Part != "" {
		k, _ := d.partIndex(s.req.Part)
		if d.Response.State != rxtypes.ChainStateReady {
			space := lineSpace{spans: []span{{part: k, first: 1, count: s.spanCount(k)}}}
			windows, err := s.lineWindows(ctx, space, s.req.Lines)
			return space, windows, err
		}
		space := s.chainSpace()
		windows, err := s.partWindows(ctx, space, k)
		return space, windows, err
	}
	space := s.chainSpace()
	if len(s.req.Lines) > 0 {
		windows, err := s.lineWindows(ctx, space, s.req.Lines)
		return space, windows, err
	}
	windows, err := s.timeWindows(ctx, space)
	return space, windows, err
}

// chainSpace is the line space of a ready chain: each part with lines
// at its global start, and the active part from its start on, however
// far it has grown.
func (s *sampler) chainSpace() lineSpace {
	space := lineSpace{global: true}
	for k := range s.d.Response.Parts {
		if s.knownCount(k) == 0 {
			continue
		}
		space.spans = append(space.spans, span{part: k, first: s.d.Starts[k], count: s.spanCount(k)})
	}
	return space
}

// spanCount is how many lines Response.Parts[k] holds in a line space:
// its count, or -1 (unbounded) for the active part, which may have
// grown since it was described: every number past the frozen parts is
// its line (Description.Locate), and reading it answers past its end
// the way one file does.
func (s *sampler) spanCount(k int) int64 {
	if s.d.Response.Parts[k].IsActive {
		return -1
	}
	return s.knownCount(k)
}

// knownCount is the line count of Response.Parts[k] as the description
// knows it, -1 when it does not (the active part without an index).
func (s *sampler) knownCount(k int) int64 {
	if n := s.d.Response.Parts[k].LineCount; n != nil {
		return *n
	}
	return -1
}

// lineWindows turns line positions in space into windows: a single line
// with the request's context around it, -N counted back from the
// space's end and keyed by the line it names, line 0 (no line), and a
// range as asked.
func (s *sampler) lineWindows(ctx context.Context, space lineSpace, positions []samples.OffsetOrRange) ([]window, error) {
	windows := make([]window, 0, len(positions))
	total := int64(-1)
	for _, v := range positions {
		if v.IsRange() {
			windows = append(windows, window{key: v.Key(), first: v.Start, last: *v.End, isRange: true, emptyList: *v.End == 0})
			continue
		}
		line := v.Start
		if line < 0 {
			if total < 0 {
				n, err := s.spaceEnd(ctx, space)
				if err != nil {
					return nil, err
				}
				total = n
			}
			// As one file resolves -N (samples.wantedLinesOf): the line
			// it names, at least line 1.
			line = max(1, total+line+1)
		}
		windows = append(windows, s.around(strconv.FormatInt(line, 10), line))
	}
	return windows, nil
}

// around is the window of a single line with the request's context.
// Line 0 is no line: its window holds none.
func (s *sampler) around(key string, line int64) window {
	if line <= 0 {
		return window{key: key, first: 1, last: 0}
	}
	return window{
		key: key, target: line,
		first: max(1, line-int64(s.req.BeforeContext)), last: addWithin(line, int64(s.req.AfterContext)),
	}
}

// addWithin is a+b, or math.MaxInt64 when the sum would overflow.
func addWithin(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// spaceEnd is the number of the last line of space: the end of its last
// span, whose count is read from the part when the description does not
// know it (the active part: it is asked for its line -1 through read,
// which counts the way one file's -1 is counted).
func (s *sampler) spaceEnd(ctx context.Context, space lineSpace) (int64, error) {
	if len(space.spans) == 0 {
		return 0, nil
	}
	last := space.spans[len(space.spans)-1]
	count, err := s.countOf(ctx, last.part)
	if err != nil {
		return 0, err
	}
	return last.first + count - 1, nil
}

// countOf is the line count of Response.Parts[k]: the description's,
// the active part's from its index when one is current, or else what
// reading its line -1 gives.
func (s *sampler) countOf(ctx context.Context, k int) (int64, error) {
	if n := s.knownCount(k); n >= 0 {
		return n, nil
	}
	req, err := s.partRequest(k)
	if err != nil {
		return 0, err
	}
	req.Lines = []samples.OffsetOrRange{{Start: -1}}
	resp, err := s.read(ctx, s.d.Candidate.Parts[s.d.Order[k]], req)
	if err != nil {
		return 0, err
	}
	// The one key is the line -1 names, at least 1; it holds a line
	// exactly when the part has one.
	for key, sample := range resp.Samples {
		n, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("the line count of %s: key %q: %w", s.d.Response.Parts[k].Name, key, err)
		}
		if len(sample) == 0 {
			return 0, nil
		}
		return n, nil
	}
	return 0, nil
}

// partWindows turns positions in part k's own numbers into windows of
// the chain's numbers (a ready chain): a single line keeps its key and
// its context crosses the part's edges; -N counts back from the part's
// end; a range stays in the part.
func (s *sampler) partWindows(ctx context.Context, space lineSpace, k int) ([]window, error) {
	start := s.d.Starts[k]
	count := s.spanCount(k)
	windows := make([]window, 0, len(s.req.Lines))
	for _, v := range s.req.Lines {
		if v.IsRange() {
			w := window{key: v.Key(), first: start + max(1, v.Start) - 1, last: start + *v.End - 1, isRange: true, emptyList: *v.End == 0}
			if *v.End == 0 {
				w.last = w.first - 1
			}
			if count >= 0 {
				w.last = min(w.last, start+count-1)
			}
			windows = append(windows, w)
			continue
		}
		line := v.Start
		if line < 0 {
			n, err := s.countOf(ctx, k)
			if err != nil {
				return nil, err
			}
			line = max(1, n+line+1)
		}
		key := strconv.FormatInt(line, 10)
		switch {
		case line == 0:
			windows = append(windows, window{key: key, first: 1, last: 0})
		case count >= 0 && line > count:
			// No such line in the part: like a line past the end of one
			// file, the key names none, and its context is the part's
			// lines before it.
			w := s.around(key, start+line-1)
			w.target, w.last = 0, min(w.last, start+count-1)
			windows = append(windows, w)
		default:
			windows = append(windows, s.around(key, start+line-1))
		}
	}
	return windows, nil
}

// piece is one part's share of one window: lines from..to of the part
// (its own numbers; to is math.MaxInt64 for a window that runs to the
// part's end).
type piece struct {
	window int
	span   span
	from   int64
	to     int64
}

// key is the lines key of the piece in its part's samples request.
func (p piece) key() string { return fmt.Sprintf("%d-%d", p.from, p.to) }

// fill reads every window's pieces, at most one read per part, and files
// each key's pieces and target into the answer.
func (s *sampler) fill(ctx context.Context, space lineSpace, windows []window, targets map[string]int64, out map[string][]rxtypes.ChainPiece) error {
	byPart, order, err := s.planPieces(space, windows)
	if err != nil {
		return err
	}
	pieces := make([][]rxtypes.ChainPiece, len(windows))
	// The parts are read in the chain's order, each once for every
	// window that touches it, and the answer counts what each read gave
	// before the next part is read, so no read holds more than the
	// answer has room for.
	slices.Sort(order)
	for _, k := range order {
		got, err := s.readPieces(ctx, space, k, byPart[k])
		if err != nil {
			return err
		}
		for i, p := range byPart[k] {
			if got[i] == nil {
				continue
			}
			if err := s.take(*got[i]); err != nil {
				return err
			}
			pieces[p.window] = append(pieces[p.window], *got[i])
		}
	}
	for w, win := range windows {
		targets[win.key] = -1
		out[win.key] = nil
		if len(pieces[w]) == 0 {
			if win.emptyList {
				out[win.key] = []rxtypes.ChainPiece{}
			}
			continue
		}
		out[win.key] = pieces[w]
		if space.global {
			targets[win.key] = targetOf(win, pieces[w])
		}
	}
	return nil
}

// planPieces splits every window into its pieces, grouped by part,
// and lists the parts the pieces lie in.
//
// SECURITY: the work and the memory follow what the answer may hold,
// not the number of positions times the number of parts. A window's
// first span is found by a binary search over the spans (their first
// lines ascend), and the walk stops at the first span past the window,
// so a window costs O(log spans) plus the spans it touches. A piece of a
// frozen part holds at least one line (cut keeps only lines the part
// has), and the answer holds each key's pieces, so once the planned
// pieces of frozen parts pass MaxLines the answer is refused with
// samples.ErrTooManyLines before any part is read. The active part adds
// at most one piece per window.
func (s *sampler) planPieces(space lineSpace, windows []window) (map[int][]piece, []int, error) {
	byPart := map[int][]piece{}
	var order []int
	frozen := 0
	for w, win := range windows {
		if win.last < win.first {
			continue
		}
		// Go note: sort.Search returns the first index for which the
		// function is true; the spans whose end is before the window's
		// first line come first.
		first := sort.Search(len(space.spans), func(i int) bool {
			sp := space.spans[i]
			return sp.count < 0 || sp.first+sp.count-1 >= win.first
		})
		for _, sp := range space.spans[first:] {
			if sp.first > win.last {
				break
			}
			p, ok := cut(win, sp)
			if !ok {
				continue
			}
			p.window = w
			if sp.count >= 0 {
				if frozen++; s.req.MaxLines > 0 && frozen > s.req.MaxLines {
					return nil, nil, fmt.Errorf("%w: the answer would hold more than the %d lines allowed",
						samples.ErrTooManyLines, s.req.MaxLines)
				}
			}
			if _, seen := byPart[sp.part]; !seen {
				order = append(order, sp.part)
			}
			byPart[sp.part] = append(byPart[sp.part], p)
		}
	}
	return byPart, order, nil
}

// cut is the share of span sp in window w, in the part's own numbers,
// and false when they share no line.
func cut(w window, sp span) (piece, bool) {
	if w.last < w.first {
		return piece{}, false
	}
	first := max(w.first, sp.first)
	last := w.last
	if sp.count >= 0 {
		last = min(last, sp.first+sp.count-1)
	}
	if last < first {
		return piece{}, false
	}
	p := piece{span: sp, from: first - sp.first + 1, to: math.MaxInt64}
	if last != math.MaxInt64 {
		p.to = last - sp.first + 1
	}
	return p, true
}

// targetOf is the global line a key names once its pieces are read: the
// line asked for when a piece holds it, a range's first line, and -1
// when the chain has no such line.
func targetOf(w window, pieces []rxtypes.ChainPiece) int64 {
	if w.isRange {
		return pieces[0].FirstGlobalLine
	}
	for _, p := range pieces {
		if w.target >= p.FirstGlobalLine && w.target < p.FirstGlobalLine+int64(len(p.Lines)) {
			return w.target
		}
	}
	return -1
}

// readPieces reads the pieces of part k in one samples request, each
// as a range of the part's lines, and returns them in order: nil for a
// piece that holds no line (the part ended before it).
func (s *sampler) readPieces(ctx context.Context, space lineSpace, k int, wanted []piece) ([]*rxtypes.ChainPiece, error) {
	req, err := s.partRequest(k)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range wanted {
		if key := p.key(); !seen[key] {
			seen[key] = true
			end := p.to
			req.Lines = append(req.Lines, samples.OffsetOrRange{Start: p.from, End: &end})
		}
	}
	// Each read may hold what the answer has room for still; the
	// answer then counts its pieces (take), a line that two keys share
	// once per key, as the answer holds it.
	req.MaxLines, req.MaxBytes = remaining(s.req.MaxLines, s.lines), remaining64(s.req.MaxBytes, s.bytes)
	if space.global {
		req.Earlier = s.earlierOf(k)
	}
	part := s.d.Candidate.Parts[s.d.Order[k]]
	resp, err := s.read(ctx, part, req)
	if err != nil {
		return nil, err
	}
	out := make([]*rxtypes.ChainPiece, len(wanted))
	for i, p := range wanted {
		lines := resp.Samples[p.key()]
		if len(lines) == 0 {
			continue
		}
		var stamps []*int64
		if resp.LineTimestamps != nil {
			stamps = resp.LineTimestamps[p.key()]
		}
		out[i] = s.newPiece(space, p, lines, stamps)
	}
	return out, nil
}

// newPiece is the answer's piece of p, read as lines with stamps.
func (s *sampler) newPiece(space lineSpace, p piece, lines []string, stamps []*int64) *rxtypes.ChainPiece {
	read := int64(len(lines))
	last := p.from + read - 1
	partEnd := read < p.to-p.from+1
	if p.span.count >= 0 {
		partEnd = last == p.span.count
	}
	piece := &rxtypes.ChainPiece{
		Part: s.d.Response.Parts[p.span.part].Name, FirstLocalLine: p.from, FirstGlobalLine: -1,
		Lines: lines, LineTimestamps: stamps, PartStart: p.from == 1, PartEnd: partEnd,
	}
	if space.global {
		piece.FirstGlobalLine = p.span.first + p.from - 1
	}
	return piece
}

// partRequest is the samples request every read of Response.Parts[k]
// starts from: the part's pin, its kind (decided once per call), the
// description's zone and the request's index loader.
func (s *sampler) partRequest(k int) (samples.Request, error) {
	part := s.d.Candidate.Parts[s.d.Order[k]]
	kind, ok := s.kinds[k]
	if !ok {
		var err error
		if kind, err = samples.Classify(samples.Request{Source: part.File}); err != nil {
			return samples.Request{}, fmt.Errorf("%s: %w", part.Name, err)
		}
		s.kinds[k] = kind
	}
	return samples.Request{
		Path: part.Path, Source: part.File, Kind: &kind,
		FileZone: s.d.fileZone, IndexLoader: s.req.IndexLoader,
	}, nil
}

// take counts a piece into the answer and fails when the answer would
// hold more lines or bytes of line text than the request allows, with
// samples.ErrTooManyLines or samples.ErrTooManyBytes. A line two keys
// share counts once per key, as the answer holds it.
func (s *sampler) take(p rxtypes.ChainPiece) error {
	s.lines += len(p.Lines)
	for _, line := range p.Lines {
		s.bytes += int64(len(line))
	}
	if s.req.MaxLines > 0 && s.lines > s.req.MaxLines {
		return fmt.Errorf("%w: the answer reached %d lines, more than the %d allowed", samples.ErrTooManyLines, s.lines, s.req.MaxLines)
	}
	if s.req.MaxBytes > 0 && s.bytes > s.req.MaxBytes {
		return fmt.Errorf("%w: the answer reached %d bytes of line text, more than the %d allowed", samples.ErrTooManyBytes, s.bytes, s.req.MaxBytes)
	}
	return nil
}

// remaining is what is left of limit after used (0: no limit), at least
// 1 so that a read is still limited: one line past a full answer is read
// and then refused by take.
func remaining(limit, used int) int {
	if limit <= 0 {
		return 0
	}
	return max(1, limit-used)
}

// remaining64 is remaining for a limit in bytes.
func remaining64(limit, used int64) int64 {
	if limit <= 0 {
		return 0
	}
	return max(1, limit-used)
}
