package findings

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TraceFile is one file of a trace answer: its id in the answer's files
// map and the file, pinned and classified as the search classified it.
type TraceFile struct {
	ID   string
	File trace.SearchFile
	// Spooled marks piped stdin copied to a temporary file: it is never
	// read from nor written to the cache.
	Spooled bool
}

// TraceOptions are the settings of Trace. The commands fill them from
// the environment; this package reads none.
type TraceOptions struct {
	// Scan holds the scan and cache settings (Scan's options). Its
	// Spooled is set per file from TraceFile.Spooled.
	Scan ScanOptions
	// Before and After are how many lines before a finding's first line
	// and after its last line come with it (rx trace -B, -A, --context).
	Before, After int
	// LineTextBytes is the most bytes of each line a finding's text
	// holds (RX_MAX_LINE_TEXT_BYTES); 0 or less holds none.
	LineTextBytes int
	// TextBudget is the most bytes of text the answer's findings hold
	// together (RX_FINDINGS_TEXT_MAX_BYTES); 0 or less holds none.
	TextBudget int64
	// LogZone is the zone a file whose timestamps carry none was written
	// in (RX_LOG_TZ): such a file's times are given as instants read in
	// it, as samples gives them. Nil or UTC leaves them as scanned.
	LogZone *time.Location
	// FileZone, when set, reads every timestamp of a file as the wall
	// clock its line writes, in this zone, as samples reads a file under
	// a request's file_tz; it takes the place of LogZone. A file whose
	// timestamps carry zones needs the offset each line writes, which
	// only its line index gives (IndexLoader, zone_offsets): without it
	// its findings have no time.
	FileZone *time.Location
	// IndexLoader, when set, gives a file's line index, which makes a
	// compressed file's text faster to reach (samples.StoredIndex, or
	// samples.NoIndex for --no-index). The text does not depend on it.
	IndexLoader samples.IndexLoader
}

// TraceAnswer is what Trace adds to a trace answer: the members of
// rxtypes.TraceResponse a trace with signatures fills, and the files it
// had to pass over.
type TraceAnswer struct {
	Signatures     map[string]rxtypes.SignatureRef
	Findings       []rxtypes.Finding
	FindingSummary map[string]rxtypes.FileFindingSummary
	// Skipped are the files whose scan or text could not be read, each
	// with the reason, as a search words it (trace.SkipReason).
	Skipped []rxtypes.SkippedFile
}

// Trace answers the findings of sel in every file of files, for a trace
// answer: each file through Scan (from the cache where it can, with its
// own cache entries), then every finding's text read from its file and
// masked (traceText).
//
// The findings are in the one order of every carrier: file id, first
// line, last line, signature id. Each carries its file's id. A file
// whose scan or text fails is in Skipped with the reason and holds no
// finding and no summary; the others are answered. Canceling ctx ends
// the call with ctx.Err().
func Trace(ctx context.Context, files []TraceFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts TraceOptions) (*TraceAnswer, error) {
	return newTracer().trace(ctx, files, cat, sel, opts)
}

// tracer runs Trace; its fields are seams a test replaces.
type tracer struct {
	// scan is Scan (the scheduler of the process).
	scan func(ctx context.Context, file trace.SearchFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts ScanOptions) (*Answer, error)
	// readSpans reads the lines of findings (samples.ReadLineSpans).
	readSpans func(ctx context.Context, req samples.SpansRequest, visit func(int, samples.SpanLine) samples.SpanAction) error
}

// newTracer returns the tracer of production.
func newTracer() *tracer {
	return &tracer{scan: defaultScheduler.scan, readSpans: samples.ReadLineSpans}
}

// fileFindings is one scanned file's part of the answer while it is put
// together.
type fileFindings struct {
	file     TraceFile
	findings []rxtypes.Finding
	// text is the file's masked ranges (the bodies of redacting blocks
	// and runs, whose lines no carrier shows) and the end of the text the
	// scan read, past which no line is shown.
	text    scannedText
	summary rxtypes.FileFindingSummary
}

func (tr *tracer) trace(ctx context.Context, files []TraceFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts TraceOptions) (*TraceAnswer, error) {
	sel = uniqueSignatures(sel)
	ans := &TraceAnswer{
		Signatures:     signatureRefs(sel),
		Findings:       []rxtypes.Finding{},
		FindingSummary: map[string]rxtypes.FileFindingSummary{},
	}
	var scanned []*fileFindings
	for _, f := range files {
		ff, err := tr.scanFile(ctx, f, cat, sel, opts)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			ans.Skipped = append(ans.Skipped, rxtypes.SkippedFile{Path: f.File.Path(), Reason: trace.SkipReason(err)})
			continue
		}
		scanned = append(scanned, ff)
	}

	// Every finding's text is masked by every loaded redacting
	// signature, as every excerpt is (Scan masks excerpts by the same
	// set, cat.Redactors()).
	masker, err := sigscan.NewLineMasker(cat.Redactors(), opts.Scan.Scan.LineBytes)
	if err != nil {
		return nil, err
	}
	text := &traceText{opts: opts, masker: masker, remaining: max(opts.TextBudget, 0), readSpans: tr.readSpans}
	for _, ff := range scanned {
		if err := text.fill(ctx, ff); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			ans.Skipped = append(ans.Skipped, rxtypes.SkippedFile{Path: ff.file.File.Path(), Reason: trace.SkipReason(err)})
			continue
		}
		ans.Findings = append(ans.Findings, ff.findings...)
		ans.FindingSummary[ff.file.ID] = ff.summary
	}
	return ans, nil
}

// signatureRefs describes each signature of sel, by id.
func signatureRefs(sel []*signatures.Signature) map[string]rxtypes.SignatureRef {
	refs := make(map[string]rxtypes.SignatureRef, len(sel))
	for _, sig := range sel {
		refs[sig.ID] = rxtypes.SignatureRef{Name: sig.Name, Group: sig.Group, Level: string(sig.Level), Kind: string(sig.Kind)}
	}
	return refs
}

// scanFile scans one file (Scan) and turns its answer into wire
// findings in the one order, with times read in the log zone
// (WireFindings).
func (tr *tracer) scanFile(ctx context.Context, f TraceFile, cat *signatures.Catalogue, sel []*signatures.Signature, opts TraceOptions) (*fileFindings, error) {
	scanOpts := opts.Scan
	scanOpts.Spooled = scanOpts.Spooled || f.Spooled
	answer, err := tr.scan(ctx, f.File, cat, sel, scanOpts)
	if err != nil {
		return nil, err
	}
	text := scannedText{ranges: answer.MaskedRanges, lines: answer.TextLines, bytes: answer.TextBytes}
	ff := &fileFindings{file: f, text: text, summary: rxtypes.FileFindingSummary{
		State: string(answer.State), Signatures: make(map[string]rxtypes.FindingCount, len(sel)),
	}}
	for _, sig := range sel {
		res := answer.BySignature[sig.ID]
		if res == nil {
			res = &sigscan.SignatureResult{}
		}
		ff.summary.Signatures[sig.ID] = rxtypes.FindingCount{Count: res.Count, Stored: len(res.Findings)}
	}
	if ff.findings, _, err = WireFindings(answer, f.File, sel, f.ID, opts); err != nil {
		return nil, err
	}
	return ff, nil
}

// wireFinding is a scan's finding as the wire carries it, without its
// text yet.
func wireFinding(sigID, fileID string, f sigscan.Finding) rxtypes.Finding {
	return rxtypes.Finding{
		Signature: sigID, File: fileID,
		StartLine: f.StartLine, EndLine: f.EndLine, StartOffset: f.StartOffset, EndOffset: f.EndOffset,
		TimeMS: f.TimeMS, Excerpt: f.Excerpt, ValueStart: f.ValueStart, ValueEnd: f.ValueEnd, Masked: f.Masked,
	}
}

// maskSlack is how many bytes past the line text limit a line is
// read, so a value that starts before the cut and ends after it is
// masked as a whole before the cut (sigscan.LineMasker.MaskLine). It is
// the scan's line view: a value a scan can match fits in it.
func maskSlack(opts TraceOptions) int {
	if lb := opts.Scan.Scan.LineBytes; lb > 0 {
		return lb
	}
	return sigscan.DefaultLineBytes
}

// maxContextLines bounds the lines asked for before and after a
// finding, far above any window a reader asks for, so a line number
// plus the window never overflows. The text budget bounds what is kept.
const maxContextLines = 1 << 30

// byteOrderMark is the UTF-8 byte-order mark. A scan's line view of a
// text's first line leaves it out (offsets still count it), so the
// text of line 1 is masked without it: an anchor at the line's start
// matches there as it did in the scan.
var byteOrderMark = []byte{0xEF, 0xBB, 0xBF}

// traceText fills the findings' text file by file, in the one order,
// within one budget for the whole answer.
type traceText struct {
	opts      TraceOptions
	masker    *sigscan.LineMasker
	readSpans func(ctx context.Context, req samples.SpansRequest, visit func(int, samples.SpanLine) samples.SpanAction) error
	// remaining is what is left of the budget; spent says it ran out
	// at an earlier finding: every later finding loses its text.
	remaining int64
	spent     bool
}

// findingText is one finding's text while it is read.
type findingText struct {
	before, lines, after []string
	// cost is the bytes of text taken so far; masked says a mask or a
	// replacement is in it; complete says its last line came.
	cost     int64
	masked   bool
	complete bool
}

// fill reads the text of one file's findings and sets it on them: the
// lines of each finding and the lines asked for around it, each masked
// and cut (maskedLine), until the budget runs out.
//
// The budget is taken in the one order: finding k keeps its text when
// the text of findings 1..k together, from the first file on, fits;
// each line counts its bytes and one more for its line break.
// The finding at which it runs out and every later one lose their text
// and are marked lines_truncated. The spans of one file are read in
// one pass (samples.ReadLineSpans), and spans that overlap are read
// together, so a finding's text is complete only at its last line;
// which findings fit is decided as the text comes: once the text taken
// by findings 1..k is more than the budget, finding k cannot fit
// whatever comes later (text only grows), so it is dropped at once and
// its text let go. The text held is thus at most the budget plus the
// line being read, and the pass stops as soon as no finding that still
// fits wants a line.
func (t *traceText) fill(ctx context.Context, ff *fileFindings) error {
	if t.spent || len(ff.findings) == 0 {
		t.truncateFrom(ff.findings, 0)
		return nil
	}
	// SECURITY: a read without the scanned end would go to the end of
	// the file as it is now (samples.SpansRequest.TextLines 0), past the
	// text the masked ranges describe.
	if ff.text.lines <= 0 {
		return fmt.Errorf("%s: %w", ff.file.File.Path(), ErrNoScannedEnd)
	}
	n := len(ff.findings)
	texts := make([]findingText, n)
	spans := make([]samples.LineSpan, n)
	for i, f := range ff.findings {
		spans[i] = samples.LineSpan{FirstLine: f.StartLine, LastLine: f.EndLine, StartOffset: f.StartOffset}
	}
	// fits is the number of findings, from the first, whose text still
	// fits; taken is the text of those findings, and complete how many
	// of them have their whole text.
	fits, taken, complete := n, int64(0), 0
	// The current line masked, kept while the spans that share the line
	// take it.
	var cached struct {
		number int64
		text   string
		masked bool
	}
	visit := func(i int, line samples.SpanLine) samples.SpanAction {
		if i >= fits {
			return samples.SpanDone
		}
		if cached.number != line.Number {
			cached.number = line.Number
			cached.text, cached.masked = t.maskedLine(line, ff.text.ranges)
		}
		f, ft := &ff.findings[i], &texts[i]
		text, masked := cached.text, cached.masked
		switch {
		case line.Number < f.StartLine:
			ft.before = append(ft.before, text)
		case line.Number > f.EndLine:
			ft.after = append(ft.after, text)
		default:
			ft.lines = append(ft.lines, text)
		}
		// A line costs its text and one byte for its line break, so even
		// empty lines spend the budget and the lines handed over stay
		// bounded by it.
		cost := int64(len(text)) + 1
		ft.cost += cost
		ft.masked = ft.masked || masked
		if !ft.complete && line.Number >= f.EndLine+int64(min(max(t.opts.After, 0), maxContextLines)) {
			ft.complete = true
			complete++
		}
		taken += cost
		// Text only grows, so the first finding whose text no longer
		// fits is found by walking back from the end of those that did.
		for taken > t.remaining {
			fits--
			taken -= texts[fits].cost
			if texts[fits].complete {
				complete--
			}
			texts[fits] = findingText{}
		}
		switch {
		case i >= fits:
			return samples.SpanDone
		case fits < n && complete == fits:
			// Every finding that fits has its text, and none after them
			// can: nothing more to read.
			return samples.StopReading
		}
		return samples.SpanContinue
	}
	req := samples.SpansRequest{
		Source: ff.file.File.Pinned(), Kind: ff.file.File.Kind(), Spans: spans,
		Before: min(t.opts.Before, maxContextLines), After: min(t.opts.After, maxContextLines),
		KeepBytes:   max(t.opts.LineTextBytes, 0) + maskSlack(t.opts),
		IndexLoader: t.opts.IndexLoader,
		// SECURITY: nothing past the text the scan read: a log that grew
		// since holds lines in no masked range (the rest of a key block
		// open at the end, say).
		TextLines: ff.text.lines, TextBytes: ff.text.bytes,
	}
	if err := t.readSpans(ctx, req, visit); err != nil {
		return err
	}
	for i := range fits {
		f, ft := &ff.findings[i], &texts[i]
		f.Before, f.Lines, f.After = ft.before, ft.lines, ft.after
		f.Masked = f.Masked || ft.masked
	}
	t.remaining -= taken
	if fits < n {
		t.spent = true
		t.truncateFrom(ff.findings, fits)
	}
	return nil
}

// truncateFrom marks findings[from:] as past the budget: no text, and
// lines_truncated.
func (t *traceText) truncateFrom(findings []rxtypes.Finding, from int) {
	for i := from; i < len(findings); i++ {
		findings[i].Before, findings[i].Lines, findings[i].After = nil, nil, nil
		findings[i].LinesTruncated = true
	}
}

// maskedLine is a line of the file as a finding's text shows it: masked
// by every redacting signature (sigscan.LineMasker), valid UTF-8, cut
// to the line text limit on a character boundary; line 1 without its
// byte-order mark. masked says a mask is in it.
//
// SECURITY: a line in one of ranges, the body of a redacting block or
// run (a PEM key's base64 lines), shows only its length: in the lines
// of any finding and in the lines before and after one alike.
func (t *traceText) maskedLine(line samples.SpanLine, ranges [][2]int64) (string, bool) {
	if sigscan.InMaskedRanges(ranges, line.Number) {
		return sigscan.MaskedLineText(line.Length), true
	}
	text := line.Text
	if line.Number == 1 {
		text = bytes.TrimPrefix(text, byteOrderMark)
	}
	return t.masker.MaskLine(text, max(t.opts.LineTextBytes, 0))
}
