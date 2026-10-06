package logchain

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// provisionalOrder is the order of a candidate's parts as Group sorted
// them: 0, 1, …, n-1.
func provisionalOrder(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	return order
}

// timeOrder is the chain's order by time: the parts whose first
// timestamp is known are sorted by it, a tie keeping the provisional
// order, and take the places these parts have in the provisional order;
// every other part (empty, without timestamps, or not read) keeps its
// own place.
//
// So an empty part stays where its name puts it, between the parts its
// key falls between, and adds no line wherever it is.
func timeOrder(c Candidate, facts []partFacts) []int {
	order := provisionalOrder(len(c.Parts))
	var timed []int
	for i, part := range c.Parts {
		if !isEmpty(part, facts[i]) && facts[i].firstMs != nil {
			timed = append(timed, i)
		}
	}
	// The slots are the provisional places of the timed parts, already
	// in ascending order; the parts that fill them are the same parts
	// sorted by their first timestamp. SortStableFunc keeps two parts
	// with one first timestamp in the provisional order.
	byTime := slices.Clone(timed)
	slices.SortStableFunc(byTime, func(a, b int) int {
		return cmp.Compare(*facts[a].firstMs, *facts[b].firstMs)
	})
	for k, slot := range timed {
		order[slot] = byTime[k]
	}
	return order
}

// journalOrder is the order of a journal chain by its parts' headers,
// as systemd orders journal files: the parts one counter (seqnum_id)
// numbers by the sequence number of their first entry, and the
// counters' groups by the time of the first entry of each group's first
// part. A tie keeps the provisional order. These parts take the places
// they have in the provisional order, as timeOrder fills its slots;
// every other part (an empty file, a journal without entries, a part
// that is not a journal or was not read) keeps its own place.
//
// It needs no line index: the headers come from every describe. The
// shown times of the parts' text play no part: they may go back (a
// program's own clock, _SOURCE_REALTIME_TIMESTAMP), and the headers
// still say which entries journald wrote first.
func journalOrder(c Candidate, facts []partFacts) []int {
	order := provisionalOrder(len(c.Parts))
	placed := partsWithEntries(order, facts)
	byHeader := slices.Clone(placed)
	// Each counter's parts together, by their first sequence number.
	// SortStableFunc keeps two parts with one first entry (a copy of a
	// part) in the provisional order.
	slices.SortStableFunc(byHeader, func(a, b int) int {
		ha, hb := facts[a].header, facts[b].header
		return cmp.Or(bytes.Compare(ha.SeqnumID[:], hb.SeqnumID[:]), cmp.Compare(ha.HeadSeqnum, hb.HeadSeqnum))
	})
	// Then the counters' groups by the head time of their first part,
	// a tie by that part's provisional place.
	groups := splitByCounter(byHeader, facts)
	slices.SortStableFunc(groups, func(a, b []int) int {
		return cmp.Or(cmp.Compare(facts[a[0]].header.HeadRealtime, facts[b[0]].header.HeadRealtime), cmp.Compare(a[0], b[0]))
	})
	sorted := slices.Concat(groups...)
	for k, slot := range placed {
		order[slot] = sorted[k]
	}
	return order
}

// partsWithEntries are the parts of order (candidate indexes) whose
// journal header was read and counts entries, in that order.
func partsWithEntries(order []int, facts []partFacts) []int {
	var parts []int
	for _, i := range order {
		if h := facts[i].header; h != nil && h.NEntries > 0 {
			parts = append(parts, i)
		}
	}
	return parts
}

// splitByCounter cuts parts, sorted so that each counter's parts are
// together, into one group per counter, in that order.
func splitByCounter(parts []int, facts []partFacts) [][]int {
	var groups [][]int
	for k, i := range parts {
		if k == 0 || facts[i].header.SeqnumID != facts[parts[k-1]].header.SeqnumID {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], i)
	}
	return groups
}

// partReasons are the reasons one part makes a chain invalid on its
// own: it cannot be read, a part of a journal chain cannot be read as a
// journal, or it holds lines and no timestamp.
func partReasons(part Part, f partFacts) []rxtypes.ChainReason {
	switch {
	case f.unreadable != "":
		return []rxtypes.ChainReason{{
			Code: rxtypes.ChainReasonUnreadable, Parts: []string{part.Name},
			Message: fmt.Sprintf("%s cannot be read: %s", part.Name, f.unreadable),
		}}
	case f.notJournal != "":
		return []rxtypes.ChainReason{{
			Code: rxtypes.ChainReasonUnreadable, Parts: []string{part.Name},
			Message: fmt.Sprintf("%s cannot be read as a journal: %s", part.Name, f.notJournal),
		}}
	case f.noTimestamps:
		return []rxtypes.ChainReason{{
			Code: rxtypes.ChainReasonNoTimestamps, Parts: []string{part.Name},
			Message: fmt.Sprintf("%s has lines and no timestamp rx recognizes, so its place in time is not known", part.Name),
		}}
	}
	return nil
}

// orderReasons are the reasons the parts, in the chain's order, make it
// invalid together: two neighbors overlap in time by more than the
// tolerance, or the active part does not come last.
//
// Only the parts with lines and timestamps take part: an empty part
// adds nothing between its neighbors.
func orderReasons(c Candidate, facts []partFacts, order []int, toleranceMs int64) []rxtypes.ChainReason {
	var timed []int
	for _, i := range order {
		if !isEmpty(c.Parts[i], facts[i]) && facts[i].firstMs != nil {
			timed = append(timed, i)
		}
	}
	var reasons []rxtypes.ChainReason
	for k := 0; k+1 < len(timed); k++ {
		if reason, ok := overlapReason(c.Parts[timed[k]], facts[timed[k]], c.Parts[timed[k+1]], facts[timed[k+1]], toleranceMs); ok {
			reasons = append(reasons, reason)
		}
	}
	return append(reasons, activeNotLastReasons(c, timed)...)
}

// journalOrderReasons are the reasons the parts of a journal chain, in
// its order, make it invalid together: two neighbors of one counter
// hold entries in common, or the active part does not come last. Only
// the parts with entries take part; the time overlap check of other
// chains does not apply (toleranceMs is not used), since a journal's
// shown times may go back while its entries stay in order.
func journalOrderReasons(c Candidate, facts []partFacts, order []int, _ int64) []rxtypes.ChainReason {
	placed := partsWithEntries(order, facts)
	var reasons []rxtypes.ChainReason
	for k := 0; k+1 < len(placed); k++ {
		if reason, ok := entriesOverlapReason(c.Parts[placed[k]], facts[placed[k]], c.Parts[placed[k+1]], facts[placed[k+1]]); ok {
			reasons = append(reasons, reason)
		}
	}
	return append(reasons, activeNotLastReasons(c, placed)...)
}

// entriesOverlapReason reports whether the part a, followed by b in a
// journal chain, holds entries b holds too: both are numbered by one
// counter, and a's last sequence number is not below b's first. A copy
// of a part left beside it does this. Parts of two counters are never
// compared: their numbers count apart.
func entriesOverlapReason(a Part, fa partFacts, b Part, fb partFacts) (rxtypes.ChainReason, bool) {
	ha, hb := fa.header, fb.header
	if ha.SeqnumID != hb.SeqnumID || ha.TailSeqnum < hb.HeadSeqnum {
		return rxtypes.ChainReason{}, false
	}
	return rxtypes.ChainReason{
		Code: rxtypes.ChainReasonEntriesOverlap, Parts: []string{a.Name, b.Name},
		Message: fmt.Sprintf("%s ends with entry %d of its counter, which is not before entry %d, the first of %s: "+
			"the two parts hold the same entries, as a copy of a part does", a.Name, ha.TailSeqnum, hb.HeadSeqnum, b.Name),
	}, true
}

// activeNotLastReasons is the reason active_not_last when the active
// part is among placed (candidate indexes in the chain's order) and not
// the last of them: it names the active part and every part of placed
// after it.
func activeNotLastReasons(c Candidate, placed []int) []rxtypes.ChainReason {
	var reasons []rxtypes.ChainReason
	for k, i := range placed {
		if !c.Parts[i].IsActive || k == len(placed)-1 {
			continue
		}
		later := make([]string, 0, len(placed)-k)
		later = append(later, c.Parts[i].Name)
		for _, j := range placed[k+1:] {
			later = append(later, c.Parts[j].Name)
		}
		reasons = append(reasons, rxtypes.ChainReason{
			Code: rxtypes.ChainReasonActiveNotLast, Parts: later,
			Message: fmt.Sprintf("the active file %s starts before %s, which should be older", c.Parts[i].Name,
				strings.Join(later[1:], ", ")),
		})
	}
	return reasons
}

// chainOrdering is how one kind of chain is ordered and checked once
// its parts' facts are read (Description.assemble).
type chainOrdering struct {
	// order is the chain's order, as Description.Order holds it.
	order func(c Candidate, facts []partFacts) []int
	// reasons are the checks that compare the parts in that order.
	reasons func(c Candidate, facts []partFacts, order []int, toleranceMs int64) []rxtypes.ChainReason
	// needsEveryPart says that the order and the checks need facts only
	// a line index gives (each part's times): until every part is known,
	// a chain no check has failed keeps the provisional order and is not
	// compared.
	needsEveryPart bool
}

// The orderings: a chain by the timestamps of its parts' text, a
// journal chain by its parts' headers, which every describe reads.
var (
	timeOrdering    = chainOrdering{order: timeOrder, reasons: orderReasons, needsEveryPart: true}
	journalOrdering = chainOrdering{order: journalOrder, reasons: journalOrderReasons}
)

// orderingOf is the ordering of the chain c.
func orderingOf(c Candidate) chainOrdering {
	if c.IsJournal() {
		return journalOrdering
	}
	return timeOrdering
}

// overlapReason reports whether the part a, followed in time by b,
// reaches past b's first timestamp by more than the tolerance: a's
// highest timestamp (its upper bound when only a bound is known) above
// b's first plus toleranceMs. A part whose highest timestamp is not
// known (the active part without an index, out of its place) is not
// compared.
func overlapReason(a Part, fa partFacts, b Part, fb partFacts, toleranceMs int64) (rxtypes.ChainReason, bool) {
	if fa.maxMs == nil || *fa.maxMs <= *fb.firstMs+toleranceMs {
		return rxtypes.ChainReason{}, false
	}
	overlap := *fa.maxMs - *fb.firstMs
	highest := "highest timestamp"
	if fa.maxIsBound {
		highest = "highest timestamp (an upper bound under file_tz)"
	}
	return rxtypes.ChainReason{
		Code: rxtypes.ChainReasonOverlap, Parts: []string{a.Name, b.Name}, OverlapMs: &overlap,
		Message: fmt.Sprintf("the %s of %s is %d ms after the first timestamp of %s, more than the tolerance of %d s (%s)",
			highest, a.Name, overlap, b.Name, toleranceMs/1000, config.ChainOverlapSecondsSetting.Name),
	}, true
}

// newResponse is the part of a description that comes from the
// candidate alone: its handle, name, fingerprint, missing parts and
// their count, with empty lists for the rest.
func newResponse(c Candidate, fingerprint string) *rxtypes.ChainResponse {
	return &rxtypes.ChainResponse{
		Path: c.Handle(), Name: c.Name, Fingerprint: fingerprint,
		Reasons: []rxtypes.ChainReason{}, Parts: []rxtypes.ChainPart{},
		Missing: c.Missing, MissingCount: c.MissingCount, Gaps: []rxtypes.ChainGap{},
	}
}

// partEntries are the part entries of the answer, in order, from the
// candidate's parts and their facts. The global starts are filled in
// later, for a ready chain only.
func partEntries(c Candidate, order []int, facts []partFacts) []rxtypes.ChainPart {
	entries := make([]rxtypes.ChainPart, 0, len(order))
	for _, i := range order {
		entries = append(entries, partEntry(c.Parts[i], facts[i]))
	}
	return entries
}

// partEntry is the answer's entry of one part.
func partEntry(p Part, f partFacts) rxtypes.ChainPart {
	entry := rxtypes.ChainPart{
		Name: p.Name, Path: p.Path, IsActive: p.IsActive,
		Size:       p.Info.Size(),
		ModifiedAt: p.Info.ModTime().UTC().Format(wireTimestampLayout),
		IsIndexed:  f.indexed,
		LineCount:  f.lines,
		FirstMs:    f.firstMs, LastMs: f.lastMs, MaxMs: f.maxMs, MaxIsBound: f.maxIsBound,
		TimeFormat: f.format,
		DayFirst:   f.dayFirst, Example: f.example,
		Duplicates: p.Duplicates,
	}
	if !p.IsActive {
		key := p.Key.Text
		entry.Key = &key
	}
	if p.Format != compression.FormatNone {
		name := filekind.Kind{Format: p.Format}.CompressionName()
		entry.CompressionFormat = &name
	}
	return entry
}

// fillReady fills what only a ready chain has: each part's global
// start, the line counts, the chain's first and last timestamp, the
// running highest timestamp and the time gaps.
func (d *Description) fillReady(facts []partFacts) {
	resp := d.Response
	d.Starts = make([]int64, len(d.Order))
	d.MaxSoFar = make([]int64, len(d.Order))
	next, highest := int64(1), int64(math.MinInt64)
	var frozen int64
	activeKnown := true
	for k, i := range d.Order {
		part, f := d.Candidate.Parts[i], facts[i]
		start := next
		d.Starts[k] = start
		resp.Parts[k].GlobalStart = &start
		lines := int64(0)
		switch {
		case f.lines != nil:
			lines = *f.lines
		case part.IsActive:
			activeKnown = false
		}
		next += lines
		if !part.IsActive {
			frozen += lines
		}
		if f.maxMs != nil {
			highest = max(highest, *f.maxMs)
		}
		d.MaxSoFar[k] = highest
	}
	resp.FrozenLineCount = &frozen
	if activeKnown {
		total := next - 1
		resp.LineCount = &total
	}
	timed := d.timedParts(facts)
	if len(timed) > 0 {
		resp.FirstMs = facts[timed[0]].firstMs
		resp.LastMs = facts[timed[len(timed)-1]].lastMs
	}
	resp.Gaps = gaps(d.Candidate, facts, timed)
}

// timedParts are the candidate indexes of the parts with lines, in the
// chain's order. In a ready chain each of them has timestamps.
func (d *Description) timedParts(facts []partFacts) []int {
	var timed []int
	for _, i := range d.Order {
		if !isEmpty(d.Candidate.Parts[i], facts[i]) {
			timed = append(timed, i)
		}
	}
	return timed
}

// minPartsForGaps is how many parts with lines a chain needs before its
// time gaps are looked for: with fewer, the usual distance between two
// parts' first timestamps says too little.
const minPartsForGaps = 4

// gaps are the stretches of time no part of a ready chain covers: after
// part k ends and before part k+1 starts, when the time between them,
// first(k+1) − max(k), is more than 1.5 times D, the median distance
// between the first timestamps of neighboring parts. Daily parts with a
// quiet weekend show no gap; a missing week does.
//
// timed are the parts with lines, in the chain's order. The comparison
// is made on integers: 2 × (first(k+1) − max(k)) > 3 × D, with D the
// median, so no rounding moves an edge case.
func gaps(c Candidate, facts []partFacts, timed []int) []rxtypes.ChainGap {
	out := []rxtypes.ChainGap{}
	if len(timed) < minPartsForGaps {
		return out
	}
	distances := make([]int64, 0, len(timed)-1)
	for k := 0; k+1 < len(timed); k++ {
		distances = append(distances, *facts[timed[k+1]].firstMs-*facts[timed[k]].firstMs)
	}
	// twiceMedian is twice the median distance: the middle value of an
	// odd count twice, the sum of the two middle ones of an even count,
	// so the median of an even count needs no division.
	slices.Sort(distances)
	mid := len(distances) / 2
	twiceMedian := 2 * distances[mid]
	if len(distances)%2 == 0 {
		twiceMedian = distances[mid-1] + distances[mid]
	}
	for k := 0; k+1 < len(timed); k++ {
		before, after := facts[timed[k]], facts[timed[k+1]]
		if before.maxMs == nil {
			continue
		}
		hole := *after.firstMs - *before.maxMs
		// hole > 1.5 × median, as 2 × hole > 3 × median, which is
		// 4 × hole > 3 × twiceMedian.
		if 4*hole > 3*twiceMedian {
			out = append(out, rxtypes.ChainGap{
				After: c.Parts[timed[k]].Name, Before: c.Parts[timed[k+1]].Name,
				FromMs: *before.maxMs, ToMs: *after.firstMs,
			})
		}
	}
	return out
}

// Locate maps a global line number of a ready chain to the part that
// holds it, as an index of Response.Parts, and its line number in that
// part. ok is false before the chain is ready, for a number below 1,
// and for one past the end of the last frozen part when the chain has
// no active part.
//
// Every number past the frozen parts belongs to the active part, at
// global − its start + 1, even past the line count this description
// knows: the active part grows, and reading it answers past its end the
// way a single file does.
func (d *Description) Locate(global int64) (part int, local int64, ok bool) {
	if d.Starts == nil || global < 1 || len(d.Starts) == 0 {
		return 0, 0, false
	}
	last := len(d.Starts) - 1
	if d.Response.Parts[last].IsActive && global >= d.Starts[last] {
		return last, global - d.Starts[last] + 1, true
	}
	// The last part whose start is at or before global. Go note:
	// sort.Search finds the first index for which the function is true,
	// in O(log n) calls, on a slice the function is false then true on:
	// Starts never decreases.
	k := sort.Search(len(d.Starts), func(i int) bool { return d.Starts[i] > global }) - 1
	// Empty parts share their start with the part after them; step back
	// over the empty parts at the end to the part that holds lines.
	for k >= 0 && d.lineCount(k) == 0 {
		k--
	}
	if k < 0 {
		return 0, 0, false
	}
	local = global - d.Starts[k] + 1
	if local > d.lineCount(k) {
		return 0, 0, false
	}
	return k, local, true
}

// lineCount is the line count of Response.Parts[k], 0 when not known.
func (d *Description) lineCount(k int) int64 {
	if n := d.Response.Parts[k].LineCount; n != nil {
		return *n
	}
	return 0
}
