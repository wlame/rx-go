package timestamps

import (
	"errors"
	"fmt"
)

// Limits that every caller of the package shares. They are part of the
// answer, not tuning knobs: the indexed path and the cold path agree on a
// file's format only because both look at exactly these bytes.
const (
	// SampleBytes is how much of a file's text (decompressed for a
	// compressed file) detection looks at: the caller passes the first
	// SampleBytes bytes, or the whole text when it is shorter.
	SampleBytes = 1 << 20
	// SampleLines is the most sample lines detection looks at.
	SampleLines = 5000
	// WindowBytes is how much of one line a matcher looks at.
	WindowBytes = 128
)

// lookahead is the most bytes a matcher examines past the end of the
// timestamp it reads, to decide where the timestamp ends: a space, a
// sign and `HH:MM` of a zone, then the byte after it.
const lookahead = 8

// maxEnd is where a timestamp must end at the latest. Every byte a
// matcher examines for a timestamp that ends there lies inside the
// window, so a line cut at WindowBytes reads exactly as the whole line
// would: `12:34:56` cut after `12:34:5` can never pass for 12:34:05.
const maxEnd = WindowBytes - lookahead

// Family names one way of writing a date and time. The names are stored
// in an index, so they never change.
type Family string

// The families, in the order detection tries them (more specific first).
// When two families match the same number of sample lines, the earlier
// one wins.
const (
	// FamilyISO is ISO 8601 and its everyday relatives:
	// `2026-10-06T12:34:56.123+02:00`, `2026-10-06 12:34:56,123`,
	// `2025-2-15 12:34:5:123`, `2026/10/06 12:34:56 UTC`.
	FamilyISO Family = "iso"
	// FamilyCLF is the Apache/nginx access-log form:
	// `[06/Oct/2026:12:34:56 +0000]`.
	FamilyCLF Family = "clf"
	// FamilyCtime is the C library's ctime form, used by the Apache
	// error log: `Tue Oct 06 12:34:56.123456 2026`.
	FamilyCtime Family = "ctime"
	// FamilySyslog is the BSD syslog form, which has no year:
	// `Oct  6 12:34:56`, `Dec 10 12:34:56.123`.
	FamilySyslog Family = "syslog"
	// FamilySlash is a date with slashes and the year last:
	// `10/06/2026 12:34:56 PM`, `06/10/26 14:34:56`. Whether the first
	// number is the month or the day is fixed per file.
	FamilySlash Family = "slash"
	// FamilyDotted is the European dotted date, day first:
	// `06.10.2026 12:34:56`.
	FamilyDotted Family = "dotted"
	// FamilyEpoch is a Unix time in seconds (10 digits, optional
	// fraction) or milliseconds (13 digits), between the years 2000 and
	// 2100, at the start of the line or right after a `[`.
	FamilyEpoch Family = "epoch"
)

// Format is the timestamp format of one file, as [Detect] decides it. It
// is stored in the file's index, so its JSON field names are part of the
// cache format.
type Format struct {
	// Family is how the timestamps are written.
	Family Family `json:"format"`
	// Anchored is true when the timestamp starts each line: at column 0,
	// or at column 1 right after a `[` at column 0. An anchored file never
	// takes a timestamp from anywhere else in a line, so a date inside a
	// message ("expires 2099-01-01") or a continuation line does not
	// count. When Anchored is false the first timestamp that ends within
	// the line's first WindowBytes bytes counts.
	Anchored bool `json:"anchored"`
	// DayFirst is set for FamilySlash only: true when the first number of
	// the date is the day (`06/10/2026` is 6 October), false when it is
	// the month. It is nil for every other family.
	DayFirst *bool `json:"day_first"`
	// HasZone is true when most timestamps in the sample carry a zone.
	// Every line is read with or without a zone regardless; HasZone says
	// which frame the file's values are in (UTC instants, or wall-clock
	// readings).
	HasZone bool `json:"has_zone"`
}

// ErrInvalidFormat is returned for a Format that names no known family or
// whose fields contradict each other, such as one read from a damaged
// index.
var ErrInvalidFormat = errors.New("invalid timestamp format")

// Validate reports whether f describes a format this package can parse.
func (f Format) Validate() error {
	fam, ok := lookupFamily(f.Family)
	if !ok {
		return fmt.Errorf("%w: unknown family %q", ErrInvalidFormat, f.Family)
	}
	if fam.hasOrder && f.DayFirst == nil {
		return fmt.Errorf("%w: family %q needs day_first", ErrInvalidFormat, f.Family)
	}
	if !fam.hasOrder && f.DayFirst != nil {
		return fmt.Errorf("%w: family %q takes no day_first", ErrInvalidFormat, f.Family)
	}
	return nil
}

// YearFromMtime reports whether the family writes no year, so that the
// year of each timestamp comes from the file's modification time. Such a
// file's values depend on its mtime: a copy with another mtime year can
// read differently.
func (f Format) YearFromMtime() bool {
	fam, ok := lookupFamily(f.Family)
	return ok && fam.yearless
}

// matchFunc tries to read one timestamp of a family that starts at b[i].
// It returns the fields as written and the index just past the
// timestamp. It checks the syntax only; whether the date exists is
// decided later by finish, which also applies the per-file facts (the
// day/month order, the year of a year-less family).
//
// INVARIANT: a matchFunc never reads past len(b), never allocates, and
// returns end > i when ok.
type matchFunc func(b []byte, i int) (r fields, end int, ok bool)

// family is one row of the family table.
type family struct {
	name  Family
	match matchFunc
	// startsWith marks the bytes a timestamp of this family can start
	// with, so a windowed scan skips a position with one table lookup
	// instead of a call.
	startsWith *[256]bool
	// onlyAfterBracket restricts a match that is not at column 0 to
	// start right after a `[`. Otherwise a match may start after any
	// byte that is not a letter or a digit.
	onlyAfterBracket bool
	// hasOrder marks the family whose first two date numbers are month
	// and day in an order fixed per file.
	hasOrder bool
	// yearless marks the family that writes no year.
	yearless bool
}

// families is the family table, in detection order. It is a slice, not
// a map, because the order breaks ties and must be the same on every
// run.
var families = []family{
	{name: FamilyISO, match: matchISO, startsWith: &digitStart},
	{name: FamilyCLF, match: matchCLF, startsWith: &bracketStart},
	{name: FamilyCtime, match: matchCtime, startsWith: &letterStart},
	{name: FamilySyslog, match: matchSyslog, startsWith: &letterStart, yearless: true},
	{name: FamilySlash, match: matchSlash, startsWith: &digitStart, hasOrder: true},
	{name: FamilyDotted, match: matchDotted, startsWith: &digitStart},
	{name: FamilyEpoch, match: matchEpoch, startsWith: &digitStart, onlyAfterBracket: true},
}

// lookupFamily returns the table row for a family name.
func lookupFamily(name Family) (*family, bool) {
	for i := range families {
		if families[i].name == name {
			return &families[i], true
		}
	}
	return nil, false
}

// Byte classes for family.startsWith. Arrays indexed by the byte value
// are the cheapest test Go offers: one bounds-check-free load.
var (
	digitStart   = byteClass("0123456789")
	bracketStart = byteClass("[")
	letterStart  = byteClass("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
)

// byteClass builds a lookup table that is true for each byte of set.
func byteClass(set string) [256]bool {
	var t [256]bool
	for i := 0; i < len(set); i++ {
		t[set[i]] = true
	}
	return t
}
