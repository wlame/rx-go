package logchain

import (
	"context"
	"errors"
	"fmt"

	"github.com/wlame/rx-go/internal/samples"
)

// partEndsWithLineBreak reads the last byte of a part's text, decoding
// at most its read limit of the text (samples.EndsWithLineBreak): the one
// read the read back across a part edge makes of an earlier part, when
// its facts cannot answer. Tests replace it to count the reads.
var partEndsWithLineBreak = samples.EndsWithLineBreak

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
// line with a timestamp of its own, with that timestamp as an instant.
// Those say whether that line starts within the read back's distance
// without a byte of the part being read.
//
// When the index does not give that timestamp as an instant (under
// file_tz, in a part whose written zone offset changes more often than
// the index records), the part is not read for it either: the stamp says
// the instant is not known (InstantOK false), and the lines that carry
// it get none, null in line_timestamps, where the parts read as one file
// give them the line's value.
//
// No index field records whether a part's text ends with a line break,
// which the chain adds after a part that ends without one. Its last byte
// is read, within what is left of the request's byte limit
// (endsWithLineBreak), only when a line's distance decides on it
// (Settle), or for a part with lines and no timestamp at all, which the
// read back passes over (never in a ready chain, whose every part with
// lines has one).
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
		// SECURITY: the instant comes from the index or is not known; the
		// part is never read for it. Reading its last timestamped line
		// meant decompressing a stream-compressed part up to that line,
		// outside every limit of the request.
		if f.lastMs != nil {
			stamp.Instant, stamp.InstantOK = *f.lastMs, true
		}
		e.from = j
		return stamp, nil
	}
	return samples.EarlierStamp{}, nil
}

// Settle implements samples.Earlier: stamp with the line break the
// chain adds after a part that ends without one counted, which moves the
// line one byte further back. It reads the last byte of the part's text
// within what is left of the request's byte limit; past it the part is
// taken to end without one (endsWithLineBreak).
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

// endsWithLineBreak reads whether part j's text ends with a line break:
// its last byte, through the part's pin.
//
// SECURITY: the read decodes at most what is left of the request's
// MaxBytes (sampler.edgeReadLimit), at least one byte, and what it may
// have decoded is counted against that (chargeEdgeRead). So the edge
// reads of one request together decode at most MaxBytes of text, plus
// one byte for each read once that is used up. A plain part costs one
// byte. A stream-compressed part whose text is longer than what is
// left, or a seekable part whose last frame holds more, is not read
// (samples.ErrReadLimit), and the part is taken to end without a line
// break. That puts the text before part j+1 one byte further back, so a
// line at exactly the read back's distance carries no timestamp (null,
// not known), never one it would not carry. Without MaxBytes (`rx logs
// samples`) the byte is always read.
func (e *earlierParts) endsWithLineBreak(ctx context.Context, j int) (bool, error) {
	req, err := e.s.partRequest(j)
	if err != nil {
		return false, err
	}
	textLen := *e.s.d.facts[e.s.d.Order[j]].textLen
	ends, err := partEndsWithLineBreak(ctx, req, textLen, e.s.edgeReadLimit())
	switch {
	case errors.Is(err, samples.ErrReadLimit):
		return false, nil
	case err != nil:
		return false, err
	}
	e.s.chargeEdgeRead(j, textLen)
	return ends, nil
}
