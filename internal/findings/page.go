package findings

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrNoScannedEnd reports an answer that holds findings but not the end
// of the text its scan read (Answer.TextLines 0). Its text is not read:
// without that end a read could hand over lines a file gained after the
// scan, which no masked range covers.
var ErrNoScannedEnd = errors.New("the findings carry no end of the text their scan read")

// WireFindings returns every stored finding of sel in answer as the
// wire carries it, without its text: in the one order of every carrier
// (first line, last line, signature id), each with fileID ("" for an
// answer about one file, which leaves the member out). With them come
// the density series of sel, by signature id.
//
// Times are read the way opts says (answerTimes): a scan keeps them in
// the file's own frame with the zone offset each stamped line writes,
// and an answer gives instants, a zone-less file's wall clocks read in
// opts.LogZone, or every line's written wall clock read in
// opts.FileZone when it is set: the line_timestamps value samples gives
// the finding's first line. A time outside the years 1 to 9999 becomes
// null; a series whose start cannot be read is left out. The file's
// format is read only when a zone asks for it.
func WireFindings(answer *Answer, file trace.SearchFile, sel []*signatures.Signature, fileID string, opts TraceOptions) ([]rxtypes.Finding, map[string]rxtypes.FindingSeries, error) {
	sel = uniqueSignatures(sel)
	stored := map[string]*sigscan.Series{}
	hasTime := false
	for _, sig := range sel {
		res := answer.BySignature[sig.ID]
		if res == nil {
			continue
		}
		hasTime = hasTime || slices.ContainsFunc(res.Findings, func(f sigscan.Finding) bool { return f.TimeMS != nil })
		if res.Series != nil {
			stored[sig.ID] = res.Series
		}
	}
	var times *samples.StoredTimes
	if hasTime || len(stored) > 0 {
		var err error
		if times, err = answerTimes(file, opts); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", file.Path(), err)
		}
	}

	found := []rxtypes.Finding{}
	for _, sig := range sel {
		res := answer.BySignature[sig.ID]
		if res == nil {
			continue
		}
		for _, f := range res.Findings {
			w := wireFinding(sig.ID, fileID, f)
			if times != nil && f.TimeMS != nil {
				w.TimeMS = instantOrNil(times, *f.TimeMS, f.TimeOffsetMinutes)
			}
			found = append(found, w)
		}
	}
	slices.SortStableFunc(found, func(a, b rxtypes.Finding) int { return rxtypes.CompareFindings(&a, &b) })
	return found, seriesIn(stored, times), nil
}

// answerTimes returns what turns a scan's stored times into the
// answer's instants, or nil when they are the answer's already: no file
// zone, and a log zone of UTC (a zoned file's instants and a zone-less
// file's wall clocks read in UTC are the stored values). Otherwise the
// file's format is read: from its line index when opts.IndexLoader
// gives one, else from the head of its text (at most a mebibyte,
// through its pin).
func answerTimes(file trace.SearchFile, opts TraceOptions) (*samples.StoredTimes, error) {
	if opts.FileZone == nil && (opts.LogZone == nil || opts.LogZone == time.UTC) {
		return nil, nil
	}
	return samples.StoredTimesOf(samples.StoredTimesRequest{
		Path: file.Path(), Source: file.Pinned(), Kind: file.Kind(), IndexLoader: opts.IndexLoader,
		FileZone: opts.FileZone, LogZone: opts.LogZone,
	})
}

// instantOrNil is the instant of a finding's stored time, whose stamped
// line writes the zone offset writtenOffset, or nil when it falls
// outside the years 1 to 9999.
func instantOrNil(times *samples.StoredTimes, stored int64, writtenOffset int) *int64 {
	instant, ok := times.Instant(stored, writtenOffset)
	if !ok {
		return nil
	}
	return &instant
}

// seriesIn turns the stored density series into the wire's, their
// starts read as times reads the file's times (nil: as stored). A start
// belongs to no line, so it is read only in the file's own frame
// (StoredTimes.Exact): under a file zone, a file whose timestamps carry
// zones has its series left out, with an index and without one alike.
// So is a series whose start falls outside the years 1 to 9999.
func seriesIn(stored map[string]*sigscan.Series, times *samples.StoredTimes) map[string]rxtypes.FindingSeries {
	out := make(map[string]rxtypes.FindingSeries, len(stored))
	for id, s := range stored {
		start := s.StartMS
		if times != nil {
			if !times.Exact() {
				continue
			}
			instant, ok := times.Instant(start, 0)
			if !ok {
				continue
			}
			start = instant
		}
		counts := slices.Clone(s.Counts)
		if counts == nil {
			counts = []int64{}
		}
		out[id] = rxtypes.FindingSeries{BucketMS: s.BucketMS, StartMS: start, Counts: counts}
	}
	return out
}

// FillText reads the text of page, findings of answer's file (from
// WireFindings, in any order), and sets it on them as a trace answer's
// text is set: each finding's lines and the opts.Before lines before
// it (opts.After after it), every line masked by the redacting set of
// the mode (redactorsOf: every loaded redacting signature of cat with
// opts.Scan.MaskSecrets, none without) and every line in answer's
// masked ranges shown as its length, valid UTF-8, cut to
// opts.LineTextBytes; nothing past the end of the text the scan read
// (answer.TextLines and TextBytes).
//
// The text of all of them together is held to opts.TextBudget, taken
// in line order: the finding at which it runs out and every finding
// after it in line order lose their text and are marked
// lines_truncated. page keeps its own order. The file is read once,
// through its pin, from the first finding's window to the last.
//
// It returns ErrNoScannedEnd (wrapped, naming the file) when page is
// not empty and answer has no scanned end, and the read's error, which
// names the file; canceling ctx stops the read.
func FillText(ctx context.Context, file trace.SearchFile, page []rxtypes.Finding, answer *Answer, cat *signatures.Catalogue, opts TraceOptions) error {
	if len(page) == 0 {
		return nil
	}
	masker, err := sigscan.NewLineMasker(redactorsOf(cat, opts.Scan), opts.Scan.Scan.LineBytes)
	if err != nil {
		return err
	}
	// The pass reads spans in order of their first line, so the page is
	// read in the one order and put back in its own after.
	order := make([]int, len(page))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Or(rxtypes.CompareFindings(&page[a], &page[b]), cmp.Compare(a, b))
	})
	sorted := make([]rxtypes.Finding, len(page))
	for k, i := range order {
		sorted[k] = page[i]
	}
	text := &traceText{opts: opts, masker: masker, remaining: max(opts.TextBudget, 0), readSpans: samples.ReadLineSpans}
	ff := &fileFindings{
		file:     TraceFile{File: file},
		findings: sorted,
		text:     scannedText{ranges: answer.MaskedRanges, lines: answer.TextLines, bytes: answer.TextBytes},
	}
	if err := text.fill(ctx, ff); err != nil {
		return err
	}
	for k, i := range order {
		page[i] = sorted[k]
	}
	return nil
}
