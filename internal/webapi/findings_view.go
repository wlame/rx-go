package webapi

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// linesFilter is the lines=A-B filter of GET /v1/findings: lines First to
// Last, 1-based and inclusive.
type linesFilter struct {
	First, Last int64
}

// maxLinesParamBytes bounds the lines parameter: two numbers of at most
// 19 digits and a dash.
const maxLinesParamBytes = 64

// parseLineRange reads the lines parameter: "A-B" with 1 <= A <= B, or
// one line "N" (N-N). The empty value is no filter (nil).
func parseLineRange(value string) (*linesFilter, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > maxLinesParamBytes {
		return nil, fmt.Errorf("lines %s: at most %d bytes", quotedPrefix(value), maxLinesParamBytes)
	}
	firstText, lastText, isRange := strings.Cut(value, "-")
	if !isRange {
		lastText = firstText
	}
	first, err1 := strconv.ParseInt(firstText, 10, 64)
	last, err2 := strconv.ParseInt(lastText, 10, 64)
	switch {
	case err1 != nil || err2 != nil:
		return nil, fmt.Errorf("lines %s: want A-B, two line numbers, or one line number", quotedPrefix(value))
	case first < 1 || last < first:
		return nil, fmt.Errorf("lines %s: want 1 <= A <= B", quotedPrefix(value))
	}
	return &linesFilter{First: first, Last: last}, nil
}

// findingsFilter is what GET /v1/findings keeps of a file's stored
// findings: the findings of some signatures or groups, holding a text,
// overlapping some lines, starting after or before a line. A zero
// member keeps everything.
type findingsFilter struct {
	signatures, groups []string
	// query is the q parameter, lowered.
	query string
	lines *linesFilter
	// afterLine and beforeLine are bounds on the first line, 0 for none.
	afterLine, beforeLine int64
}

// apply returns the findings f keeps, in the one order, or in reverse
// with a before_line bound (the nearest before the line first). cat
// gives each signature's group and name.
func (f findingsFilter) apply(all []rxtypes.Finding, cat *signatures.Catalogue) []rxtypes.Finding {
	out := make([]rxtypes.Finding, 0, len(all))
	for _, finding := range all {
		if f.keeps(finding, cat) {
			out = append(out, finding)
		}
	}
	if f.beforeLine > 0 {
		slices.Reverse(out)
	}
	return out
}

// keeps reports whether f keeps one finding.
func (f findingsFilter) keeps(finding rxtypes.Finding, cat *signatures.Catalogue) bool {
	sig, known := cat.Lookup(finding.Signature)
	switch {
	case len(f.signatures) > 0 && !slices.Contains(f.signatures, finding.Signature):
		return false
	case len(f.groups) > 0 && (!known || !slices.Contains(f.groups, sig.Group)):
		return false
	case f.lines != nil && (finding.EndLine < f.lines.First || finding.StartLine > f.lines.Last):
		return false
	case f.afterLine > 0 && finding.StartLine <= f.afterLine:
		return false
	case f.beforeLine > 0 && finding.StartLine >= f.beforeLine:
		return false
	}
	if f.query == "" {
		return true
	}
	// The excerpt is masked already, so a query never tests a secret.
	return strings.Contains(strings.ToLower(finding.Excerpt), f.query) ||
		(known && strings.Contains(strings.ToLower(sig.Name), f.query))
}

// pageOf is the page of filtered: from offset, at most limit findings,
// in a slice of its own.
func pageOf(filtered []rxtypes.Finding, offset, limit int) []rxtypes.Finding {
	if offset >= len(filtered) {
		return []rxtypes.Finding{}
	}
	return slices.Clone(filtered[offset:min(len(filtered), offset+limit)])
}

// ticksOf places the findings on a timeline of width columns from
// start to end (milliseconds, the file's time range; nil when it is not
// known): column k holds the findings whose time t has
// floor((t - start) * width / (end - start + 1)) = k, so start falls in
// column 0 and end in column width - 1. A finding without a time, or
// with one outside start to end, is in no column. For each column that
// holds findings it gives, per signature, how many and the first line
// of the first in line order.
//
// INVARIANT: no product overflows. t - start is at most end - start, and
// times lie within the years 1 to 9999 (about 3.2e14 ms), so times
// width (at most 4000) stays below 1.3e18, under the int64 limit.
func ticksOf(found []rxtypes.Finding, width int, start, end *int64) *rxtypes.FindingsTicks {
	ticks := &rxtypes.FindingsTicks{Width: width, StartMS: start, EndMS: end, Columns: []rxtypes.TickColumn{}}
	if start == nil || end == nil || *end < *start || width <= 0 {
		return ticks
	}
	span := *end - *start + 1
	columns := map[int]map[string]rxtypes.TickCount{}
	for _, f := range found {
		if f.TimeMS == nil || *f.TimeMS < *start || *f.TimeMS > *end {
			continue
		}
		column := int((*f.TimeMS - *start) * int64(width) / span)
		bySignature := columns[column]
		if bySignature == nil {
			bySignature = map[string]rxtypes.TickCount{}
			columns[column] = bySignature
		}
		count, seen := bySignature[f.Signature]
		if !seen || f.StartLine < count.FirstLine {
			count.FirstLine = f.StartLine
		}
		count.Count++
		bySignature[f.Signature] = count
	}
	for _, column := range slices.Sorted(maps.Keys(columns)) {
		ticks.Columns = append(ticks.Columns, rxtypes.TickColumn{Column: column, Signatures: columns[column]})
	}
	return ticks
}
