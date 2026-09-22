package samples

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// IndexLoader is the loose-coupling seam between this package and the
// unified index cache. The CLI and HTTP callers both construct a loader
// that hits internal/index.LoadForSource; tests substitute an in-memory
// stub. Returning (nil, nil) means "no index available — use a linear
// scan fallback". Returning an error aborts the resolve.
type IndexLoader func(path string) (*rxtypes.UnifiedFileIndex, error)

// NoIndex is an IndexLoader that always reports "no cache". Useful in
// tests and when callers explicitly want to skip index-aware seeks.
func NoIndex(string) (*rxtypes.UnifiedFileIndex, error) { return nil, nil }

// Request is the input to Resolve. Exactly one of Offsets or Lines
// must be non-empty. Context / BeforeContext / AfterContext are
// caller-resolved (Resolve does not apply defaults).
type Request struct {
	Path          string
	Offsets       []OffsetOrRange
	Lines         []OffsetOrRange
	BeforeContext int
	AfterContext  int
	// IndexLoader is invoked once per Resolve call if Lines mode and
	// large-file shortcuts are needed. Set to NoIndex for the linear
	// scan fallback.
	IndexLoader IndexLoader
}

// Mode reports which dispatch path Resolve will take. Returns OffsetsMode
// when Offsets is non-empty, LinesMode otherwise.
func (r Request) Mode() Mode {
	if len(r.Offsets) > 0 {
		return OffsetsMode
	}
	return LinesMode
}

// Mode is the enum of request dispatch paths.
type Mode int

// Mode values.
const (
	LinesMode Mode = iota
	OffsetsMode
)

// Resolve executes the request and returns a populated SamplesResponse.
// Path validation / compression detection / sandboxing are the caller's
// responsibility — this function assumes the path exists and is a text
// file.
//
// Offsets mode (byte offset → line):
//
//	Single:   key = string(start_byte), sample = ±context lines around
//	          the line containing start_byte, offsets[key] = line number.
//	Range:    key = "start-end", sample = every line overlapping the
//	          byte range [start, end], offsets[key] = start line number.
//
// Lines mode (line number → byte offset):
//
//	Single:   key = string(line), sample = ±context lines around line,
//	          lines[key] = byte offset of LINE (not context start).
//	Range:    key = "start-end", sample = lines start..end, lines[key]
//	          = -1 (sentinel; Python parity).
//
// Negative single values are resolved against file size (byte mode) or
// total line count (lines mode). Ranges must have both ends >= 0.
func Resolve(req Request) (*rxtypes.SamplesResponse, error) {
	resp := &rxtypes.SamplesResponse{
		Path:          req.Path,
		Offsets:       map[string]int64{},
		Lines:         map[string]int64{},
		BeforeContext: req.BeforeContext,
		AfterContext:  req.AfterContext,
		Samples:       map[string][]string{},
	}
	// A compressed file has its own path: there is nothing to seek to,
	// so the stream is read once and the wanted lines are kept. Doing
	// this here rather than in a caller is what keeps `rx samples` and
	// GET /v1/samples answering the same way — the CLI used to send a
	// compressed file down the plain-text path and print its bytes.
	if format, _ := compression.DetectFromPath(req.Path); format != compression.FormatNone {
		resp.IsCompressed = true
		name := string(format)
		resp.CompressionFormat = &name
		// A seekable .zst with a frame index can decompress just the
		// frames holding the wanted lines. Without an index it streams
		// like any other archive: the answer is the same, only slower,
		// which is what an index is for.
		if len(req.Offsets) == 0 && seekable.IsSeekable(req.Path) {
			err := resolveSeekableLines(req, resp)
			if err == nil {
				return resp, nil
			}
			if !errors.Is(err, errNoFrameIndex) {
				return nil, err
			}
		}
		if err := resolveCompressedLines(req, format, resp); err != nil {
			return nil, err
		}
		return resp, nil
	}

	switch req.Mode() {
	case OffsetsMode:
		if err := resolveOffsets(req, resp); err != nil {
			return nil, err
		}
	case LinesMode:
		if err := resolveLines(req, resp); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// ============================================================================
// Byte-offset mode
// ============================================================================

// window is one byte-offset request being collected as the file goes by.
type window struct {
	key     string
	start   int64
	end     int64 // byte range end, or -1 for a single offset
	line    int64
	started bool
	done    bool
	after   int
	collect []string
}

// resolveOffsets answers every byte offset in the request from one
// sequential pass over the file.
//
// A caller with a batch of match offsets — which is how the viewer asks
// after a capped search — used to pay a full scan per offset, once to
// find the line and again to read the window around it. The pass here
// resolves the line numbers and collects the windows together, keeping
// the last few lines in a ring so a window that reaches backwards is
// already in hand.
func resolveOffsets(req Request, resp *rxtypes.SamplesResponse) error {
	fi, err := os.Stat(req.Path)
	if err != nil {
		return err
	}
	fileSize := fi.Size()

	windows := make([]*window, 0, len(req.Offsets))
	for _, v := range req.Offsets {
		start := v.Start
		if start < 0 {
			// Python parity: a negative offset counts back from the end
			// and the response reports the resolved positive value.
			start = fileSize + start
			if start < 0 {
				start = 0
			}
		}
		w := &window{key: strconv.FormatInt(start, 10), start: start, end: -1}
		if v.IsRange() {
			w.key, w.end = v.Key(), *v.End
		}
		windows = append(windows, w)
		resp.Samples[w.key] = []string{}
	}
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].start < windows[j].start })

	var idx *rxtypes.UnifiedFileIndex
	if req.IndexLoader != nil {
		idx, _ = req.IndexLoader(req.Path)
	}
	// The pass may start at a checkpoint before the first offset, as
	// long as it leaves room for the leading context.
	startOffset, startLine := int64(0), int64(1)
	if idx != nil && len(windows) > 0 {
		startOffset, startLine = checkpointBefore(idx, windows[0].start, req.BeforeContext)
	}

	f, err := openFileForSamples(req.Path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if startOffset > 0 {
		if _, seekErr := f.Seek(startOffset, io.SeekStart); seekErr != nil {
			return seekErr
		}
	}

	before := newLineRing(req.BeforeContext)
	r := bufio.NewReaderSize(f, readBufferFor(windows[len(windows)-1].start-startOffset))
	pos, lineNum, next := startOffset, startLine, 0
	for {
		raw, readErr := r.ReadString('\n')
		if len(raw) == 0 && readErr != nil {
			break
		}
		text := stripNewline(raw)
		end := pos + int64(len(raw))

		// Start every window whose offset falls on this line.
		for next < len(windows) && windows[next].start < end {
			w := windows[next]
			w.started, w.line = true, lineNum
			resp.Offsets[w.key] = lineNum
			if w.end < 0 {
				w.collect = append(w.collect, before.lines()...)
				w.after = req.AfterContext
			}
			next++
		}

		for _, w := range windows {
			if !w.started || w.done {
				continue
			}
			if lineNum < w.line {
				continue
			}
			w.collect = append(w.collect, text)
			switch {
			case w.end >= 0:
				// A byte range ends on the line holding its end offset.
				if w.end < end {
					w.done = true
				}
			case lineNum > w.line:
				w.after--
				if w.after <= 0 {
					w.done = true
				}
			case req.AfterContext == 0:
				w.done = true
			}
		}

		before.push(text)
		pos, lineNum = end, lineNum+1
		if readErr != nil {
			break
		}
		if next >= len(windows) && allWindowsDone(windows) {
			break
		}
	}

	for _, w := range windows {
		if !w.started {
			// The offset is past the end of the file, so there is no
			// line at it. Reporting the file's last line was a number
			// counted from the wrong place; -1 is what the line
			// numbering contract spells "asked but unknown", and it is
			// what a line number past the last line already answers.
			resp.Offsets[w.key] = -1
			resp.Samples[w.key] = nil
			continue
		}
		resp.Samples[w.key] = w.collect
	}
	return nil
}

// allWindowsDone reports whether every started window has all its lines.
func allWindowsDone(windows []*window) bool {
	for _, w := range windows {
		if w.started && !w.done {
			return false
		}
	}
	return true
}

// readBufferFor sizes the read buffer to the span a pass will cover.
//
// A pass over a large file wants a big buffer to keep the syscall count
// down; a request that stops a few kilobytes in wants a small one,
// because whatever the buffer holds past the answer is read for
// nothing. The bounded-read tests measure exactly that overshoot.
func readBufferFor(span int64) int {
	const smallBuffer, largeBuffer = 4 * 1024, 64 * 1024
	if span > 512*1024 {
		return largeBuffer
	}
	return smallBuffer
}

// checkpointBefore returns the index checkpoint to start a pass from so
// that `context` lines are available before `offset`, and the line
// number that checkpoint names.
func checkpointBefore(
	idx *rxtypes.UnifiedFileIndex,
	offset int64,
	context int,
) (byteOffset, line int64) {
	pick := index.CheckpointIndexForOffset(idx, offset)
	// Step back one checkpoint when the window reaches behind the
	// offset, so the leading context is inside the pass.
	if pick > 0 && context > 0 {
		pick--
	}
	if pick < 0 {
		return 0, 1
	}
	return idx.LineIndex[pick].ByteOffset, idx.LineIndex[pick].LineNumber
}

// lineRing remembers the last n lines read, which is what a window that
// reaches backwards needs.
type lineRing struct {
	buf  []string
	next int
	size int
}

func newLineRing(n int) *lineRing {
	if n < 0 {
		n = 0
	}
	return &lineRing{buf: make([]string, n)}
}

func (r *lineRing) push(text string) {
	if len(r.buf) == 0 {
		return
	}
	r.buf[r.next] = text
	r.next = (r.next + 1) % len(r.buf)
	if r.size < len(r.buf) {
		r.size++
	}
}

// lines returns the remembered lines in file order, oldest first.
func (r *lineRing) lines() []string {
	if r.size == 0 {
		return nil
	}
	out := make([]string, 0, r.size)
	start := (r.next - r.size + len(r.buf)) % len(r.buf)
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

// ============================================================================
// Lines mode
// ============================================================================

// resolveLines dispatches line-number single values and ranges.
// When req.IndexLoader returns a valid index and the requested line is
// beyond the first checkpoint, we seek directly to the nearest checkpoint
// at-or-before the target — avoids scanning the file from byte 0.
func resolveLines(req Request, resp *rxtypes.SamplesResponse) error {
	// Lazy-load index; only needed if at least one query would benefit
	// (single lines with context, or any range).
	var idx *rxtypes.UnifiedFileIndex
	idxLoaded := false
	loadIdx := func() (*rxtypes.UnifiedFileIndex, error) {
		if idxLoaded {
			return idx, nil
		}
		idxLoaded = true
		if req.IndexLoader == nil {
			return nil, nil
		}
		got, err := req.IndexLoader(req.Path)
		if err != nil {
			return nil, err
		}
		idx = got
		return idx, nil
	}

	// Resolve negative singles against total line count (index hit or
	// linear count fallback).
	needTotal := false
	for _, v := range req.Lines {
		if !v.IsRange() && v.Start < 0 {
			needTotal = true
			break
		}
	}
	var totalLines int64
	if needTotal {
		ix, err := loadIdx()
		if err != nil {
			return err
		}
		if ix != nil && ix.LineCount != nil && *ix.LineCount > 0 {
			totalLines = *ix.LineCount
		} else {
			n, err := countLines(req.Path)
			if err != nil {
				return err
			}
			totalLines = n
		}
	}

	// Resolve each request.
	for _, v := range req.Lines {
		if v.IsRange() {
			// Range.
			ix, err := loadIdx()
			if err != nil {
				return err
			}
			lines, err := readLineRangeWithIndex(
				req.Path, v.Start, *v.End, ix,
			)
			if err != nil {
				return err
			}
			key := v.Key()
			resp.Samples[key] = lines
			resp.Lines[key] = -1 // Python parity: ranges skip the expensive offset compute
			continue
		}

		// Single line. Resolve negative, then compute context window
		// AND the requested-line's byte offset.
		target := v.Start
		if target < 0 {
			target = totalLines + target + 1
			if target < 1 {
				target = 1
			}
		}
		// Line 0 is not a line: they are numbered from 1. Asking for it
		// used to return the window that clamping produced, which
		// answered a question nobody asked.
		if target < 1 {
			key := strconv.FormatInt(target, 10)
			resp.Lines[key] = -1
			resp.Samples[key] = nil
			continue
		}
		startLine := target - int64(req.BeforeContext)
		if startLine < 1 {
			startLine = 1
		}
		endLine := target + int64(req.AfterContext)
		ix, err := loadIdx()
		if err != nil {
			return err
		}
		lines, targetOffset, err := readLinesWithTarget(
			req.Path, startLine, endLine, target, ix,
		)
		if err != nil {
			return err
		}
		// Python parity: negative inputs are converted to their
		// resolved positive value for the key (samples.py line 600:
		// `line_to_offset[str(start)] = byte_offset_val` where `start`
		// has already been reassigned to the positive value).
		key := strconv.FormatInt(target, 10)
		// A line the file does not have — past the last one, or line 0
		// of an empty file — is asked-but-unknown, which the line
		// numbering contract spells -1 with a null sample. Reporting
		// offset 0 for line 1 of an empty file claimed a line that is
		// not there.
		if len(lines) == 0 {
			resp.Lines[key] = -1
			resp.Samples[key] = nil
			continue
		}
		resp.Samples[key] = lines
		// resp.Lines[key] holds the offset of line `target` — the line
		// the caller asked about, not the context window's first line.
		resp.Lines[key] = targetOffset
	}
	return nil
}

// ============================================================================
// Low-level file helpers (seeking, line counting)
// ============================================================================

// readSeekCloser is the narrow interface the line-reading helpers need.
// Extracted from *os.File so tests can inject counting wrappers that
// observe byte traffic without changing production signatures. Every
// method is a direct subset of *os.File's surface — no adapter needed
// at the call site.
type readSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

// openFileForSamples is the indirection point tests hook to count
// bytes read from the source file. Production default is os.Open wrapped
// so the returned value satisfies readSeekCloser. Tests reassign this
// var to a function that wraps *os.File in counting.File, observe
// counter.Load() after the operation, and restore the original in
// t.Cleanup. See resolver_budget_test.go.
var openFileForSamples = func(path string) (readSeekCloser, error) {
	return os.Open(path)
}

// readLinesWithTarget reads lines [startLine, endLine] (1-based,
// inclusive) and returns them PLUS the byte offset of `targetLine`.
//
// When idx is non-nil and has a checkpoint at-or-before startLine, we
// seek to that checkpoint first instead of scanning from byte 0. This
// is the "index-aware seek" path: line-offset queries get O(1)
// seek-to-chunk when the unified index is cached.
//
// # Bounded-read contract
//
// This function MUST stop reading as soon as it has produced its
// result. In particular the loop terminates once currentLine > endLine,
// EXCEPT when the caller still needs the byte offset of a targetLine
// we haven't passed yet. The range-only path (readLineRangeWithIndex)
// passes targetLine = -1 to signal "no offset needed"; the target
// sentinel check below MUST treat that as "nothing to wait for". See
// the original condition `targetOffset >= 0` kept the loop
// running to EOF on every range request because a -1 targetLine
// never matches currentLine, so targetOffset stayed -1 forever and
// the break was unreachable. This caused a 225× slowdown on large
// files (1.3 GB file, lines=1-1000 range: 2.8 ms Python vs 636 ms Go
// before fix; ~30-60 ms after fix).
func readLinesWithTarget(
	path string, startLine, endLine, targetLine int64,
	idx *rxtypes.UnifiedFileIndex,
) (lines []string, targetOffset int64, err error) {
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine {
		return []string{}, -1, nil
	}

	// Decide seek origin: closest checkpoint <= startLine, or 0.
	seekOffset, seekLine := chooseSeekOrigin(idx, startLine)

	f, err := openFileForSamples(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()

	if seekOffset > 0 {
		if _, err := f.Seek(seekOffset, io.SeekStart); err != nil {
			return nil, 0, err
		}
	}

	br := bufio.NewReader(f)
	currentLine := seekLine
	offset := seekOffset
	targetOffset = -1
	// needTarget is TRUE when the caller requested a specific line's
	// byte offset (single-line mode); FALSE when they only need the
	// range content (passed targetLine < 0). This boolean is the
	// the break condition below must not wait for
	// a target that will never be found when none was requested.
	needTarget := targetLine >= 0

	for {
		// Record offsets BEFORE reading each line — `offset` holds the
		// byte position where the next ReadString('\n') will start,
		// which IS the start of currentLine.
		if needTarget && currentLine == targetLine {
			targetOffset = offset
		}
		line, readErr := br.ReadString('\n')
		// A file that ends with a newline gives one final zero-length
		// read. That is the end of the file, not an empty last line:
		// appending it invented a line the file does not have, which is
		// what the compressed paths and rx-python have always known.
		if len(line) > 0 && currentLine >= startLine && currentLine <= endLine {
			lines = append(lines, stripNewline(line))
		}
		offset += int64(len(line))
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, 0, readErr
		}
		currentLine++
		// Break as soon as we're past the requested range AND, if a
		// targetLine was requested, we've already captured its offset.
		// When needTarget is false (range-only path), the second clause
		// is automatically satisfied and we break immediately once past
		// endLine. Without that break a range request read the whole
		// file to produce a slice of it.
		pastRange := currentLine > endLine
		haveTargetOrDontNeedIt := !needTarget || targetOffset >= 0
		if pastRange && haveTargetOrDontNeedIt {
			break
		}
	}
	return lines, targetOffset, nil
}

// readLineRangeWithIndex is the range-path sibling of
// readLinesWithTarget. Returns only the lines in [startLine, endLine];
// the byte offset is not needed for range queries (Python returns -1).
func readLineRangeWithIndex(
	path string, startLine, endLine int64,
	idx *rxtypes.UnifiedFileIndex,
) ([]string, error) {
	lines, _, err := readLinesWithTarget(path, startLine, endLine, -1, idx)
	return lines, err
}

// readLineRange is the index-free sibling used by the byte-offset path.
// Returns the lines and the offset of startLine (classic signature).
func readLineRange(path string, startLine, endLine int64) ([]string, int64, error) {
	lines, off, err := readLinesWithTarget(path, startLine, endLine, startLine, nil)
	return lines, off, err
}

// chooseSeekOrigin walks idx.LineIndex in reverse and returns the
// checkpoint at-or-before targetLine. Returns (0, 1) when no index is
// available — the caller scans from the top.
//
// The unified index stores [[line_number, byte_offset], ...] pairs so
// we can jump straight to the nearest checkpoint. Without it, a linear
// scan is still correct, just slower.
func chooseSeekOrigin(idx *rxtypes.UnifiedFileIndex, targetLine int64) (offset, line int64) {
	if idx == nil || len(idx.LineIndex) == 0 {
		return 0, 1
	}
	if entry := index.FindNearestCheckpoint(idx, targetLine); entry.LineNumber > 0 {
		return entry.ByteOffset, entry.LineNumber
	}
	return 0, 1
}

// lineNumberForOffset returns the 1-based line number containing offset.
func lineNumberForOffset(path string, offset int64) (int64, error) {
	lines, err := lineNumbersForOffsets(path, []int64{offset}, nil)
	if err != nil {
		return 0, err
	}
	return lines[offset], nil
}

// lineNumbersForOffsets resolves every offset in one pass.
//
// The offsets are sorted and answered as the file goes by, so asking
// about twenty matches costs one read rather than twenty. That is how
// the viewer asks: a capped search leaves it with a handful of offsets
// whose line numbers it wants at once, and a scan per offset turned
// that into twenty reads of the same multi-gigabyte file.
//
// An index shortens the pass further, since the checkpoint before the
// lowest offset is a known line at a known byte. Without one the pass
// starts at the beginning, which is the only place a line count can
// start from.
func lineNumbersForOffsets(
	path string,
	offsets []int64,
	idx *rxtypes.UnifiedFileIndex,
) (map[int64]int64, error) {
	out := make(map[int64]int64, len(offsets))
	if len(offsets) == 0 {
		return out, nil
	}
	sorted := append([]int64(nil), offsets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// Nearest checkpoint at or before the first offset we need.
	startOffset, startLine := int64(0), int64(1)
	if idx != nil {
		if entry := index.FindNearestCheckpointForOffset(idx, sorted[0]); entry.LineNumber > 0 {
			startOffset, startLine = entry.ByteOffset, entry.LineNumber
		}
	}

	f, err := openFileForSamples(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return nil, err
		}
	}

	br := bufio.NewReaderSize(f, readBufferFor(sorted[len(sorted)-1]-startOffset))
	pos, lineNum, next := startOffset, startLine, 0
	for next < len(sorted) {
		line, readErr := br.ReadString('\n')
		end := pos + int64(len(line))
		for next < len(sorted) && sorted[next] < end {
			if sorted[next] >= pos {
				out[sorted[next]] = lineNum
			}
			next++
		}
		if readErr != nil {
			// Past the end of the file: every remaining offset is on the
			// last line, which is what a single lookup used to report.
			for ; next < len(sorted); next++ {
				out[sorted[next]] = lineNum
			}
			break
		}
		pos, lineNum = end, lineNum+1
	}
	return out, nil
}

// countLines returns the number of '\n' bytes + 1 if the final chunk
// has unterminated content. Matches Python's `sum(1 for _ in open(p, 'rb'))`.
func countLines(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	var (
		total       int64
		tailHasData bool
	)
	buf := make([]byte, 64*1024)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			total += int64(bytes.Count(chunk, []byte{'\n'}))
			// If the last byte of the last chunk is NOT a newline,
			// there's a trailing unterminated line to count. gosec
			// needs the indexing to go through the slice bound it
			// already proved (`len(chunk) == n > 0`).
			tailHasData = chunk[len(chunk)-1] != '\n'
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, err
		}
	}
	if tailHasData {
		total++
	}
	return total, nil
}

// stripNewline drops a trailing '\n' and an optional preceding '\r'.
// Matches Python's `line.rstrip('\n\r')` when working with bytes-mode
// file iteration.
func stripNewline(s string) string {
	if len(s) == 0 {
		return s
	}
	if s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s
}

// FormatInt64 returns the JSON-compatible decimal form of n. Exposed
// so callers building response keys can avoid importing strconv just
// for this.
func FormatInt64(n int64) string { return strconv.FormatInt(n, 10) }

// ErrInvalidRequest is returned when a Request has neither Offsets nor
// Lines set, or has both set at once.
var ErrInvalidRequest = fmt.Errorf("samples.Resolve: exactly one of Offsets / Lines must be set")
