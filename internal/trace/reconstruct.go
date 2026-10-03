package trace

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// reconstructBufferBytes is the read buffer of a reconstruction pass. A
// pass stops at the line after the last match it needs, so it reads at
// most this much past that line.
const reconstructBufferBytes = 256 * 1024

// openForReconstruct opens the source a reconstruction pass reads,
// refusing a path that no longer leads to the file the trace checked.
// Tests replace it to count the bytes read.
var openForReconstruct = func(src sandbox.Pinned) (io.ReadSeekCloser, error) {
	return src.Open()
}

// ReconstructRequest describes one cache hit to rebuild.
type ReconstructRequest struct {
	// Source is the cached file, pinned when the trace checked it.
	Source        sandbox.Pinned
	Cached        []rxtypes.TraceCacheMatch
	Patterns      []string
	FileID        string
	RgExtraArgs   []string
	ContextBefore int
	ContextAfter  int
	UseIndex      bool
	// MaxMatches, when above 0, is the trace's max_results. The engine
	// keeps the first max_results matches by offset, so the pass stops
	// once it holds that many and has read the lines their context can
	// reach (see contextReachPastLastMatch). 0 rebuilds every match.
	MaxMatches int
}

// ReconstructFromCache rebuilds full matches and their context lines
// from the minimal records a trace cache holds.
//
// The cache stores only (pattern_index, offset, line_number) per match.
// The line text, the submatches and the surrounding lines have to come
// from the source again, and the byte offset is what addresses them.
// The offset is also the only field that cannot be stale: the cache is
// keyed on the source's size and mtime, so an offset still points at
// the same bytes, while a stored line number is only as good as the
// version that wrote it. Line numbers are therefore counted during this
// pass rather than trusted from the file.
//
// One sequential pass serves every match in the cache, so a hit on a
// file with thousands of matches costs about one read of the file
// instead of one read per match. With an index present the pass starts
// at the checkpoint before the first match rather than at byte 0.
//
// A compressed source is read through its decompressor from the start:
// the offsets in the cache address the decompressed stream, which is
// also what the scan that filled the cache measured.
//
// A context line carries the byte offset of its first byte, as a scan
// reports it; rx-python's reconstruction leaves it -1.
//
// Parity: rx-python/src/rx/trace_cache.py::reconstruct_match_data.
func ReconstructFromCache(req ReconstructRequest) ([]rxtypes.Match, []rxtypes.ContextLine, error) {
	matches, ctxLines, _, err := reconstructLines(req)
	return matches, ctxLines, err
}

// reconstructLines is ReconstructFromCache that also reports where each
// line it returns ends, keyed by where it starts: the engine links the
// lines of a context window through those ends.
func reconstructLines(req ReconstructRequest) ([]rxtypes.Match, []rxtypes.ContextLine, map[int64]int64, error) {
	if len(req.Cached) == 0 {
		return nil, nil, nil, nil
	}
	cached := append([]rxtypes.TraceCacheMatch(nil), req.Cached...)
	sort.SliceStable(cached, func(i, j int) bool { return cached[i].Offset < cached[j].Offset })

	src, err := openReconstructSource(req, cached[0].Offset)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = src.close() }()

	flags := matchFlagsFrom(req.RgExtraArgs)
	limits := currentEventLimits()
	var lineBuf []byte // reused by every line read
	matches := make([]rxtypes.Match, 0, len(cached))
	var ctxLines []rxtypes.ContextLine
	ends := map[int64]int64{} // where each line returned ends, by where it starts
	emitted := map[int]bool{} // context line numbers already emitted

	before := newLineRing(req.ContextBefore)
	afterWanted := 0
	next := 0 // index into cached

	// lastLineToRead is 0 until the pass holds MaxMatches matches, and
	// then the last line it still has to read.
	lastLineToRead := 0

	r := bufio.NewReaderSize(src.reader, reconstructBufferBytes)
	pos, line := src.startOffset, src.startLine
	for next < len(cached) || afterWanted > 0 {
		if lastLineToRead > 0 && line > lastLineToRead {
			break
		}
		// A line is read in bounded memory and cut as a scan cuts it, so
		// a line of any length costs at most the line text limit.
		var size int64
		var readErr error
		lineBuf, size, readErr = readBoundedLine(r, lineBuf, limits.lineTextBytes+lineBreakRoom)
		if size == 0 && readErr != nil {
			break
		}
		text, cut := boundedLineText(lineBuf, size, limits.lineTextBytes)
		end := pos + size

		if afterWanted > 0 && !emitted[line] {
			emitted[line] = true
			ends[pos] = end
			ctxLines = append(ctxLines, rxtypes.ContextLine{
				RelativeLineNumber: line,
				AbsoluteLineNumber: line,
				LineText:           text,
				AbsoluteOffset:     pos,
				LineTextTruncated:  cut,
			})
			afterWanted--
		}

		// Every cached record whose offset falls inside this line.
		first := next
		for next < len(cached) && cached[next].Offset < end {
			next++
		}
		if next > first {
			for _, prev := range before.lines() {
				if emitted[prev.number] {
					continue
				}
				emitted[prev.number] = true
				ends[prev.offset] = prev.end
				ctxLines = append(ctxLines, rxtypes.ContextLine{
					RelativeLineNumber: prev.number,
					AbsoluteLineNumber: prev.number,
					LineText:           prev.text,
					AbsoluteOffset:     prev.offset,
					LineTextTruncated:  prev.cut,
				})
			}
			emitted[line] = true
			ends[pos] = end
			read := readLine{text: text, cut: cut, number: line}
			for _, cm := range cached[first:next] {
				m, mErr := matchFromCached(cm, read, req, flags, limits.submatches)
				if mErr != nil {
					continue
				}
				matches = append(matches, m)
			}
			if req.ContextAfter > afterWanted {
				afterWanted = req.ContextAfter
			}
			if lastLineToRead == 0 && req.MaxMatches > 0 && len(matches) >= req.MaxMatches {
				lastLineToRead = line + contextReachPastLastMatch(req.ContextAfter)
			}
		}

		before.push(ringLine{number: line, offset: pos, end: end, text: text, cut: cut})
		pos, line = end, line+1
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return matches, ctxLines, ends, fmt.Errorf("reconstruct %s: %w", req.Source.Path(), readErr)
			}
			break
		}
	}
	return matches, ctxLines, ends, nil
}

// contextReachPastLastMatch is how many lines past the last match a
// trace keeps a pass must still read so the window of that match is
// exactly what a pass over every cached match gives it.
//
// buildContextDict gives a match the lines from before lines ahead of
// it to after lines past it. Every line past it in that window is
// emitted as context by the match's own trailing count, or is a match
// itself, and reading up to it finds both; nothing further on reaches
// the window. A match past the cap that this reads is dropped by the
// engine's cut and still serves as a line of the window, as in the full
// pass.
func contextReachPastLastMatch(after int) int {
	return after
}

// readLine is a line a reconstruction pass read: its text as an answer
// reports it (cut at the line text limit when longer), whether it was
// cut, and its number.
type readLine struct {
	text   string
	cut    bool
	number int
}

// matchFromCached turns one cached record plus the line it points into
// into a full match. Submatches are found in the text the line keeps and
// capped at maxSubmatches; like a scan's, the list is marked as possibly
// incomplete when it hit the cap or the line was cut.
func matchFromCached(
	cm rxtypes.TraceCacheMatch,
	line readLine,
	req ReconstructRequest,
	flags matchFlags,
	maxSubmatches int,
) (rxtypes.Match, error) {
	if cm.PatternIndex < 0 || cm.PatternIndex >= len(req.Patterns) {
		return rxtypes.Match{}, fmt.Errorf(
			"reconstruct: pattern_index %d out of range (have %d patterns)",
			cm.PatternIndex, len(req.Patterns))
	}
	lineText := line.text
	subs, capped := submatchesFromPattern(req.Patterns[cm.PatternIndex], line.text, flags, maxSubmatches)
	return rxtypes.Match{
		Pattern:             fmt.Sprintf("p%d", cm.PatternIndex+1),
		File:                req.FileID,
		Offset:              cm.Offset,
		RelativeLineNumber:  ptrInt(line.number),
		AbsoluteLineNumber:  line.number,
		LineText:            &lineText,
		Submatches:          subs,
		LineTextTruncated:   line.cut,
		SubmatchesTruncated: capped || line.cut,
	}, nil
}

// readBoundedLine reads the next line of r, its break included, keeping
// at most keep of its bytes in buf, whose memory it reuses. It returns
// the kept bytes, the line's whole length and the read error, as
// ReadBytes does: a last line without a break comes with io.EOF.
//
// bufio.Reader.ReadSlice hands out the line a buffer at a time
// (bufio.ErrBufferFull until the break), so nothing past keep is held
// however long the line is.
func readBoundedLine(r *bufio.Reader, buf []byte, keep int) (kept []byte, size int64, err error) {
	kept = buf[:0]
	for {
		piece, err := r.ReadSlice('\n')
		if room := keep - len(kept); room > 0 {
			kept = append(kept, piece[:min(room, len(piece))]...)
		}
		size += int64(len(piece))
		if !errors.Is(err, bufio.ErrBufferFull) {
			return kept, size, err
		}
	}
}

// boundedLineText is a line's text as an answer reports it, from the
// first bytes readBoundedLine kept (at least limit+lineBreakRoom of
// them, or the whole line) and its whole length: without its break,
// and cut at limit, at the start of a character, when longer. It cuts
// exactly where the scan's parser cuts the same line.
func boundedLineText(kept []byte, size int64, limit int) (text string, cut bool) {
	if size <= int64(len(kept)) {
		if whole := trimTrailingNewline(string(kept)); len(whole) <= limit {
			return whole, false
		}
	}
	return string(kept[:characterCut(kept, limit)]), true
}

// ============================================================================
// Reading the source
// ============================================================================

// reconstructSource is the stream a reconstruction pass reads, plus the
// file position and line number that stream starts at.
type reconstructSource struct {
	reader      io.Reader
	close       func() error
	startOffset int64
	startLine   int
}

// openReconstructSource opens the source for a reconstruction pass and
// positions it so the first cached match is reached with room to spare
// for its leading context.
func openReconstructSource(req ReconstructRequest, firstOffset int64) (*reconstructSource, error) {
	f, err := openForReconstruct(req.Source)
	if err != nil {
		return nil, fmt.Errorf("reconstruct: open %s: %w", req.Source.Path(), err)
	}

	// A compressed source is read through its decompressor from the
	// start: cached offsets address the decompressed stream, and there
	// is no cheap way into the middle of it.
	if format, _ := compression.DetectFromPath(req.Source.Path()); format != compression.FormatNone {
		dec, dErr := compression.NewReader(f, format)
		if dErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("reconstruct: decompress %s: %w", req.Source.Path(), dErr)
		}
		return &reconstructSource{
			reader:    dec,
			close:     func() error { _ = dec.Close(); return f.Close() },
			startLine: 1,
		}, nil
	}

	src := &reconstructSource{reader: f, close: f.Close, startLine: 1}
	if !req.UseIndex {
		return src, nil
	}
	// Start one checkpoint earlier than the one holding the first
	// match, so the lines before it are available as leading context.
	idx, idxErr := index.LoadForPinned(req.Source)
	if idxErr != nil || idx == nil || len(idx.LineIndex) == 0 {
		return src, nil
	}
	pick := index.CheckpointIndexForOffset(idx, firstOffset)
	if pick > 0 {
		pick--
	}
	if pick < 0 {
		return src, nil
	}
	entry := idx.LineIndex[pick]
	if _, sErr := f.Seek(entry.ByteOffset, io.SeekStart); sErr != nil {
		return src, nil //nolint:nilerr // a failed seek only costs a longer scan
	}
	src.startOffset, src.startLine = entry.ByteOffset, int(entry.LineNumber)
	return src, nil
}

// ============================================================================
// Leading-context ring
// ============================================================================

// ringLine is one remembered line: its number, the byte offset of its
// first byte, where it ends (its line break included), and its text.
type ringLine struct {
	number int
	offset int64
	end    int64
	text   string
	cut    bool // the text holds only the line's first bytes
}

// lineRing remembers the last n lines read, which is what a match needs
// for its leading context. A ring keeps that bounded no matter how big
// the file is.
type lineRing struct {
	buf  []ringLine
	next int
	size int
}

func newLineRing(n int) *lineRing {
	if n < 0 {
		n = 0
	}
	return &lineRing{buf: make([]ringLine, n)}
}

func (r *lineRing) push(line ringLine) {
	if len(r.buf) == 0 {
		return
	}
	r.buf[r.next] = line
	r.next = (r.next + 1) % len(r.buf)
	if r.size < len(r.buf) {
		r.size++
	}
}

// lines returns the remembered lines in file order, oldest first.
func (r *lineRing) lines() []ringLine {
	if r.size == 0 {
		return nil
	}
	out := make([]ringLine, 0, r.size)
	start := (r.next - r.size + len(r.buf)) % len(r.buf)
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

// ============================================================================
// Submatches
// ============================================================================

// submatchesFromPattern re-runs the pattern against the line and returns
// the byte positions of its first max hits, sorted by start, and
// whether there were more.
//
// The pattern is compiled by compileLikeRipgrep, the same way
// identification compiles it, so a rebuilt submatch covers the text rg
// matched under the request's -i, -w, -x and -F. A pattern Go cannot
// compile (PCRE2 under -P) yields no submatches; the match itself is
// still reported, because the cache recorded which pattern it was.
func submatchesFromPattern(pattern, line string, flags matchFlags, maxHits int) ([]rxtypes.Submatch, bool) {
	re, err := compileLikeRipgrep(pattern, flags)
	if err != nil {
		return nil, false
	}
	// One hit past the cap says whether the cap left any out.
	locs := re.FindAllStringIndex(line, maxHits+1)
	capped := len(locs) > maxHits
	if capped {
		locs = locs[:maxHits]
	}
	subs := make([]rxtypes.Submatch, 0, len(locs))
	for _, l := range locs {
		subs = append(subs, rxtypes.Submatch{
			Text:  line[l[0]:l[1]],
			Start: l[0],
			End:   l[1],
		})
	}
	return subs, capped
}

// ptrInt helper — returns &v.
func ptrInt(v int) *int { return &v }

// largeFileThresholdBytes returns the size in bytes at which a file
// triggers unified-index + trace cache. Pulls from config so tests can
// override via RX_LARGE_FILE_MB.
func largeFileThresholdBytes() int64 {
	return int64(config.LargeFileMB()) * 1024 * 1024
}
