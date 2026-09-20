package timestamps

import "bytes"

// Detection thresholds.
const (
	// minAnchoredLines is how many sample lines must start with a
	// timestamp for a family to qualify as anchored. A timestamp at
	// column 0 is strong evidence of a log, so the share does not matter:
	// a postgres log whose query-plan lines start with a tab has only a
	// few percent of its lines timestamped.
	minAnchoredLines = 3
	// minAnchoredPercent is the share of the non-blank sample lines an
	// anchored qualifier needs to outrank a windowed one. Log lines often
	// carry text an outside party writes (a request path, a message with
	// a newline in it), so a handful of injected lines that start with a
	// timestamp must not take over a file whose every line carries one
	// further in. Below this share the anchored qualifier wins only when
	// no windowed one qualifies. A postgres log has a few percent.
	minAnchoredPercent = 1
	// minDayFirstLines is how many sample lines must read day first only
	// (their first number is above 12) before a slash file is read day
	// first, and then only when they outnumber the lines that read month
	// first only. One injected `13/01/2026` line must not turn every
	// `10/06/2026` of the file into 10 June.
	minDayFirstLines = 3
)

// candidate is one way detection tries to read a file: a family, anchored
// or windowed. The slash family has two parsers, month first and day
// first; the others have one.
type candidate struct {
	family   int // index into families; lower wins a tie
	anchored bool
	parsers  []*Parser
}

// tally counts what one candidate found in the sample.
type tally struct {
	// read is the number of non-blank sample lines every parser of the
	// candidate reads a timestamp from. For a family with one parser it
	// is every line the candidate reads; for the slash family it is the
	// lines that read both ways (both numbers 12 or less).
	read int
	// monthFirstOnly and dayFirstOnly count, for the slash family only,
	// the lines that read in one order and not the other.
	monthFirstOnly, dayFirstOnly int
}

// dayFirst reports whether a slash file is read day first: enough lines
// read only that way, and more of them than read only month first. A tie
// reads month first.
func (t tally) dayFirst() bool {
	return t.dayFirstOnly >= minDayFirstLines && t.dayFirstOnly > t.monthFirstOnly
}

// lines is the number of sample lines the candidate reads once its order
// is locked: a slash line that reads only in the other order has no
// timestamp.
func (t tally) lines() int {
	if t.dayFirst() {
		return t.read + t.dayFirstOnly
	}
	return t.read + t.monthFirstOnly
}

// tier is the first key detection ranks qualifiers by. A higher tier
// wins whatever the line counts.
type tier int

const (
	// tierFewAnchored is an anchored qualifier below minAnchoredPercent:
	// it wins only when no windowed qualifier exists.
	tierFewAnchored tier = iota
	// tierWindowed is every windowed qualifier.
	tierWindowed
	// tierAnchored is an anchored qualifier at minAnchoredPercent or more.
	tierAnchored
)

// rank is how detection orders the qualifiers: by tier, then by the
// lines read, then by the family listed first. Comparing the three keys
// in turn is a total order, so the winner does not depend on the order
// the candidates are visited in.
type rank struct {
	tier   tier
	lines  int
	family int
}

// above reports whether r ranks above o.
func (r rank) above(o rank) bool {
	if r.tier != o.tier {
		return r.tier > o.tier
	}
	if r.lines != o.lines {
		return r.lines > o.lines
	}
	return r.family < o.family
}

// candidates holds every candidate, built once: the parsers are
// stateless, so all detections share them. The mtime given to them only
// picks the year of a year-less timestamp, which never decides whether a
// line matches.
var candidates = buildCandidates()

func buildCandidates() []candidate {
	var out []candidate
	for fi, fam := range families {
		for _, anchored := range []bool{true, false} {
			c := candidate{family: fi, anchored: anchored}
			orders := []*bool{nil}
			if fam.hasOrder {
				orders = []*bool{boolValue(false), boolValue(true)}
			}
			for _, order := range orders {
				p, err := NewParser(Format{Family: fam.name, Anchored: anchored, DayFirst: order}, 0)
				if err != nil {
					panic("timestamps: family table is inconsistent: " + err.Error())
				}
				c.parsers = append(c.parsers, p)
			}
			out = append(out, c)
		}
	}
	return out
}

func boolValue(b bool) *bool { return &b }

// Detect chooses the timestamp format of a file from sample, the first
// SampleBytes of the file's text (decompressed for a compressed file), or
// all of it when the text is shorter. It returns false when the file has
// no timestamps it recognizes.
//
// It reads at most SampleBytes of sample and at most SampleLines lines.
// A final line with no newline, such as one cut at SampleBytes, is read
// as it stands. Every caller passes the same bytes for the same file, so
// every caller gets the same Format.
//
// The rules:
//
//   - Each family is tried on each non-blank line twice: anchored (the
//     timestamp starts the line) and windowed (anywhere in the window).
//   - An anchored family qualifies when at least 3 lines start with its
//     timestamp, whatever their share. A windowed family qualifies when
//     it reads a timestamp from at least half of the non-blank lines:
//     JSON, CSV and XML carry dates too, so a date further into a line is
//     weak evidence.
//   - An anchored qualifier that reads at least 1% of the non-blank lines
//     beats every windowed one; below 1% it wins only when no windowed
//     family qualifies, so a few injected lines cannot take over a file
//     whose every line carries a timestamp further in. Among qualifiers
//     of one rank, the one that reads more lines wins; then the family
//     listed first.
//   - For the slash family, the file is day first when at least 3 lines
//     read only that way (their first number is above 12) and they
//     outnumber the lines that read only month first; it is month first
//     otherwise, ties included.
//   - HasZone is true when more than half of the lines the chosen format
//     reads carry a zone.
//
// The cost is bounded by the sample: at most SampleLines lines, each
// tried by a fixed number of parsers that look at WindowBytes of it.
func Detect(sample []byte) (Format, bool) {
	if len(sample) > SampleBytes {
		sample = sample[:SampleBytes]
	}
	tallies := make([]tally, len(candidates))
	nonBlank := 0
	forEachSampleLine(sample, func(line []byte) {
		nonBlank++
		for k := range candidates {
			countLine(&candidates[k], &tallies[k], line)
		}
	})
	best, bestRank := -1, rank{}
	for k := range candidates {
		c, t := &candidates[k], tallies[k]
		if !qualifies(c, t, nonBlank) {
			continue
		}
		if r := rankOf(c, t, nonBlank); best < 0 || r.above(bestRank) {
			best, bestRank = k, r
		}
	}
	if best < 0 {
		return Format{}, false
	}
	return lockFormat(&candidates[best], tallies[best], sample), true
}

// forEachSampleLine calls fn with each non-blank line among the first
// SampleLines lines of sample. A line of spaces, tabs and a carriage
// return is blank.
func forEachSampleLine(sample []byte, fn func(line []byte)) {
	for n := 0; n < SampleLines && len(sample) > 0; n++ {
		line := sample
		if i := bytes.IndexByte(sample, '\n'); i >= 0 {
			line, sample = sample[:i], sample[i+1:]
		} else {
			sample = nil
		}
		if len(bytes.Trim(line, " \t\r")) > 0 {
			fn(line)
		}
	}
}

// countLine adds one line to a candidate's tally. A slash candidate
// tries both orders on every line, so the lines that read only one way
// are counted for each order.
func countLine(c *candidate, t *tally, line []byte) {
	_, monthFirst := c.parsers[0].Own(line)
	if len(c.parsers) == 1 {
		if monthFirst {
			t.read++
		}
		return
	}
	_, dayFirst := c.parsers[1].Own(line)
	switch {
	case monthFirst && dayFirst:
		t.read++
	case monthFirst:
		t.monthFirstOnly++
	case dayFirst:
		t.dayFirstOnly++
	}
}

// qualifies applies the thresholds.
func qualifies(c *candidate, t tally, nonBlank int) bool {
	lines := t.lines()
	if lines == 0 {
		return false
	}
	if c.anchored {
		return lines >= minAnchoredLines
	}
	return 2*lines >= nonBlank
}

// rankOf places a qualifying candidate in the ranking.
func rankOf(c *candidate, t tally, nonBlank int) rank {
	r := rank{tier: tierWindowed, lines: t.lines(), family: c.family}
	if c.anchored {
		r.tier = tierFewAnchored
		if 100*r.lines >= minAnchoredPercent*nonBlank {
			r.tier = tierAnchored
		}
	}
	return r
}

// lockFormat builds the chosen Format: the slash order, then HasZone from
// the lines the locked format reads.
func lockFormat(c *candidate, t tally, sample []byte) Format {
	p := c.parsers[0]
	if len(c.parsers) > 1 && t.dayFirst() {
		p = c.parsers[1]
	}
	f := p.Format()
	read, zoned := 0, 0
	forEachSampleLine(sample, func(line []byte) {
		if s, ok := p.Own(line); ok {
			read++
			if s.Zoned {
				zoned++
			}
		}
	})
	f.HasZone = 2*zoned > read
	return f
}
