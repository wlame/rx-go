package timestamps

import (
	"errors"
	"fmt"
	"strings"
)

// MaxQueryBytes bounds the length of one query value. A query arrives
// from an HTTP parameter, so the parser refuses a longer value before
// it looks at it; the longest accepted form is well under 100 bytes.
const MaxQueryBytes = 256

// rangeSeparator splits a range query. It is never a comma: a comma is
// part of Python's timestamp format (`12:34:56,123`).
const rangeSeparator = ".."

// ErrInvalidQuery is wrapped by every error [ParseQuery] and [Resolve]
// return for a query the caller should refuse as a usage error.
var ErrInvalidQuery = errors.New("invalid timestamp query")

// acceptedForms is the end of every parse error message.
const acceptedForms = "accepted: a date and time as in ISO 8601 or RFC 3339, with or without " +
	"seconds, fraction and zone (2026-10-06T12:34:56.789+02:00, 2026-10-06 12:34); a date " +
	"(2026-10-06); a time of day (14:33:12, 14:33, 2:33:12 PM); epoch seconds or " +
	"milliseconds (1759754096, 1759754096.5, 1759754096789); or a timestamp as the file " +
	"writes it; a range is A..B, A.. or ..B"

// EndpointKind says what a query endpoint holds and what it still needs
// to become a moment in a file.
type EndpointKind uint8

// The endpoint kinds.
const (
	// EndpointOpen is the missing side of a range (`..B`, `A..`), and
	// the unused end of a single query.
	EndpointOpen EndpointKind = iota
	// EndpointInstant holds a UTC instant: the value carried a zone, or
	// was an epoch time.
	EndpointInstant
	// EndpointWall holds a date and time with no zone, as its
	// wall-clock reading as if it were UTC. Resolving it needs a zone.
	EndpointWall
	// EndpointTimeOfDay holds a time with no date and no zone, as
	// milliseconds since midnight. Resolving it needs the file's date
	// and a zone.
	EndpointTimeOfDay
)

// Endpoint is one end of a query, as parsed.
type Endpoint struct {
	Kind EndpointKind
	// Ms is the value; its meaning depends on Kind.
	Ms int64
}

// Query is a parsed timestamp query: a single moment (`T`) or a range
// (`T1..T2`, `..T2`, `T1..`).
type Query struct {
	// Value is the query as the caller gave it.
	Value string
	// Range is true for a range, even one with an open end.
	Range bool
	// Start is the moment of a single query, or the start of a range.
	Start Endpoint
	// End is the end of a range; EndpointOpen for a single query.
	End Endpoint
}

// NeedsSpan reports whether resolving q needs the file's first and last
// timestamps (ResolveContext.FirstMs and LastMs): only a time of day
// with no date does. A caller reads them only then.
func (q Query) NeedsSpan() bool {
	return q.Start.Kind == EndpointTimeOfDay || q.End.Kind == EndpointTimeOfDay
}

// ParseQuery parses one query value. p is the Parser of the file the
// query is about, so a timestamp copied from one of its lines parses as
// the line does (a syslog time takes the year the line takes); p may be
// nil.
//
// Each endpoint may be, in the order tried:
//
//   - a timestamp as the file writes it, filling the endpoint, with one
//     pair of `[ ]` around it ignored, and a trailing zone word the file's
//     lines do not convert (`MST`) ignored too;
//   - epoch seconds (10 digits, optional fraction) or milliseconds (13
//     digits), from the year 2000 up to 2100;
//   - an ISO 8601 or RFC 3339 date and time, `-` or `/` in the date, `T`
//     or a space before the time, seconds and fraction optional, with or
//     without a zone (`Z`, `±HH:MM`, `±HHMM`, `±HH`, ` UTC`, ` GMT`);
//   - a date alone, meaning 00:00:00.000 that day;
//   - a time of day alone (`14:33`, `14:33:12.5`, `2:33:12 PM`).
//
// Spaces around the value and around `..` are ignored. A value is never
// split on commas. Errors wrap [ErrInvalidQuery] and list the accepted
// forms.
func ParseQuery(value string, p *Parser) (Query, error) {
	q := Query{Value: value}
	if len(value) > MaxQueryBytes {
		return q, fmt.Errorf("%w: value is %d bytes, longer than %d; %s",
			ErrInvalidQuery, len(value), MaxQueryBytes, acceptedForms)
	}
	text := strings.TrimSpace(value)
	if text == "" {
		return q, fmt.Errorf("%w: empty value; %s", ErrInvalidQuery, acceptedForms)
	}
	before, after, isRange := strings.Cut(text, rangeSeparator)
	if !isRange {
		start, err := parseEndpoint(value, text, p)
		q.Start = start
		return q, err
	}
	q.Range = true
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)
	if strings.Contains(after, rangeSeparator) {
		return q, fmt.Errorf("%w %q: more than one %q; %s", ErrInvalidQuery, value, rangeSeparator, acceptedForms)
	}
	if before == "" && after == "" {
		return q, fmt.Errorf("%w %q: a range needs at least one end; %s", ErrInvalidQuery, value, acceptedForms)
	}
	var err error
	if before != "" {
		if q.Start, err = parseEndpoint(value, before, p); err != nil {
			return q, err
		}
	}
	if after != "" {
		if q.End, err = parseEndpoint(value, after, p); err != nil {
			return q, err
		}
	}
	return q, nil
}

// endpointParsers are the generic endpoint forms, tried in order after
// the file's own format.
var endpointParsers = []func(b []byte) (Endpoint, bool){
	epochEndpoint,
	isoEndpoint,
	timeOfDayEndpoint,
}

// parseEndpoint parses one trimmed endpoint text of the query value.
func parseEndpoint(value, text string, p *Parser) (Endpoint, error) {
	b := []byte(text)
	if p != nil {
		if e, ok := ownFormatEndpoint(b, p); ok {
			return e, nil
		}
	}
	for _, parse := range endpointParsers {
		if e, ok := parse(b); ok {
			return e, nil
		}
	}
	if text == value {
		return Endpoint{}, fmt.Errorf("%w %q: not a timestamp; %s", ErrInvalidQuery, value, acceptedForms)
	}
	return Endpoint{}, fmt.Errorf("%w %q: %q is not a timestamp; %s", ErrInvalidQuery, value, text, acceptedForms)
}

// ownFormatEndpoint reads b as a timestamp in the file's own format.
func ownFormatEndpoint(b []byte, p *Parser) (Endpoint, bool) {
	tries := [][]byte{b}
	if len(b) >= 2 && b[0] == '[' && b[len(b)-1] == ']' {
		tries = append(tries, b[1:len(b)-1])
	}
	for _, t := range tries {
		for _, candidate := range [][]byte{t, withoutZoneWord(t)} {
			if s, ok := p.whole(candidate); ok {
				return stampEndpoint(s), true
			}
		}
	}
	return Endpoint{}, false
}

// Lengths of a zone abbreviation a query may end with (`MST`, `CEST`,
// `AEST`). Two letters are refused: `PM` is not a zone.
const (
	minZoneWordLetters = 3
	maxZoneWordLetters = 5
)

// withoutZoneWord drops a trailing zone abbreviation the package does
// not convert (` MST`, ` CEST`): the file's line reads as zone-less, so
// the same text as a query must too. It returns b unchanged when b ends
// with no such word.
func withoutZoneWord(b []byte) []byte {
	j := len(b)
	for j > 0 && b[j-1] >= 'A' && b[j-1] <= 'Z' {
		j--
	}
	letters := len(b) - j
	if letters < minZoneWordLetters || letters > maxZoneWordLetters || j == 0 || b[j-1] != ' ' {
		return b
	}
	return b[:j-1]
}

// stampEndpoint turns a parsed line timestamp into an endpoint.
func stampEndpoint(s Stamp) Endpoint {
	if s.Zoned {
		return Endpoint{Kind: EndpointInstant, Ms: s.Ms}
	}
	return Endpoint{Kind: EndpointWall, Ms: s.Ms}
}

// epochEndpoint reads epoch seconds or milliseconds filling b.
func epochEndpoint(b []byte) (Endpoint, bool) {
	r, end, ok := matchEpoch(b, 0)
	if !ok || end != len(b) {
		return Endpoint{}, false
	}
	return Endpoint{Kind: EndpointInstant, Ms: r.epochMs}, true
}

// isoEndpoint reads an ISO date, with or without a time, filling b.
func isoEndpoint(b []byte) (Endpoint, bool) {
	var r fields
	j, ok := 0, false
	if r.year, j, ok = number(b, j, 4, 4); !ok || j >= len(b) || (b[j] != '-' && b[j] != '/') {
		return Endpoint{}, false
	}
	sep := b[j]
	if r.month, j, ok = number(b, j+1, 1, 2); !ok {
		return Endpoint{}, false
	}
	if j, ok = expect(b, j, sep); !ok {
		return Endpoint{}, false
	}
	if r.day, j, ok = number(b, j, 1, 2); !ok {
		return Endpoint{}, false
	}
	if r.month < 1 || r.month > 12 || r.day < 1 || r.day > daysIn(r.year, r.month) {
		return Endpoint{}, false
	}
	date := civilMs(r.year, r.month, r.day)
	if j == len(b) {
		return Endpoint{Kind: EndpointWall, Ms: date}, true
	}
	if b[j] != 'T' && b[j] != ' ' {
		return Endpoint{}, false
	}
	var tod int64
	if tod, j, ok = queryClock(b, j+1); !ok {
		return Endpoint{}, false
	}
	var offset int
	var zoned bool
	if offset, zoned, j, ok = isoZone(b, j); !ok {
		return Endpoint{}, false
	}
	if !zoned {
		b = withoutZoneWord(b)
	}
	if j != len(b) {
		return Endpoint{}, false
	}
	if zoned {
		return Endpoint{Kind: EndpointInstant, Ms: date + tod - int64(offset)*msPerMinute}, true
	}
	return Endpoint{Kind: EndpointWall, Ms: date + tod}, true
}

// timeOfDayEndpoint reads a time with no date filling b, with an
// optional AM/PM.
func timeOfDayEndpoint(b []byte) (Endpoint, bool) {
	tod, j, ok := queryClock(b, 0)
	if !ok {
		return Endpoint{}, false
	}
	hour := int(tod / msPerHour)
	var hour24 int
	if hour24, j, ok = amPM(b, j, hour); !ok || j != len(b) {
		return Endpoint{}, false
	}
	return Endpoint{Kind: EndpointTimeOfDay, Ms: tod + int64(hour24-hour)*msPerHour}, true
}

// queryClock reads `H:M`, `H:M:S` or `H:M:S.fff` (fields of one or two
// digits, fraction after `.` or `,`) and returns milliseconds since
// midnight.
func queryClock(b []byte, j int) (tod int64, next int, ok bool) {
	var r fields
	if r.hour, j, ok = number(b, j, 1, 2); !ok {
		return 0, j, false
	}
	if j, ok = expect(b, j, ':'); !ok {
		return 0, j, false
	}
	if r.minute, j, ok = number(b, j, 1, 2); !ok {
		return 0, j, false
	}
	if j < len(b) && b[j] == ':' {
		if r.second, j, ok = number(b, j+1, 1, 2); !ok {
			return 0, j, false
		}
		if r.milli, j, ok = decimalFraction(b, j); !ok {
			return 0, j, false
		}
	}
	if !validClock(r) {
		return 0, j, false
	}
	return clockMs(r), j, true
}
