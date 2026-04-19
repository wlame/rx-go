package samples

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"

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

// resolveOffsets dispatches byte-offset single values and ranges.
func resolveOffsets(req Request, resp *rxtypes.SamplesResponse) error {
	fi, err := os.Stat(req.Path)
	if err != nil {
		return err
	}
	fileSize := fi.Size()

	for _, v := range req.Offsets {
		if v.IsRange() {
			// Range mode: collect every line overlapping [start, *end].
			startLine, err := lineNumberForOffset(req.Path, v.Start)
			if err != nil {
				return err
			}
			endLine, err := lineNumberForOffset(req.Path, *v.End)
			if err != nil {
				return err
			}
			lines, _, err := readLineRange(req.Path, startLine, endLine)
			if err != nil {
				return err
			}
			key := v.Key()
			resp.Samples[key] = lines
			// Offsets[key] = start line number (Python parity).
			resp.Offsets[key] = startLine
			continue
		}

		// Single offset: resolve negative, then ±context window.
		start := v.Start
		if start < 0 {
			start = fileSize + start
			if start < 0 {
				start = 0
			}
		}
		lineNum, err := lineNumberForOffset(req.Path, start)
		if err != nil {
			return err
		}
		startLine := lineNum - int64(req.BeforeContext)
		if startLine < 1 {
			startLine = 1
		}
		endLine := lineNum + int64(req.AfterContext)
		lines, _, err := readLineRange(req.Path, startLine, endLine)
		if err != nil {
			return err
		}
		// Python parity: key is the RESOLVED positive offset, not the
		// user's signed input (samples.py line 557:
		// `offset_to_line[str(start)] = line_num` where `start` has
		// been reassigned).
		key := strconv.FormatInt(start, 10)
		resp.Samples[key] = lines
		resp.Offsets[key] = lineNum
	}
	return nil
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
		resp.Samples[key] = lines
		// resp.Lines[key] = offset of line `target` (the REQUESTED
		// line), not the context window's first line. Stage 9 Round 2
		// R1-B5 fix lives in this single assignment.
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
// is the "index-aware seek" path called for in Stage 9 Round 2 R1-B4
// user design: line-offset queries get O(1) seek-to-chunk when the
// unified index is cached.
//
// # Bounded-read contract (Stage 9 Round 5)
//
// This function MUST stop reading as soon as it has produced its
// result. In particular the loop terminates once currentLine > endLine,
// EXCEPT when the caller still needs the byte offset of a targetLine
// we haven't passed yet. The range-only path (readLineRangeWithIndex)
// passes targetLine = -1 to signal "no offset needed"; the target
// sentinel check below MUST treat that as "nothing to wait for". See
// R5-B1: the original condition `targetOffset >= 0` kept the loop
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
	// Stage 9 Round 5 fix: the break condition below must not wait for
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
		if currentLine >= startLine && currentLine <= endLine {
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
		// endLine — this is the core of the R5-B1 fix.
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
	for i := len(idx.LineIndex) - 1; i >= 0; i-- {
		entry := idx.LineIndex[i]
		if entry.LineNumber <= targetLine {
			return entry.ByteOffset, entry.LineNumber
		}
	}
	return 0, 1
}

// lineNumberForOffset returns the 1-based line number containing offset.
// Linear scan from byte 0 — byte-offset mode is rarely called enough
// that an index lookup isn't necessary (the index is keyed by line,
// not byte, so a reverse lookup would require a different data structure).
func lineNumberForOffset(path string, offset int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	br := bufio.NewReader(f)
	var (
		pos     int64 = 0
		lineNum int64 = 1
	)
	for {
		line, err := br.ReadString('\n')
		next := pos + int64(len(line))
		if offset >= pos && offset < next {
			return lineNum, nil
		}
		if err != nil {
			// EOF with offset beyond EOF → clamp to last line.
			return lineNum, nil
		}
		pos = next
		lineNum++
	}
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
