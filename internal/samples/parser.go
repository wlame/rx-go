package samples

import (
	"fmt"
	"strconv"
	"strings"
)

// OffsetOrRange represents one item in a comma-separated samples spec.
// A single value has End == nil (negative allowed). A range has both
// ends non-negative with Start <= End.
//
// Examples produced by Parse("100,200-350,-5"):
//
//	{Start: 100}
//	{Start: 200, End: &350}
//	{Start: -5}
type OffsetOrRange struct {
	Start int64
	End   *int64
}

// ParseCSV parses a comma-separated spec into a slice of OffsetOrRange
// values. Empty or whitespace-only entries are rejected. Multi-range
// support per Stage 9 Round 2 R1-B4 user design: a single request can
// contain any mix of singles and ranges.
//
// Syntax:
//
//	"100"        → single, start=100
//	"-5"         → single, start=-5
//	"100-200"    → range [100, 200] (both >= 0, start <= end)
//	"100,200-300" → multi
func ParseCSV(spec string) ([]OffsetOrRange, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("empty spec")
	}
	parts := strings.Split(spec, ",")
	out := make([]OffsetOrRange, 0, len(parts))
	for _, p := range parts {
		v, err := parseOne(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// parseOne parses a single "100", "-5", or "100-200" token. Mirrors
// rx-python/src/rx/cli/samples.py::parse_offset_or_range semantics.
func parseOne(value string) (OffsetOrRange, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return OffsetOrRange{}, fmt.Errorf("empty value")
	}
	// Try parsing as a single integer first (covers positive AND
	// negative singles).
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return OffsetOrRange{Start: n}, nil
	}
	// Range format requires the value to contain a '-' but NOT start
	// with '-' (that would be a negative single). Pythonic check.
	if strings.Contains(value, "-") && !strings.HasPrefix(value, "-") {
		parts := strings.SplitN(value, "-", 2)
		if len(parts) != 2 {
			return OffsetOrRange{}, fmt.Errorf(
				"invalid range format: %s. Expected START-END", value)
		}
		start, e1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		end, e2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if e1 != nil || e2 != nil {
			return OffsetOrRange{}, fmt.Errorf(
				"invalid range format: %s. Both values must be integers", value)
		}
		if start < 0 || end < 0 {
			return OffsetOrRange{}, fmt.Errorf(
				"invalid range: %s. Ranges cannot contain negative values", value)
		}
		if start > end {
			return OffsetOrRange{}, fmt.Errorf(
				"invalid range: %s. Start must be <= end", value)
		}
		return OffsetOrRange{Start: start, End: &end}, nil
	}
	return OffsetOrRange{}, fmt.Errorf(
		"invalid offset: %s. Must be an integer or range (e.g., 100-200)", value)
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
// value). Single values can be negative; ranges cannot.
func (o OffsetOrRange) IsRange() bool { return o.End != nil }
