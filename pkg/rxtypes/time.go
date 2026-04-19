package rxtypes

import (
	"fmt"
	"strings"
	"time"
)

// isoTimeLayout is the exact format Python's datetime.isoformat() emits
// for UTC-naive datetimes: YYYY-MM-DDTHH:MM:SS.microseconds, NO timezone
// suffix. Go's reference time is "2006-01-02T15:04:05.000000".
//
// Decision 5.7 in stage-5-decisions.md locks this layout.
const isoTimeLayout = "2006-01-02T15:04:05.000000"

// ISOTime is a time.Time wrapper whose JSON form matches Python's
// datetime.isoformat() output: "2006-01-02T15:04:05.000000" in UTC,
// 6-digit microseconds, NO timezone suffix ("Z" or offset).
//
// WHY a custom type: Go's default time.Time marshaller emits RFC3339 with
// nanosecond precision and a "Z" suffix. rx-python writes
// ~/.cache/rx/ JSON artifacts using the Python layout; if the Go port
// re-emitted those timestamps in Go's default shape, the cache files
// would fail the round-trip parity invariant (spec §6.3).
//
// Round-trip behavior:
//
//	MarshalJSON always writes 6-digit microseconds in UTC (lossy: nanosecond
//	precision is truncated).
//
//	UnmarshalJSON accepts the Python layout first, then RFC3339Nano, then
//	RFC3339. We accept both because the Go server may ingest its OWN
//	output (stdlib time.Time) as well as Python's.
//
// Embedding time.Time instead of wrapping as a named field means every
// time.Time method is forwarded automatically (.Year, .Before, etc.).
type ISOTime struct {
	time.Time
}

// NewISOTime constructs an ISOTime from a time.Time. Equivalent to
// ISOTime{t} but more readable at call sites.
func NewISOTime(t time.Time) ISOTime {
	return ISOTime{Time: t}
}

// MarshalJSON emits the Python-compatible layout in UTC. Nanosecond
// precision beyond microseconds is TRUNCATED (not rounded) because
// Python's datetime type has only microsecond resolution and we want
// byte-identical round trips between Go and Python.
func (t ISOTime) MarshalJSON() ([]byte, error) {
	// Zero value → "null" to match JSON semantics for optional times.
	// IsZero is forwarded via the embedded time.Time; using the bare
	// method keeps the call-site succinct and avoids staticcheck QF1008.
	if t.IsZero() {
		return []byte("null"), nil
	}
	// Truncate to microsecond precision before formatting.
	truncated := t.UTC().Truncate(time.Microsecond)
	return []byte(`"` + truncated.Format(isoTimeLayout) + `"`), nil
}

// UnmarshalJSON accepts the following layouts, in priority order:
//
//  1. Python layout: "2006-01-02T15:04:05.000000" (our canonical form).
//  2. Python layout with shorter fractional: "2006-01-02T15:04:05"
//     (no microseconds). Python drops trailing zeroes on some versions.
//  3. RFC3339Nano: "2006-01-02T15:04:05.999999999Z07:00".
//  4. RFC3339: "2006-01-02T15:04:05Z07:00".
//
// The literal JSON token "null" decodes to a zero time.Time with no
// error — mirroring how encoding/json handles *time.Time fields.
func (t *ISOTime) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" {
		*t = ISOTime{}
		return nil
	}
	// Strip surrounding quotes. We accept single or double quotes
	// defensively even though valid JSON must use double.
	s = strings.Trim(s, `"`)
	if s == "" {
		*t = ISOTime{}
		return nil
	}

	// Try each layout in order until one succeeds.
	layouts := []string{
		isoTimeLayout,           // Python microseconds, no TZ
		"2006-01-02T15:04:05",   // Python, second precision
		time.RFC3339Nano,        // RFC3339 with fractional seconds
		time.RFC3339,            // RFC3339, second precision
		"2006-01-02T15:04:05.9", // Python trailing-zero variant (1+ digits)
	}
	var lastErr error
	for _, layout := range layouts {
		parsed, err := time.Parse(layout, s)
		if err == nil {
			t.Time = parsed
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("ISOTime: cannot parse %q: %w", s, lastErr)
}

// String returns the ISO layout (helpful for log messages and templates).
func (t ISOTime) String() string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Microsecond).Format(isoTimeLayout)
}
