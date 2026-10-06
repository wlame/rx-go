package timestamps

import (
	"fmt"
	"time"
)

// Stamp is the timestamp written on one line.
type Stamp struct {
	// Ms is milliseconds since the Unix epoch. When Zoned is true it is
	// the UTC instant; when false it is the wall-clock reading as if it
	// were UTC (the file frame).
	Ms int64
	// Zoned is true when the line carried a zone the package converts
	// (`Z`, a numeric offset, `UTC`, `GMT`), or was an epoch value.
	Zoned bool
	// OffsetMinutes is the zone offset east of UTC as written (`+02:00`
	// is 120). It is 0 when Zoned is false, and for `Z`, `UTC`, `GMT`
	// and epoch values.
	OffsetMinutes int
}

// Parser reads the timestamp of each line of one file. Build it once per
// file with [NewParser]; it holds no per-line state, so one Parser may
// be used from several goroutines at once.
type Parser struct {
	format   Format
	fam      *family
	dayFirst bool
	// mtimeMs and mtimeYear give a year-less family its year.
	mtimeMs   int64
	mtimeYear int
}

// NewParser returns the Parser for a file whose timestamps have format f
// and whose modification time is mtimeNs nanoseconds since the Unix
// epoch. The mtime is used only by a family that writes no year (see
// [Format.YearFromMtime]); pass the same mtime that identifies the file
// in its index, so the index and a cold scan read the same years.
//
// It returns an error wrapping [ErrInvalidFormat] when f is not valid.
func NewParser(f Format, mtimeNs int64) (*Parser, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	fam, _ := lookupFamily(f.Family)
	p := &Parser{
		format:    f,
		fam:       fam,
		mtimeMs:   floorDiv(mtimeNs, int64(time.Millisecond)),
		mtimeYear: time.Unix(0, mtimeNs).UTC().Year(),
	}
	if f.DayFirst != nil {
		// Keep a private copy: the caller's pointer must not be able to
		// change a Parser that other goroutines may be using.
		p.dayFirst = *f.DayFirst
		p.format.DayFirst = boolValue(p.dayFirst)
	}
	return p, nil
}

// Format returns the format the Parser reads. The result is a copy that
// shares nothing with the Parser.
func (p *Parser) Format() Format {
	f := p.format
	if f.DayFirst != nil {
		f.DayFirst = boolValue(*f.DayFirst)
	}
	return f
}

// String describes the parser for logs and error messages.
func (p *Parser) String() string {
	return fmt.Sprintf("timestamps.Parser{%s anchored=%t}", p.format.Family, p.format.Anchored)
}

// Own returns the timestamp written on line itself, and false when the
// line has none. line may end with its newline or not.
//
// Own looks at line[:WindowBytes] only, so a line of any length costs the
// same, and a timestamp counts only when it ends within the first
// WindowBytes-8 bytes (see maxEnd). In an anchored format the timestamp
// must start at column 0 (or at column 1 after a `[`); otherwise the
// first timestamp that starts after a byte that is not a letter or digit
// (for epoch values: after a `[`) counts.
//
// Own allocates nothing: every value it builds is a fixed-size struct
// returned by value, which Go keeps on the stack. It runs on every line
// of an index build, so a heap allocation here would cost one garbage
// object per line of a multi-gigabyte file.
func (p *Parser) Own(line []byte) (Stamp, bool) {
	w := line
	if len(w) > WindowBytes {
		w = w[:WindowBytes]
	}
	if p.format.Anchored {
		return p.anchored(w)
	}
	return p.windowed(w)
}

// anchored tries column 0 and, after a leading `[`, column 1.
func (p *Parser) anchored(w []byte) (Stamp, bool) {
	if s, ok := p.at(w, 0); ok {
		return s, true
	}
	if len(w) > 0 && w[0] == '[' {
		return p.at(w, 1)
	}
	return Stamp{}, false
}

// windowed returns the first valid timestamp that starts at a word
// boundary within w.
func (p *Parser) windowed(w []byte) (Stamp, bool) {
	for i := 0; i < len(w) && i < maxEnd; i++ {
		if !p.fam.startsWith[w[i]] || !p.fam.canStartAfter(w, i) {
			continue
		}
		if s, ok := p.at(w, i); ok {
			return s, true
		}
	}
	return Stamp{}, false
}

// canStartAfter reports whether a match of the family may start at w[i]
// given the byte before it. Without this rule `2026/10/06` would read as
// the slash date `26/10/06` from its third byte.
func (f *family) canStartAfter(w []byte, i int) bool {
	if i == 0 {
		return true
	}
	prev := w[i-1]
	if f.onlyAfterBracket {
		return prev == '['
	}
	return !isAlnum(prev)
}

// at reads one timestamp that starts at w[i] and turns it into a Stamp.
func (p *Parser) at(w []byte, i int) (Stamp, bool) {
	if i >= len(w) || !p.fam.startsWith[w[i]] {
		return Stamp{}, false
	}
	r, end, ok := p.fam.match(w, i)
	if !ok || end > maxEnd {
		return Stamp{}, false
	}
	return p.finish(r)
}

// whole reads s as one timestamp of the file's family that fills all of
// s. Queries use it to accept a value copied from a log line.
func (p *Parser) whole(s []byte) (Stamp, bool) {
	if len(s) == 0 || len(s) > maxEnd || !p.fam.startsWith[s[0]] {
		return Stamp{}, false
	}
	r, end, ok := p.fam.match(s, 0)
	if !ok || end != len(s) {
		return Stamp{}, false
	}
	return p.finish(r)
}

// finish checks that the fields name a real moment and converts them to
// a Stamp, applying the per-file facts: the day/month order of a slash
// date and the year of a year-less family.
func (p *Parser) finish(r fields) (Stamp, bool) {
	if r.isEpoch {
		return Stamp{Ms: r.epochMs, Zoned: true}, true
	}
	if p.fam.hasOrder && p.dayFirst {
		r.month, r.day = r.day, r.month
	}
	if !validClock(r) || r.month < 1 || r.month > 12 || r.day < 1 {
		return Stamp{}, false
	}
	var ms int64
	if p.fam.yearless {
		var ok bool
		ms, ok = inferYear(r, p.mtimeMs, p.mtimeYear)
		if !ok {
			return Stamp{}, false
		}
	} else {
		if r.day > daysIn(r.year, r.month) {
			return Stamp{}, false
		}
		ms = civilMs(r.year, r.month, r.day) + clockMs(r)
	}
	if !r.zoned {
		return Stamp{Ms: ms}, true
	}
	return Stamp{
		Ms:            ms - int64(r.offsetMinutes)*msPerMinute,
		Zoned:         true,
		OffsetMinutes: r.offsetMinutes,
	}, true
}
