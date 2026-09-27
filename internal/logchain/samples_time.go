package logchain

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/wlame/rx-go/internal/samples"
)

// timeWindows turns the request's time queries into windows of a ready
// chain's global lines, by the rule one file answers them with: the
// line at T is the first line, in the chain's order, whose own
// timestamp is at or after T; a range runs from the line at T1 (line 1
// for an open start) to the line before the first line later than T2
// (the chain's end for an open end, or when no line is later).
//
// The queries are resolved once for the whole chain
// (samples.ResolveQueries), in the frame of its first part with lines,
// as one file holding the chain would resolve them from its head; a
// time of day without a date takes its date from the chain's first and
// last timestamps. Each bound is then searched in the parts that may
// hold its line (linesAt).
func (s *sampler) timeWindows(ctx context.Context, space lineSpace) ([]window, error) {
	if len(space.spans) == 0 {
		return nil, fmt.Errorf("%w of %s: the chain has no lines", samples.ErrNoTimeFormat, s.d.Response.Path)
	}
	first, err := s.partRequest(space.spans[0].part)
	if err != nil {
		return nil, err
	}
	last, err := s.partRequest(space.spans[len(space.spans)-1].part)
	if err != nil {
		return nil, err
	}
	queries, err := samples.ResolveQueries(ctx, first, s.req.Timestamps, samples.QueryScope{
		FirstMs: s.d.Response.FirstMs, LastMs: s.d.Response.LastMs, Last: last,
	})
	if err != nil {
		return nil, err
	}
	var bounds []samples.TimeBound
	for _, q := range queries {
		for _, b := range []*samples.TimeBound{q.Start, q.After} {
			if b != nil {
				bounds = append(bounds, *b)
			}
		}
	}
	found, err := s.linesAt(ctx, space, bounds)
	if err != nil {
		return nil, err
	}
	windows := make([]window, 0, len(queries))
	next := 0
	// lineOf takes the next bound's line, in the order the bounds were
	// collected above.
	lineOf := func() int64 { line := found[next]; next++; return line }
	for _, q := range queries {
		windows = append(windows, s.timeWindow(q, q.Start != nil, q.After != nil, lineOf))
	}
	return windows, nil
}

// timeWindow is the window of one resolved query, whose bounds' lines
// lineOf gives in order (the start's when hasStart, then the end's when
// hasAfter): a single line with context, or a range, or a window with no
// line (no line at T, or a range whose end comes before its start, as
// one file answers it).
func (s *sampler) timeWindow(q samples.ResolvedQuery, hasStart, hasAfter bool, lineOf func() int64) window {
	none := window{key: q.Value, first: 1, last: 0}
	if !q.Range {
		line := lineOf()
		if line < 1 {
			return none
		}
		return s.around(q.Value, line)
	}
	first := int64(1)
	if hasStart {
		first = lineOf()
	}
	last := int64(math.MaxInt64)
	if hasAfter {
		if after := lineOf(); after > 0 {
			last = after - 1
		}
	}
	if first < 1 || last < first {
		return none
	}
	return window{key: q.Value, first: first, last: last, target: first, isRange: true}
}

// linesAt answers, for each bound, the global line of the first line in
// the chain's order whose own timestamp is at or after it, or -1.
//
// A bound's line lies in the first part whose highest timestamp reaches
// it: every part before has its lines below the bound. A part whose
// highest timestamp is an upper bound (max_is_bound, under file_tz) may
// hold no such line after all; its search finds none, and the bound
// moves on to the next part that may. The active part is always
// searched once the frozen parts are passed: it may have grown since it
// was described.
//
// SECURITY: the parts are searched in one forward sweep. Each bound
// waits at its next candidate span (candidate), in waiting. When the
// sweep reaches a span, it searches that part once for every bound
// waiting there (samples searches them in shared passes), and a bound
// the part does not answer waits at its next candidate after that span,
// which the sweep reaches later in the same pass. So each part is
// searched once, however many bounds pass over it: in one read, or two
// when more bounds wait there than samples takes in one request
// (searchSpan). Each bound's candidate walks together cover each span
// at most once: at most bounds × spans highest times compared, and no
// read.
func (s *sampler) linesAt(ctx context.Context, space lineSpace, bounds []samples.TimeBound) ([]int64, error) {
	found := make([]int64, len(bounds))
	// waiting[j] lists the bounds whose next candidate is span j. A bound
	// is only ever added to a span after the one being searched, so the
	// sweep below meets every entry it adds.
	waiting := make([][]int, len(space.spans))
	for b := range bounds {
		found[b] = -1
		if j := s.candidate(space, bounds[b], 0); j >= 0 {
			waiting[j] = append(waiting[j], b)
		}
	}
	for j := range space.spans {
		which := waiting[j]
		if len(which) == 0 {
			continue
		}
		waiting[j] = nil
		lines, err := s.searchSpan(ctx, space.spans[j], bounds, which)
		if err != nil {
			return nil, err
		}
		for i, b := range which {
			if lines[i] > 0 {
				found[b] = space.spans[j].first + lines[i] - 1
				continue
			}
			if next := s.candidate(space, bounds[b], j+1); next >= 0 {
				waiting[next] = append(waiting[next], b)
			}
		}
	}
	return found, nil
}

// candidate is the first span from `from` on whose part may hold the
// line bound asks for, or -1 when none may: the active part, a part
// whose highest timestamp is not known, or one whose highest timestamp
// reaches the bound (compared in the bound's frame).
func (s *sampler) candidate(space lineSpace, bound samples.TimeBound, from int) int {
	for j := from; j < len(space.spans); j++ {
		p := s.d.Response.Parts[space.spans[j].part]
		if p.IsActive || p.MaxMs == nil || bound.ReachedBy(*p.MaxMs) {
			return j
		}
	}
	return -1
}

// searchSpan searches one part for the bounds listed in which and
// returns each one's line in the part, in the order of which, -1 for
// none.
//
// SECURITY: samples takes at most samples.MaxTimestampValues time
// queries in one request, and refuses more. The bounds waiting at one
// part can number twice that: a request holds at most that many queries
// (SamplesRequest.check), and a range closed at both ends has two
// bounds. So the bounds are searched in requests of at most
// samples.MaxTimestampValues each (searchSpanOnce), one after the
// other: a part is read at most twice for the bounds of one request,
// and never with more queries than samples answers.
//
// slices.Chunk returns an iterator: a function that the range loop
// calls, and that hands the loop consecutive pieces of which, in order,
// each a subslice sharing which's backing array (nothing is copied).
// The lines of each piece are appended in that order, so lines[i]
// answers which[i].
func (s *sampler) searchSpan(ctx context.Context, sp span, bounds []samples.TimeBound, which []int) ([]int64, error) {
	lines := make([]int64, 0, len(which))
	for chunk := range slices.Chunk(which, samples.MaxTimestampValues) {
		found, err := s.searchSpanOnce(ctx, sp, bounds, chunk)
		if err != nil {
			return nil, err
		}
		lines = append(lines, found...)
	}
	return lines, nil
}

// searchSpanOnce searches one part for the bounds listed in which, at
// most samples.MaxTimestampValues of them, through one samples request
// whose time queries carry those bounds already resolved
// (samples.Request.TimeBounds), and returns each one's line in the
// part, -1 for none. The request asks for no context: only the line's
// number is used, and the part is read through read, by the rules that
// read it for a samples answer. Each call builds its own request, so no
// key of one call's bounds reaches the next.
func (s *sampler) searchSpanOnce(ctx context.Context, sp span, bounds []samples.TimeBound, which []int) ([]int64, error) {
	req, err := s.partRequest(sp.part)
	if err != nil {
		return nil, err
	}
	req.MaxLines, req.MaxBytes = s.req.MaxLines, s.req.MaxBytes
	req.TimeBounds = make(map[string]samples.TimeBound, len(which))
	for _, b := range which {
		key := fmt.Sprintf("@%d", b)
		req.Timestamps = append(req.Timestamps, key)
		req.TimeBounds[key] = bounds[b]
	}
	resp, err := s.read(ctx, s.d.Candidate.Parts[s.d.Order[sp.part]], req)
	if err != nil {
		return nil, err
	}
	lines := make([]int64, len(which))
	for i, key := range req.Timestamps {
		lines[i] = -1
		if line, ok := resp.Timestamps[key]; ok {
			lines[i] = line
		}
	}
	return lines, nil
}
