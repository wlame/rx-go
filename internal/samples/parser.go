package samples

import (
	"fmt"
	"strconv"
	"strings"
)

// OffsetOrRange represents one item in a comma-separated samples spec.
// A single value has End == nil (negative allowed). A range has both
// ends 0 or more with Start <= End, or, for lines, both ends negative
// with Start <= End: a range counted from the end (-100--1 is the last
// hundred lines). A range never mixes the two.
//
// Examples produced by ParseLines("100,200-350,-5,-3--1"):
//
//	{Start: 100}
//	{Start: 200, End: &350}
//	{Start: -5}
//	{Start: -3, End: &-1}
type OffsetOrRange struct {
	Start int64
	End   *int64
}

// specGrammar is what the positions of a spec are: the noun its errors
// name, and whether a range may count from the end. The two grammars
// differ in nothing else, so the data decides and the parser is one.
type specGrammar struct {
	noun           string
	negativeRanges bool
}

var (
	lineGrammar   = specGrammar{noun: "line", negativeRanges: true}
	offsetGrammar = specGrammar{noun: "offset", negativeRanges: false}
)

// ParseLines parses a comma-separated spec of line numbers and ranges
// (the --lines flag, the lines query parameter) into OffsetOrRange
// values. Empty or whitespace-only entries are rejected. A single
// request can contain any mix of singles and ranges.
//
// Syntax:
//
//	"100"         → single, start=100
//	"-5"          → single, start=-5 (the fifth line from the end)
//	"100-200"     → range [100, 200] (both >= 0, start <= end)
//	"-100--1"     → range counted from the end (both < 0, start <= end)
//	"100,200-300" → multi
func ParseLines(spec string) ([]OffsetOrRange, error) {
	return parseSpec(spec, lineGrammar)
}

// ParseOffsets parses a comma-separated spec of byte offsets and ranges
// (the --offsets flag, the offsets query parameter): the grammar of
// ParseLines, except that a range cannot count from the end.
func ParseOffsets(spec string) ([]OffsetOrRange, error) {
	return parseSpec(spec, offsetGrammar)
}

// parseSpec is the parser behind ParseLines and ParseOffsets.
func parseSpec(spec string, g specGrammar) ([]OffsetOrRange, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("empty spec")
	}
	parts := strings.Split(spec, ",")
	out := make([]OffsetOrRange, 0, len(parts))
	for _, p := range parts {
		v, err := parseOne(strings.TrimSpace(p), g)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// parseOne parses a single "100", "-5", "100-200" or "-100--1" token.
// Every error names the grammar's noun, so a user who typed --lines is
// never told about an offset.
func parseOne(value string, g specGrammar) (OffsetOrRange, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return OffsetOrRange{}, fmt.Errorf("empty value")
	}
	// A single integer first: it covers positive and negative singles.
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return OffsetOrRange{Start: n}, nil
	}
	sep := rangeSeparator(value)
	if sep < 0 {
		return OffsetOrRange{}, fmt.Errorf(
			"invalid %s: %s. Must be an integer or range (e.g., 100-200)", g.noun, value)
	}
	start, e1 := strconv.ParseInt(strings.TrimSpace(value[:sep]), 10, 64)
	end, e2 := strconv.ParseInt(strings.TrimSpace(value[sep+1:]), 10, 64)
	if e1 != nil || e2 != nil {
		return OffsetOrRange{}, fmt.Errorf(
			"invalid %s range: %s. Both values must be integers", g.noun, value)
	}
	if start < 0 || end < 0 {
		if !g.negativeRanges {
			return OffsetOrRange{}, fmt.Errorf(
				"invalid %s range: %s. Ranges cannot contain negative values", g.noun, value)
		}
		if (start < 0) != (end < 0) {
			return OffsetOrRange{}, fmt.Errorf(
				"invalid %s range: %s. Both ends count from the start (100-200) or both from the end (-100--1)", g.noun, value)
		}
	}
	if start > end {
		return OffsetOrRange{}, fmt.Errorf(
			"invalid %s range: %s. Start must be <= end", g.noun, value)
	}
	return OffsetOrRange{Start: start, End: &end}, nil
}

// rangeSeparator returns the index of the '-' that parts a range's two
// ends: the first '-' whose nearest non-space character before it is a
// digit. A leading '-' and the '-' of a negative end (-100--1) are
// signs, not separators. -1 when the value has none.
func rangeSeparator(value string) int {
	lastWasDigit := false
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '-' && lastWasDigit:
			return i
		case c == ' ':
			// Spaces around the separator are tolerated; skip them.
		default:
			lastWasDigit = c >= '0' && c <= '9'
		}
	}
	return -1
}

// Key returns the string key the samples response uses for this value.
// Matches Python's samples.py convention:
//
//	OffsetOrRange{100}          → "100"
//	OffsetOrRange{-5}           → "-5"
//	OffsetOrRange{100, &200}    → "100-200"
func (o OffsetOrRange) Key() string {
	if o.End == nil {
		return strconv.FormatInt(o.Start, 10)
	}
	return fmt.Sprintf("%d-%d", o.Start, *o.End)
}

// IsRange reports whether this value represents a range (vs a single
// value).
func (o OffsetOrRange) IsRange() bool { return o.End != nil }

// LinesFromStart returns o with a line counted from the end resolved to
// the line it names in a text of total lines: -1 is the last line and
// -total the first. A single and both ends of a range follow the one
// rule, so -3--1 on 300 lines is 298-300, and a position counted from
// the start is returned as it is. A position before the first line
// resolves to 0 or less: the text has no such line (NamesALine,
// ReachesALine say what of the position the text has). The answer's
// key for a position counted from the end is the Key of what this
// returns when the text has it, and the key asked by otherwise.
func (o OffsetOrRange) LinesFromStart(total int64) OffsetOrRange {
	if o.Start >= 0 {
		return o
	}
	resolved := OffsetOrRange{Start: total + o.Start + 1}
	if o.End != nil {
		end := total + *o.End + 1
		resolved.End = &end
	}
	return resolved
}

// NamesALine reports whether o, counted from the start, is a position
// the text's numbering has: a line at or after line 1, or a range whose
// first line is. Line 0 and a position resolved to before the first
// line are not.
func (o OffsetOrRange) NamesALine() bool { return o.Start >= 1 }

// ReachesALine reports whether o, counted from the start, has at least
// one line in the text's numbering: a single at or after line 1, or a
// range whose last line is, whatever its first (a range that starts
// before the first line holds the lines from line 1 on).
func (o OffsetOrRange) ReachesALine() bool {
	if o.End == nil {
		return o.Start >= 1
	}
	return *o.End >= 1
}

// IsRangeKey reports whether key, a key of a samples answer, names a
// range ("100-200", "-3--1") rather than a single position ("100",
// "-31"): it has the separator of a range after a digit.
func IsRangeKey(key string) bool { return rangeSeparator(key) >= 0 }
