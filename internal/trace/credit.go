package trace

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/wlame/rx-go/internal/compression"
	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Which pattern matched a line
// ============================================================================
//
// ripgrep runs every -e pattern of a search as one alternation and
// reports the spans of that alternation, never which pattern matched.
// Its spans cannot tell either: on "NEEDLE here" with -e NEEDLE -e NEED
// the alternation reports the one span "NEEDLE", although NEED matches
// the line too, and with the patterns the other way round it reports
// "NEED".
//
// So the patterns of a line are decided after the scan, by ripgrep
// again: each pattern runs alone, with the request's matching flags,
// over the lines the scan reported, and a line is credited to exactly
// the patterns whose run reports it. That is the same test a trace of
// one pattern applies, so the lines and submatches an answer credits to
// a pattern are the ones a trace of that pattern alone gives them,
// whatever the other patterns are and in whichever order they come. Go's
// regexp plays no part: its \w, \b and \d are ASCII-only where ripgrep's
// are Unicode, it reads bytes that are not UTF-8 differently, and it
// cannot compile what -P accepts.
//
// The check reads only the matched lines, never the whole file again: a
// line ripgrep reported whole is fed from the text the answer already
// holds, and only a line the answer cut (longer than
// RX_MAX_LINE_TEXT_BYTES) is read again from the file, through its pin,
// because the part left out may hold any pattern's match.

// errLineMatchesNoPattern reports a line the scan reported that no
// pattern matches when run alone. A line rg reported always matches at
// least one of its patterns, so this means the line changed between the
// scan and the check (only a cut line is read again from the file).
var errLineMatchesNoPattern = errors.New("the line matches none of the patterns")

// creditBatchBytes is about how many bytes of line text one ripgrep run
// of the check reads. Many matched lines are checked in several batches,
// which run in parallel like the chunks of a scan.
const creditBatchBytes = 8 << 20

// patternCredit is one pattern a matched line is credited to, with that
// pattern's own submatches on the line, as a trace of the pattern alone
// reports them.
type patternCredit struct {
	patternID string
	// submatches are the pattern's own spans on the line, under the same
	// bounds as a scan's: at most RX_MAX_SUBMATCHES_PER_LINE, and only
	// those that start in the text a cut line keeps.
	submatches []rxtypes.Submatch
	// submatchesTruncated is true when submatches may leave some of the
	// pattern's spans out (see MatchRaw.SubmatchesTruncated).
	submatchesTruncated bool
}

// creditFile is one file's part of a creditRequest: the lines its scan
// reported, and the file as the trace pinned it, which is read only for
// a line the answer cut.
type creditFile struct {
	source sandbox.Pinned
	lines  []MatchRaw
}

// creditRequest is the input of creditPatterns: the lines the scans of a
// trace reported, file by file, and the search that reported them.
type creditRequest struct {
	files        []creditFile
	patternIDs   map[string]string
	patternOrder []string
	rgExtraArgs  []string
}

// fileCredits is what creditPatterns decided for one file: for each of
// its lines, in their order, the patterns credited, in pattern order. err
// is set instead when the file's lines could not be decided, and the
// trace then reports the file as skipped.
type fileCredits struct {
	lines [][]patternCredit
	err   error
}

// lineRef names one line of a creditRequest: req.files[file].lines[line].
type lineRef struct {
	file int
	line int
}

// creditPatterns decides, for every line of every file of req, the
// patterns that match it. The result has one entry per file of req, in
// the same order.
//
// With one pattern there is nothing to decide: every line is that
// pattern's, with the submatches the scan reported, and no ripgrep runs.
// With more, every pattern runs alone over the lines (see the comment at
// the top of this file). The lines of all files are checked together, so
// a search of many small files costs one ripgrep run per pattern and
// batch, not one per file.
//
// A file is lost alone (its fileCredits.err) when a run over its lines
// fails, when its cut lines cannot be read again, or with
// errLineMatchesNoPattern when one of its lines matches no pattern. The
// returned error ends the whole trace: a pattern ripgrep refuses
// (ErrInvalidPattern), or ctx's end. A line is never credited to a
// pattern its run did not report.
func creditPatterns(ctx context.Context, req creditRequest) ([]fileCredits, error) {
	if len(req.patternOrder) == 1 {
		return creditTheOnlyPattern(req), nil
	}
	patterns := len(req.patternOrder)

	// One cell per (file, line, pattern), written by the run of that
	// pattern over the batch holding that line. Every cell has exactly
	// one writer, so the runs share the slices without a lock; g.Wait
	// below is the happens-before edge for the reads after it.
	hits := make([][]patternHit, len(req.files))
	for f, file := range req.files {
		hits[f] = make([]patternHit, len(file.lines)*patterns)
	}
	batches := planCreditBatches(req.files)
	// runErrs holds the failure of each (batch, pattern) run, one writer
	// per cell like hits.
	runErrs := make([]error, len(batches)*patterns)

	// errgroup runs the (batch, pattern) checks at most workerLimit at a
	// time, as many as the chunks of a scan. A run's own failure is kept
	// in runErrs and loses only the files of its batch; only a failure
	// that ends the trace is returned to the group, which then cancels
	// gctx, kills the ripgrep of every run still going
	// (exec.CommandContext), and hands that failure to g.Wait.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workerLimit())
	for b, batch := range batches {
		for p, pid := range req.patternOrder {
			g.Go(func() error {
				err := checkBatchAgainstPattern(gctx, req, batch, pid, func(ref lineRef, hit patternHit) {
					hits[ref.file][ref.line*patterns+p] = hit
				})
				if err != nil && (errors.Is(err, ErrInvalidPattern) || gctx.Err() != nil) {
					return err
				}
				runErrs[b*patterns+p] = err
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	// A canceled request can stop runs before they report anything; the
	// cancel is the answer then, not lines without a pattern.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]fileCredits, len(req.files))
	for b, batch := range batches {
		for p := range req.patternOrder {
			if err := runErrs[b*patterns+p]; err != nil {
				for _, ref := range batch.lines {
					out[ref.file].err = err
				}
			}
		}
	}
	for f := range req.files {
		if out[f].err == nil {
			out[f].lines, out[f].err = creditsFromHits(req, f, hits[f])
		}
	}
	return out, nil
}

// creditTheOnlyPattern credits every line to the search's one pattern,
// with the submatches the scan reported.
func creditTheOnlyPattern(req creditRequest) []fileCredits {
	out := make([]fileCredits, len(req.files))
	for f, file := range req.files {
		out[f].lines = make([][]patternCredit, len(file.lines))
		for i, line := range file.lines {
			out[f].lines[i] = []patternCredit{{
				patternID:           req.patternOrder[0],
				submatches:          line.Submatches,
				submatchesTruncated: line.SubmatchesTruncated,
			}}
		}
	}
	return out
}

// patternHit is what one pattern's run reported for one line: whether
// it matched, and its submatches when it did.
type patternHit struct {
	matched             bool
	submatches          []rxtypes.Submatch
	submatchesTruncated bool
}

// creditsFromHits turns file f's hit cells into each of its lines'
// credits, in pattern order, and fails on a line no pattern matched.
func creditsFromHits(req creditRequest, f int, hits []patternHit) ([][]patternCredit, error) {
	patterns := len(req.patternOrder)
	file := req.files[f]
	out := make([][]patternCredit, len(file.lines))
	for i, line := range file.lines {
		for p, pid := range req.patternOrder {
			hit := hits[i*patterns+p]
			if hit.matched {
				out[i] = append(out[i], patternCredit{
					patternID:           pid,
					submatches:          hit.submatches,
					submatchesTruncated: hit.submatchesTruncated,
				})
			}
		}
		if len(out[i]) == 0 {
			return nil, fmt.Errorf("%w: line at offset %d of %s", errLineMatchesNoPattern, line.Offset, file.source.Path())
		}
	}
	return out, nil
}

// ============================================================================
// Batches
// ============================================================================

// creditBatch is a group of lines one ripgrep run reads, in the order it
// reads them.
type creditBatch struct {
	lines []lineRef
	// fromSource is true for a batch of one file's cut lines, whose whole
	// text is read again from that file; the others are fed from the text
	// the answer holds.
	fromSource bool
}

// planCreditBatches splits the lines of files into batches: the whole
// lines of every file, file after file, in batches of about
// creditBatchBytes, and then, for each file with cut lines, one batch of
// them by offset, so that file is read once from front to back.
func planCreditBatches(files []creditFile) []creditBatch {
	var batches []creditBatch
	var current creditBatch
	var currentBytes int
	var cutBatches []creditBatch
	for f, file := range files {
		cut := creditBatch{fromSource: true}
		for i, line := range file.lines {
			ref := lineRef{file: f, line: i}
			if line.LineTextTruncated {
				cut.lines = append(cut.lines, ref)
				continue
			}
			current.lines = append(current.lines, ref)
			currentBytes += len(line.LineText)
			if currentBytes >= creditBatchBytes {
				batches = append(batches, current)
				current, currentBytes = creditBatch{}, 0
			}
		}
		if len(cut.lines) > 0 {
			sort.Slice(cut.lines, func(a, b int) bool {
				return file.lines[cut.lines[a].line].Offset < file.lines[cut.lines[b].line].Offset
			})
			cutBatches = append(cutBatches, cut)
		}
	}
	if len(current.lines) > 0 {
		batches = append(batches, current)
	}
	return append(batches, cutBatches...)
}

// fedLength is how many bytes of ripgrep's input a line takes: its whole
// length for a line read again from the file, and its text plus the
// line break it is fed with otherwise.
func fedLength(line MatchRaw, fromSource bool) int64 {
	if fromSource {
		return line.End - line.Offset
	}
	return int64(len(line.LineText) + len(fedLineBreak(line)))
}

// fedLineBreak is the line break a whole line is fed with: the one it
// had in the file, so a pattern such as `\r$` sees a CRLF line as the
// scan did. A last line without a break gets "\n", which matches
// exactly what the line without it matches.
func fedLineBreak(line MatchRaw) string {
	if line.lineBreak == "" {
		return "\n"
	}
	return line.lineBreak
}

// ============================================================================
// One ripgrep run: one pattern over one batch
// ============================================================================

// checkBatchAgainstPattern runs pattern pid alone over the lines of
// batch and calls record for each line the run reports, with what it
// reports. The run's arguments are a scan's (newRgArgs and the request's
// matching flags) with the one pattern, so the run matches exactly what
// a trace of that pattern would.
func checkBatchAgainstPattern(
	ctx context.Context,
	req creditRequest,
	batch creditBatch,
	pid string,
	record func(ref lineRef, hit patternHit),
) error {
	args := rgPatternArgs(req.patternIDs, []string{pid}, req.rgExtraArgs)

	// starts[k] is where the batch's k-th line starts in ripgrep's
	// input. A match event names its line by that position, which is
	// checked rather than trusted: a line fed one byte off fails the run
	// instead of crediting the wrong line.
	starts := make([]int64, len(batch.lines))
	var pos int64
	for k, ref := range batch.lines {
		starts[k] = pos
		pos += fedLength(req.line(ref), batch.fromSource)
	}

	feed := func(w io.Writer) error {
		if batch.fromSource {
			return feedLinesFromSource(w, req, batch.lines)
		}
		return feedWholeLines(w, req, batch.lines)
	}
	onMatch := func(d *RgMatchData) error {
		k := sort.Search(len(starts), func(k int) bool { return starts[k] >= d.AbsoluteOffset })
		if k == len(starts) || starts[k] != d.AbsoluteOffset {
			return fmt.Errorf("ripgrep reported a line at input offset %d, where no line was fed", d.AbsoluteOffset)
		}
		record(batch.lines[k], patternHit{
			matched:             true,
			submatches:          submatchesOf(d),
			submatchesTruncated: d.SubmatchesTruncated,
		})
		return nil
	}
	out := runRipgrepOverInput(ctx, args, feed, onMatch)
	return classifyChunkOutcome(ctx, chunkOutcome{
		groupErr:   out.groupErr,
		waitErr:    out.waitErr,
		stderr:     out.stderr,
		patternIDs: map[string]string{pid: req.patternIDs[pid]},
		order:      []string{pid},
	})
}

// ripgrepRun is how a ripgrep run over a fed input ended: the first
// failure of its feeder or parser, rg's own exit, and what it wrote on
// stderr. classifyChunkOutcome turns it into one error.
type ripgrepRun struct {
	groupErr error
	waitErr  error
	stderr   string
}

// runRipgrepOverInput starts `rg args...`, writes its input with feed
// and passes each match event to onMatch, until rg has written its last
// event.
//
// Two goroutines run beside rg, under an errgroup: the feeder writes
// rg's stdin and closes it, which is rg's end of input; the parser reads
// rg's stdout through StreamEvents, whose bounds keep any one event
// small. INVARIANT: rg is never waited on while its stdout is unread.
// When either goroutine fails, stopOnFailure kills rg, so a parser that
// stopped reading cannot leave rg blocked on a full pipe, and a feeder
// blocked on rg's stdin gets a broken pipe and returns.
func runRipgrepOverInput(
	ctx context.Context,
	args []string,
	feed func(io.Writer) error,
	onMatch func(*RgMatchData) error,
) ripgrepRun {
	// rg runs under its own context so a failed goroutine can kill it
	// without canceling the caller's context; the deferred cancel
	// releases that context on every return.
	rgCtx, killRg := context.WithCancel(ctx)
	defer killRg()
	rgCmd := exec.CommandContext(rgCtx, "rg", args...)
	rgStdin, err := rgCmd.StdinPipe()
	if err != nil {
		return ripgrepRun{groupErr: fmt.Errorf("rg stdin pipe: %w", err)}
	}
	rgStdout, err := rgCmd.StdoutPipe()
	if err != nil {
		return ripgrepRun{groupErr: fmt.Errorf("rg stdout pipe: %w", err)}
	}
	var stderrBuf strings.Builder
	rgCmd.Stderr = &stderrBuf
	if err := rgCmd.Start(); err != nil {
		return ripgrepRun{groupErr: fmt.Errorf("rg start: %w", err)}
	}

	g, gctx := errgroup.WithContext(rgCtx)
	stopOnFailure := func(step func() error) func() error {
		return func() error {
			err := step()
			if err != nil {
				killRg()
			}
			return err
		}
	}
	g.Go(stopOnFailure(func() error {
		feedErr := feed(rgStdin)
		closeErr := rgStdin.Close()
		if feedErr == nil {
			feedErr = closeErr
		}
		// A run stopped on purpose, or an rg that exited before reading
		// all of its input, ends the feed with a broken pipe; rg's exit
		// status says why, so the pipe is not the error.
		if feedErr == nil || gctx.Err() != nil || isBrokenPipe(feedErr) {
			return nil
		}
		return fmt.Errorf("feed ripgrep: %w", feedErr)
	}))
	g.Go(stopOnFailure(func() error {
		return StreamEvents(gctx, rgStdout, func(ev *RgEvent, parseErr error) error {
			if parseErr != nil {
				return parseErr
			}
			if ev == nil || ev.Type != RgEventMatch || ev.Match == nil {
				return nil
			}
			return onMatch(ev.Match)
		})
	}))
	groupErr := g.Wait()
	waitErr := rgCmd.Wait()
	return ripgrepRun{groupErr: groupErr, waitErr: waitErr, stderr: strings.TrimSpace(stderrBuf.String())}
}

// line is the line ref names.
func (req creditRequest) line(ref lineRef) MatchRaw {
	return req.files[ref.file].lines[ref.line]
}

// feedWholeLines writes the lines refs name, in that order, each as its
// text and its line break. The text is the line's own bytes (see
// RgText), so a line that is not valid UTF-8 is fed as the scan read it.
func feedWholeLines(w io.Writer, req creditRequest, refs []lineRef) error {
	bw := bufio.NewWriterSize(w, 64*1024)
	for _, ref := range refs {
		line := req.line(ref)
		// bufio.Writer keeps its first write error and returns it from
		// every later call, so checking Flush at the end is enough.
		_, _ = bw.WriteString(line.LineText)
		_, _ = bw.WriteString(fedLineBreak(line))
	}
	return bw.Flush()
}

// feedLinesFromSource writes the whole bytes of the lines refs name, all
// of one file and in offset order, line break included, read again from
// the file's text: the file itself for a plain file, its decompressed
// stream otherwise, which is then decompressed once, from its start to
// the end of the last line. Nothing is held: each line is copied
// through in pieces, however long it is.
//
// The file is read through its pin, so a path that leads elsewhere by
// now is refused. A line that ends before its recorded end (the file
// shrank) is an error.
func feedLinesFromSource(w io.Writer, req creditRequest, refs []lineRef) error {
	source := req.files[refs[0].file].source
	f, err := source.Open()
	if err != nil {
		return fmt.Errorf("read the matched lines of %s again: %w", source.Path(), err)
	}
	format, err := compression.DetectFromOpenFile(source.Path(), f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("read the matched lines of %s again: %w", source.Path(), err)
	}
	if format == compression.FormatNone {
		defer func() { _ = f.Close() }()
		for _, ref := range refs {
			line := req.line(ref)
			length := line.End - line.Offset
			if _, copyErr := io.CopyN(w, io.NewSectionReader(f, line.Offset, length), length); copyErr != nil {
				return fmt.Errorf("read the line at offset %d of %s again: %w", line.Offset, source.Path(), copyErr)
			}
		}
		return nil
	}

	// NewReader takes f over: closing the decompressor closes f too.
	text, err := compression.NewReader(f, format)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("read the matched lines of %s again: %w", source.Path(), err)
	}
	defer func() { _ = text.Close() }()
	var pos int64
	for _, ref := range refs {
		line := req.line(ref)
		if _, err := io.CopyN(io.Discard, text, line.Offset-pos); err != nil {
			return fmt.Errorf("read the line at offset %d of %s again: %w", line.Offset, source.Path(), err)
		}
		if _, err := io.CopyN(w, text, line.End-line.Offset); err != nil {
			return fmt.Errorf("read the line at offset %d of %s again: %w", line.Offset, source.Path(), err)
		}
		pos = line.End
	}
	return nil
}
