package logchain

import (
	"context"
	"fmt"
	"strconv"

	"github.com/wlame/rx-go/internal/samples"
)

// The reads the read back across a part edge makes of an earlier part,
// when its facts cannot answer. Tests replace them to count the reads;
// each is the samples function otherwise.
var (
	// partEndsWithLineBreak reads the last byte of a part's text.
	partEndsWithLineBreak = samples.EndsWithLineBreak
	// resolveEarlierLine reads one line of a part, for its timestamp.
	resolveEarlierLine = samples.Resolve
)

// earlierOf is the text a ready chain reads before Response.Parts[k],
// for the read back of line_timestamps in a piece of part k (see
// earlierParts); nil when no part with lines comes before it.
func (s *sampler) earlierOf(k int) samples.Earlier {
	for j := k - 1; j >= 0; j-- {
		if s.knownCount(j) != 0 {
			return &earlierParts{s: s, k: k}
		}
	}
	return nil
}

// earlierParts is the text a ready chain reads before one of its parts:
// the parts before it, in the chain's order. It implements
// samples.Earlier, so the read back of line_timestamps crosses the part
// edge: a line without a timestamp of its own at the start of part k (a
// traceback continued from the part before) gets the value it gets
// when the parts are read as one file.
//
// It answers from what describing the chain read of each part, which
// comes from the part's line index: the length of its text and its last
// line with a timestamp of its own, with that timestamp. Those say
// whether that line starts within the read back's distance without a
// byte of the part being read. A part is read only when they cannot say:
//
//   - the timestamp of its last stamped line is not known from the index
//     (under file_tz, in a part whose zone offsets the index does not
//     record): that line is read;
//   - a line's distance from it decides on whether the part ends with a
//     line break (Settle): the part's last byte is read;
//   - a part has lines and no timestamp at all (never in a ready chain,
//     whose every part with lines has one): its last byte is read, and
//     the read back passes over it.
//
// INVARIANT: the read back of part k crosses only into parts before k
// in a ready chain, every one of which has facts from a line index
// (stored, or built in memory by `rx logs show`'s scan); the distance
// it covers is at most the read back's (RX_TIMESTAMP_LOOKBACK_KB), so
// it passes at most that many bytes of parts, however many there are.
type earlierParts struct {
	s *sampler
	k int
	// from is the part (an index of Response.Parts) whose line LastStamp
	// found, which Settle reads the end of.
	from int
}

// LastStamp implements samples.Earlier: the nearest line before part k
// with a timestamp of its own, when it starts at most within bytes
// before part k's first byte. Its Start assumes the part that holds it
// ends with a line break; OneEarlier says it may not.
func (e *earlierParts) LastStamp(ctx context.Context, within int64) (samples.EarlierStamp, error) {
	d := e.s.d
	// passed is the length of the text of the parts the look back has
	// passed over, from their first byte to part k's.
	var passed int64
	for j := e.k - 1; j >= 0 && passed <= within; j-- {
		i := d.Order[j]
		part, f := d.Candidate.Parts[i], d.facts[i]
		if isEmpty(part, f) {
			continue
		}
		if f.textLen == nil {
			return samples.EarlierStamp{}, fmt.Errorf("the line index of %s does not give the length of its text", part.Name)
		}
		if f.last == nil {
			n, err := e.chainLength(ctx, j)
			if err != nil {
				return samples.EarlierStamp{}, err
			}
			passed += n
			continue
		}
		distance := passed + *f.textLen - f.last.Offset
		if distance > within {
			return samples.EarlierStamp{}, nil
		}
		stamp := samples.EarlierStamp{Found: true, Start: -distance, OneEarlier: true}
		if f.lastMs != nil {
			stamp.Instant, stamp.InstantOK = *f.lastMs, true
		} else {
			var err error
			if stamp.Instant, stamp.InstantOK, err = e.stampOfLine(ctx, j, f.last.Line); err != nil {
				return samples.EarlierStamp{}, err
			}
		}
		e.from = j
		return stamp, nil
	}
	return samples.EarlierStamp{}, nil
}

// Settle implements samples.Earlier: stamp with the line break the
// chain adds after a part that ends without one counted, which moves the
// line one byte further back. It reads the last byte of the part's text.
func (e *earlierParts) Settle(ctx context.Context, stamp samples.EarlierStamp) (samples.EarlierStamp, error) {
	ends, err := e.endsWithLineBreak(ctx, e.from)
	if err != nil {
		return stamp, err
	}
	if !ends {
		stamp.Start--
	}
	stamp.OneEarlier = false
	return stamp, nil
}

// chainLength is the length of part j's text in the chain read as one:
// its own, and one more for the line break added after a part that
// ends without one.
func (e *earlierParts) chainLength(ctx context.Context, j int) (int64, error) {
	f := e.s.d.facts[e.s.d.Order[j]]
	ends, err := e.endsWithLineBreak(ctx, j)
	if err != nil {
		return 0, err
	}
	if ends {
		return *f.textLen, nil
	}
	return *f.textLen + 1, nil
}

// endsWithLineBreak reads whether part j's text ends with a line break.
func (e *earlierParts) endsWithLineBreak(ctx context.Context, j int) (bool, error) {
	req, err := e.s.partRequest(j)
	if err != nil {
		return false, err
	}
	return partEndsWithLineBreak(ctx, req, *e.s.d.facts[e.s.d.Order[j]].textLen)
}

// stampOfLine reads line `line` of part j, a line with a timestamp of
// its own, and returns that timestamp as a UTC instant, read the way a
// samples answer reads the part (its zone included); ok is false when it
// has none in the years 1 to 9999.
func (e *earlierParts) stampOfLine(ctx context.Context, j int, line int64) (instant int64, ok bool, err error) {
	req, err := e.s.partRequest(j)
	if err != nil {
		return 0, false, err
	}
	req.Lines = []samples.OffsetOrRange{{Start: line}}
	resp, err := resolveEarlierLine(ctx, req)
	if err != nil {
		return 0, false, err
	}
	stamps := resp.LineTimestamps[strconv.FormatInt(line, 10)]
	if len(stamps) == 0 || stamps[0] == nil {
		return 0, false, nil
	}
	return *stamps[0], true, nil
}
