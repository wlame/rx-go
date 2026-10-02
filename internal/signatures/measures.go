package signatures

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MeasureParams holds the parameters of one measure, defaults applied
// and ranges checked. It is implemented by exactly the *Params types of
// this file, one per measure name.
type MeasureParams interface {
	// Canonical gives deterministic bytes for the parameters: the
	// measure's name and every parameter in a fixed order. A findings
	// cache key hashes them, so two definitions with the same values
	// give the same bytes.
	Canonical() []byte
	// Fields lists the parameters by their YAML names, in the order the
	// measure table declares them. A value is an int, a float64, or a
	// string for a duration ("60s", "auto").
	Fields() []Param
}

// Param is one measure parameter: its YAML name and its value (an int,
// a float64, or a string for a duration).
type Param struct {
	Name  string
	Value any
}

// LongLineParams are the parameters of "long-line": a line whose full
// length is at least max(MinBytes, Factor × the median length of the
// Window lines on each side).
type LongLineParams struct{ Window, Factor, MinBytes int }

// BinaryParams are the parameters of "binary", which has none.
type BinaryParams struct{}

// EntropyParams are the parameters of "entropy": a token of at least
// MinLength bytes whose Shannon entropy reaches a share of the entropy a
// random token of its length has on average. A token of the base64
// alphabet needs Ratio of it over 64 symbols and an upper-case letter,
// a lower-case letter and a digit; a token of hex digits only needs
// HexRatio of it over 16 symbols, and a HexRatio of 0 leaves every hex
// token alone.
type EntropyParams struct {
	MinLength       int
	Ratio, HexRatio float64
}

// JSONBlockParams are the parameters of "json-block".
type JSONBlockParams struct{ MaxLines int }

// UnprefixedParams are the parameters of "unprefixed": a run of at
// least Min lines without a timestamp at their start, looked for only
// when stamped lines are at least MinShare of the file.
type UnprefixedParams struct {
	Min      int
	MinShare float64
}

// DensityParams are the parameters of "density". Bucket 0 means
// "auto": about 1,000 buckets over the file's time span.
type DensityParams struct {
	Bucket    time.Duration
	Factor, Z float64
	MinCount  int
}

// TimeGapParams are the parameters of "time-gap": a silence of at
// least max(MinGap, Factor × the median interval).
type TimeGapParams struct {
	MinGap time.Duration
	Factor float64
}

// TimeBackwardsParams are the parameters of "time-backwards".
type TimeBackwardsParams struct{ Tolerance time.Duration }

// Base64RunParams are the parameters of "base64-run": a run of at least
// MinLength base64 bytes inside a line.
type Base64RunParams struct{ MinLength int }

// Fields lists the parameters of long-line.
func (p LongLineParams) Fields() []Param {
	return []Param{{"window", p.Window}, {"factor", p.Factor}, {"min_bytes", p.MinBytes}}
}

// Canonical gives the deterministic bytes of the parameters.
func (p LongLineParams) Canonical() []byte { return canonical("long-line", p.Fields()) }

// Fields lists the parameters of binary: none.
func (BinaryParams) Fields() []Param { return []Param{} }

// Canonical gives the deterministic bytes of the parameters.
func (p BinaryParams) Canonical() []byte { return canonical("binary", p.Fields()) }

// Fields lists the parameters of entropy.
func (p EntropyParams) Fields() []Param {
	return []Param{{"min_length", p.MinLength}, {"ratio", p.Ratio}, {"hex_ratio", p.HexRatio}}
}

// Canonical gives the deterministic bytes of the parameters.
func (p EntropyParams) Canonical() []byte { return canonical("entropy", p.Fields()) }

// Fields lists the parameters of json-block.
func (p JSONBlockParams) Fields() []Param { return []Param{{"max_lines", p.MaxLines}} }

// Canonical gives the deterministic bytes of the parameters.
func (p JSONBlockParams) Canonical() []byte { return canonical("json-block", p.Fields()) }

// Fields lists the parameters of unprefixed.
func (p UnprefixedParams) Fields() []Param {
	return []Param{{"min", p.Min}, {"min_share", p.MinShare}}
}

// Canonical gives the deterministic bytes of the parameters.
func (p UnprefixedParams) Canonical() []byte { return canonical("unprefixed", p.Fields()) }

// Fields lists the parameters of density; a zero bucket is "auto".
func (p DensityParams) Fields() []Param {
	return []Param{{"bucket", formatBucket(p.Bucket)}, {"factor", p.Factor}, {"z", p.Z}, {"min_count", p.MinCount}}
}

// Canonical gives the deterministic bytes of the parameters.
func (p DensityParams) Canonical() []byte { return canonical("density", p.Fields()) }

// Fields lists the parameters of time-gap.
func (p TimeGapParams) Fields() []Param {
	return []Param{{"min_gap", formatDuration(p.MinGap)}, {"factor", p.Factor}}
}

// Canonical gives the deterministic bytes of the parameters.
func (p TimeGapParams) Canonical() []byte { return canonical("time-gap", p.Fields()) }

// Fields lists the parameters of time-backwards.
func (p TimeBackwardsParams) Fields() []Param {
	return []Param{{"tolerance", formatDuration(p.Tolerance)}}
}

// Canonical gives the deterministic bytes of the parameters.
func (p TimeBackwardsParams) Canonical() []byte { return canonical("time-backwards", p.Fields()) }

// Fields lists the parameters of base64-run.
func (p Base64RunParams) Fields() []Param { return []Param{{"min_length", p.MinLength}} }

// Canonical gives the deterministic bytes of the parameters.
func (p Base64RunParams) Canonical() []byte { return canonical("base64-run", p.Fields()) }

// canonical writes "name;param=value;param=value", every value in one
// fixed text form: an int in decimal, a float64 in its shortest exact
// form ('g', -1), a string as it is.
func canonical(name string, fields []Param) []byte {
	var b strings.Builder
	b.WriteString(name)
	for _, f := range fields {
		b.WriteByte(';')
		b.WriteString(f.Name)
		b.WriteByte('=')
		b.WriteString(paramText(f.Value))
	}
	return []byte(b.String())
}

// paramText is the text form of one parameter value.
func paramText(v any) string {
	switch x := v.(type) {
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// paramKind is the type of a measure parameter as it is written in
// YAML.
type paramKind int

const (
	// paramInt is a whole number.
	paramInt paramKind = iota
	// paramFloat is a number, whole or not.
	paramFloat
	// paramDuration is a duration with a unit: "500ms", "60s", "5m",
	// "2h", "1d".
	paramDuration
	// paramBucket is a duration or "auto" (stored as 0).
	paramBucket
)

// paramSpec declares one measure parameter: its YAML name, its type,
// its default and its inclusive range. Numbers are float64 whatever the
// kind; a duration is in seconds.
type paramSpec struct {
	name          string
	kind          paramKind
	def, min, max float64
}

// paramValues are the values of one measure's parameters after parsing,
// defaults applied: by name, as float64 (a duration in seconds).
type paramValues map[string]float64

// int returns a whole-number parameter.
func (v paramValues) int(name string) int { return int(v[name]) }

// float returns a number parameter.
func (v paramValues) float(name string) float64 { return v[name] }

// duration returns a duration parameter. The seconds are rounded to the
// nanosecond, so 0.001 s is exactly one millisecond.
func (v paramValues) duration(name string) time.Duration {
	return time.Duration(math.Round(v[name] * float64(time.Second)))
}

// measureDef is a row of the measure table: the parameters of one
// measure and how their values become its typed parameters.
type measureDef struct {
	params []paramSpec
	build  func(paramValues) MeasureParams
	// removed names parameters an earlier form of the measure took,
	// each with the message that refuses it and says what replaced it.
	removed map[string]string
}

// day is the length of the "d" duration unit.
const day = 24 * time.Hour

// measureDefs is the measure table: every measure name the loader
// knows, with its parameters, defaults and ranges. Parsing, defaults,
// range checks and `rx signatures template` all read it.
var measureDefs = map[string]measureDef{
	"long-line": {
		params: []paramSpec{
			{name: "window", kind: paramInt, def: 100, min: 1, max: 10000},
			{name: "factor", kind: paramInt, def: 10, min: 1, max: 1000},
			{name: "min_bytes", kind: paramInt, def: 1024, min: 1, max: 1 << 30},
		},
		build: func(v paramValues) MeasureParams {
			return LongLineParams{Window: v.int("window"), Factor: v.int("factor"), MinBytes: v.int("min_bytes")}
		},
	},
	"binary": {
		params: []paramSpec{},
		build:  func(paramValues) MeasureParams { return BinaryParams{} },
	},
	"entropy": {
		params: []paramSpec{
			{name: "min_length", kind: paramInt, def: 20, min: 8, max: 1024},
			{name: "ratio", kind: paramFloat, def: 0.94, min: 0.1, max: 1.5},
			{name: "hex_ratio", kind: paramFloat, def: 0, min: 0, max: 1.5},
		},
		build: func(v paramValues) MeasureParams {
			return EntropyParams{MinLength: v.int("min_length"), Ratio: v.float("ratio"), HexRatio: v.float("hex_ratio")}
		},
		removed: map[string]string{
			"base64": "base64 was replaced by ratio: a token needs ratio (default 0.94) times the entropy a random token of its length has",
			"hex":    "hex was replaced by hex_ratio: hex tokens are left alone unless hex_ratio is set, as a share of a random hex token's entropy",
		},
	},
	"json-block": {
		params: []paramSpec{
			{name: "max_lines", kind: paramInt, def: 200, min: 2, max: 10000},
		},
		build: func(v paramValues) MeasureParams { return JSONBlockParams{MaxLines: v.int("max_lines")} },
	},
	"unprefixed": {
		params: []paramSpec{
			{name: "min", kind: paramInt, def: 5, min: 1, max: 1000000},
			{name: "min_share", kind: paramFloat, def: 0.5, min: 0, max: 1},
		},
		build: func(v paramValues) MeasureParams {
			return UnprefixedParams{Min: v.int("min"), MinShare: v.float("min_share")}
		},
	},
	"base64-run": {
		params: []paramSpec{
			{name: "min_length", kind: paramInt, def: 100, min: 16, max: 1 << 20},
		},
		build: func(v paramValues) MeasureParams { return Base64RunParams{MinLength: v.int("min_length")} },
	},
	"density": {
		params: []paramSpec{
			{name: "bucket", kind: paramBucket, def: 0, min: 1, max: 7 * day.Seconds()},
			{name: "factor", kind: paramFloat, def: 5, min: 1, max: 1000},
			{name: "z", kind: paramFloat, def: 3.5, min: 0, max: 100},
			{name: "min_count", kind: paramInt, def: 100, min: 1, max: 1e9},
		},
		build: func(v paramValues) MeasureParams {
			return DensityParams{Bucket: v.duration("bucket"), Factor: v.float("factor"), Z: v.float("z"), MinCount: v.int("min_count")}
		},
	},
	"time-gap": {
		params: []paramSpec{
			{name: "min_gap", kind: paramDuration, def: 60, min: 0.001, max: 365 * day.Seconds()},
			{name: "factor", kind: paramFloat, def: 50, min: 1, max: 10000},
		},
		build: func(v paramValues) MeasureParams {
			return TimeGapParams{MinGap: v.duration("min_gap"), Factor: v.float("factor")}
		},
	},
	"time-backwards": {
		params: []paramSpec{
			{name: "tolerance", kind: paramDuration, def: 1, min: 0, max: day.Seconds()},
		},
		build: func(v paramValues) MeasureParams { return TimeBackwardsParams{Tolerance: v.duration("tolerance")} },
	},
}

// MeasureNames returns the names of every measure the loader knows, in
// alphabetical order.
func MeasureNames() []string {
	names := make([]string, 0, len(measureDefs))
	for name := range measureDefs {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// defaultMeasureParams returns a measure's parameters with every
// default applied, or false for a name the table does not hold.
func defaultMeasureParams(name string) (MeasureParams, bool) {
	def, ok := measureDefs[name]
	if !ok {
		return nil, false
	}
	values := paramValues{}
	for _, p := range def.params {
		values[p.name] = p.def
	}
	return def.build(values), true
}

// durationUnits are the units a duration may be written in, largest
// first: formatDuration writes the largest that divides the value.
var durationUnits = []struct {
	suffix string
	unit   time.Duration
}{
	{"d", day},
	{"h", time.Hour},
	{"m", time.Minute},
	{"s", time.Second},
	{"ms", time.Millisecond},
}

// maxDurationDays is the longest duration a measure parameter may be
// written as, in days (about ten years). Every parameter's own range is
// far shorter; the bound is checked before a day count is multiplied
// into a time.Duration, so a large count is refused rather than wrapped
// around to a small value.
const maxDurationDays = 3650

// parseDuration reads a duration as a measure parameter writes it: a
// whole number of days ("1d"), or Go's duration syntax ("500ms", "60s",
// "1h30m"). It refuses a negative duration and one longer than
// maxDurationDays days.
func parseDuration(text string) (time.Duration, error) {
	tooLong := fmt.Errorf("%q is longer than %dd", text, maxDurationDays)
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 64)
		switch {
		case err != nil && errors.Is(err, strconv.ErrRange):
			return 0, tooLong
		case err != nil || n < 0:
			return 0, fmt.Errorf("%q is not a duration: write a whole number of days, such as 1d", text)
		case n > maxDurationDays:
			return 0, tooLong
		}
		return time.Duration(n) * day, nil
	}
	// time.ParseDuration refuses a value that does not fit a
	// time.Duration with the same error as a typo, so one message
	// covers both.
	d, err := time.ParseDuration(text)
	switch {
	case err != nil:
		return 0, fmt.Errorf("%q is not a duration of at most %dd: write a number and a unit (ms, s, m, h, d), such as 60s",
			text, maxDurationDays)
	case d < 0:
		return 0, fmt.Errorf("%q is negative: a duration is 0 or more", text)
	case d > maxDurationDays*day:
		return 0, tooLong
	}
	return d, nil
}

// formatDuration writes a duration in the largest unit that divides it
// exactly ("60s" is "1m", "90s" stays "90s"); one under a millisecond
// is written in Go's syntax.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	for _, u := range durationUnits {
		if d%u.unit == 0 {
			return strconv.FormatInt(int64(d/u.unit), 10) + u.suffix
		}
	}
	return d.String()
}

// formatBucket writes a density bucket: "auto" for 0, else the
// duration.
func formatBucket(d time.Duration) string {
	if d == 0 {
		return "auto"
	}
	return formatDuration(d)
}
