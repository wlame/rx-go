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
	// lines is the number of non-blank sample lines it read a timestamp
	// from.
	lines int
	// dayFirstOnly is set when some line reads day first but not month
	// first (its first number is above 12).
	dayFirstOnly bool
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
//   - An anchored qualifier beats a windowed one; then the one that read
//     more lines wins; then the family listed first.
//   - For the slash family, the file is day first when some line reads
//     only that way (its first number is above 12), and month first
//     otherwise.
//   - HasZone is true when more than half of the lines the chosen format
//     reads carry a zone.
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
	best := -1
	for k := range candidates {
		if qualifies(&candidates[k], tallies[k], nonBlank) && (best < 0 || beats(k, best, tallies)) {
			best = k
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

// countLine adds one line to a candidate's tally.
func countLine(c *candidate, t *tally, line []byte) {
	_, first := c.parsers[0].Own(line)
	if first {
		t.lines++
		return
	}
	if len(c.parsers) > 1 {
		if _, second := c.parsers[1].Own(line); second {
			t.lines++
			t.dayFirstOnly = true
		}
	}
}

// qualifies applies the thresholds.
func qualifies(c *candidate, t tally, nonBlank int) bool {
	if t.lines == 0 {
		return false
	}
	if c.anchored {
		return t.lines >= minAnchoredLines
	}
	return 2*t.lines >= nonBlank
}

// beats reports whether candidate a ranks above candidate b.
func beats(a, b int, tallies []tally) bool {
	ca, cb := &candidates[a], &candidates[b]
	if ca.anchored != cb.anchored {
		return ca.anchored
	}
	if tallies[a].lines != tallies[b].lines {
		return tallies[a].lines > tallies[b].lines
	}
	return ca.family < cb.family
}

// lockFormat builds the chosen Format: the slash order, then HasZone from
// the lines the locked format reads.
func lockFormat(c *candidate, t tally, sample []byte) Format {
	p := c.parsers[0]
	if len(c.parsers) > 1 && t.dayFirstOnly {
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
