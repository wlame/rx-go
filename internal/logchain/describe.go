package logchain

import (
	"context"
	"errors"
	"fmt"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrPartChanged reports that a part of a chain changed between the
// listing that found it and the read that describes it: its name now
// leads to another file (a rotation renamed it, or replaced it), or to
// none. The error also wraps the cause (paths.ErrFileChanged or
// fs.ErrNotExist). The routes answer it with 409; the chain is listed
// and described again to give the current description.
var ErrPartChanged = errors.New("a part of the chain changed while it was read")

// Options says how Describe reads a chain's parts.
type Options struct {
	// FileZone, when it names a zone, reads the timestamps of every part
	// as the wall clock its lines write, in that zone, as
	// samples.Request.FileZone reads one file (`--file-tz`, `file_tz`).
	// The zero value reads each part as its timestamps say.
	FileZone config.Zone
	// Scan reads what an index would give when a part has no current
	// one: a frozen part's line count and times, and the active part's
	// line count, from an index built in memory and not stored. It is
	// what `rx logs show` does, which never waits for background work.
	// Without it, as over HTTP, such a frozen part is not read, and the
	// chain is pending until its index is built.
	Scan bool
}

// Description is one chain described: the wire answer, and what the
// readers of the chain's lines (samples, search) compute with, so they
// need no second pass over the parts.
type Description struct {
	// Candidate is the chain as Resolve found it, its parts in the
	// provisional order.
	Candidate Candidate
	// Response is the answer of GET /v1/logs/chain and of
	// `rx logs show --json`. Its Parts are in the chain's order: by time
	// when the chain is ready (or invalid), provisional before.
	Response *rxtypes.ChainResponse
	// Order maps the chain's order to the candidate's: Response.Parts[i]
	// describes Candidate.Parts[Order[i]].
	Order []int
	// Starts is, for each part of Response.Parts in that order, the
	// global line number of its first line: 1 plus the line counts of
	// the parts before it. An empty part's start is the start of the
	// part after it. Nil until the chain is ready.
	Starts []int64
	// MaxSoFar is, for each part of Response.Parts in that order, the
	// highest max_ms of that part and every part before it, or
	// math.MinInt64 when none of them has one (empty parts at the
	// start). A part whose max_ms is an upper bound (max_is_bound) adds
	// its bound. The active part adds its max_ms only when it is known,
	// so a time after every frozen part's MaxSoFar lies in the active
	// part. Nil until the chain is ready.
	MaxSoFar []int64
}

// wireTimestampLayout is how modified_at is written: RFC 3339 in UTC
// with six fractional digits, as GET /v1/tree writes it.
const wireTimestampLayout = "2006-01-02T15:04:05.000000Z"

// Describe reads what a chain needs to be read as one text and returns
// its description: the parts' line counts and times, their order by
// time, the checks, the state, the fingerprint, each part's global
// start, the time gaps and the missing parts.
//
// What it reads (every read through the part's pin, Part.File):
//
//   - each frozen part: its current line index (index.LoadForPinned),
//     which gives its line count, its first, last and highest
//     timestamp, and its format. A part without one is opened to learn
//     that it can be read, and with opts.Scan indexed in memory; without
//     Scan it stays unread and the chain is pending. An empty part
//     (0 bytes) is never opened: it adds no line.
//   - the active part: the head of its text and a bounded read back
//     from its end (samples.TimeRange), or nothing when a current index
//     gives them; its line count from that index, or with opts.Scan
//     from an index built in memory.
//
// The frozen parts' data of a chain whose frozen parts all have a
// current index (or are empty) is kept in memory (see
// descriptionCache), keyed by the handle, a digest of every part's stat
// and the zones the times are read in; a second describe then reads no
// index of a frozen part, only stats each one's index file. The active
// part is read on every call: it grows without changing the key.
//
// The error is ErrPartChanged (a part's name led to another file, or to
// none, by the time it was read: describe the chain again from a new
// listing) or the context's error. Every other failure to read a part is
// a reason of an invalid chain (unreadable), not an error.
func Describe(ctx context.Context, c Candidate, opts Options) (*Description, error) {
	fingerprint := Fingerprint(c)
	d := &Description{Candidate: c, Response: newResponse(c, fingerprint)}
	if c.TooManyParts {
		// SECURITY: a chain past MaxParts is refused before any of its
		// parts is read, so the work of describing one is bounded by
		// MaxParts index loads whatever a directory holds. Its answer
		// lists no part (the reason gives the count), so its size does
		// not follow the number of files that share the chain's name.
		d.Order = []int{}
		d.Response.State = rxtypes.ChainStateInvalid
		d.Response.Reasons = append(d.Response.Reasons, rxtypes.ChainReason{
			Code: rxtypes.ChainReasonTooManyParts, Parts: []string{},
			Message: fmt.Sprintf("the names of the chain's files give %d parts; at most %d are read as one text",
				c.NamedParts, MaxParts),
		})
		return d, nil
	}
	facts, err := readFacts(ctx, c, opts)
	if err != nil {
		return nil, err
	}
	d.assemble(facts, config.ChainOverlapMs())
	return d, nil
}

// assemble turns the parts' facts into the description: the reasons of
// each part, the order, the checks that compare parts, the state, and
// for a ready chain the global starts, the line counts, the times and
// the gaps. toleranceMs is RX_CHAIN_OVERLAP_SECONDS, in milliseconds.
func (d *Description) assemble(facts []partFacts, toleranceMs int64) {
	c := d.Candidate
	resp := d.Response
	for i, part := range c.Parts {
		resp.Reasons = append(resp.Reasons, partReasons(part, facts[i])...)
	}
	known := everyPartKnown(c, facts)
	invalid := len(resp.Reasons) > 0
	// Once every part is known the parts are ordered by time. An invalid
	// chain is ordered by time as far as the parts read allow, so its
	// reasons read in the order they name. A pending chain keeps the
	// provisional order: a part not read yet could belong anywhere.
	switch {
	case known || invalid:
		d.Order = timeOrder(c, facts)
	default:
		d.Order = provisionalOrder(len(c.Parts))
	}
	if known {
		resp.Reasons = append(resp.Reasons, orderReasons(c, facts, d.Order, toleranceMs)...)
	}
	resp.Parts = partEntries(c, d.Order, facts)
	switch {
	case len(resp.Reasons) > 0:
		resp.State = rxtypes.ChainStateInvalid
	case !known:
		resp.State = rxtypes.ChainStatePending
	default:
		resp.State = rxtypes.ChainStateReady
		d.fillReady(facts)
	}
}

// everyPartKnown reports whether every part's facts that a ready chain
// needs were read: each frozen part's line count and times, and the
// active part's first timestamp (or that it has none). An empty part
// needs nothing.
func everyPartKnown(c Candidate, facts []partFacts) bool {
	for i, part := range c.Parts {
		f := facts[i]
		if isEmpty(part, f) {
			continue
		}
		if !f.timesKnown || (!part.IsActive && f.lines == nil) {
			return false
		}
	}
	return true
}

// isEmpty reports whether a part holds no line: a file of 0 bytes, or
// one whose line count is 0 (a compressed empty text).
func isEmpty(part Part, f partFacts) bool {
	return part.Info.Size() == 0 || (f.lines != nil && *f.lines == 0)
}

// maxDescribeAttempts is how many times DescribeHandle lists and
// describes a chain whose parts change while it is read. A rotation
// renames a few files in a moment, so the next listing nearly always
// sees the chain at rest; the bound stops the work when files keep
// moving.
const maxDescribeAttempts = 3

// DescribeHandle finds the chain a handle names (Resolve) and describes
// it (Describe): what GET /v1/logs/chain and the `rx logs` commands that
// read one chain do.
//
// When a part changes between the listing and the read (ErrPartChanged:
// a rotation ran in between), the chain is listed and described again,
// at most maxDescribeAttempts times in all, and changed is true: the
// answer is the chain as it is now, and a caller holding an earlier
// description learns that the files moved (GET /v1/logs/chain answers
// 409). When every attempt meets a change the error is ErrPartChanged.
// The other errors are Resolve's, and the context's.
func DescribeHandle(ctx context.Context, handle string, opts Options) (d *Description, changed bool, err error) {
	for attempt := 0; attempt < maxDescribeAttempts; attempt++ {
		var c Candidate
		if c, err = Resolve(handle); err != nil {
			return nil, changed, err
		}
		d, err = Describe(ctx, c, opts)
		if !errors.Is(err, ErrPartChanged) {
			return d, changed, err
		}
		changed = true
	}
	return nil, changed, err
}
