package trace

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ReconstructMatchData rebuilds a full rxtypes.Match + its surrounding
// context lines from a minimal TraceCacheMatch entry.
//
// The cache only stores (pattern_index, offset, line_number). Everything
// else — line text, submatches — must be recomputed from the source
// file. We re-use the unified index to jump directly to the right byte
// range (checkpoint-based bisect) so cache hits stay fast.
//
// Parity: rx-python/src/rx/trace_cache.py::reconstruct_match_data.
func ReconstructMatchData(
	sourcePath string,
	cached rxtypes.TraceCacheMatch,
	patterns []string,
	patternIDs map[string]string,
	fileID string,
	rgExtraArgs []string,
	contextBefore, contextAfter int,
	useIndex bool,
) (rxtypes.Match, []rxtypes.ContextLine, error) {
	if cached.PatternIndex < 0 || cached.PatternIndex >= len(patterns) {
		return rxtypes.Match{}, nil, fmt.Errorf(
			"reconstruct: pattern_index %d out of range (have %d patterns)",
			cached.PatternIndex, len(patterns))
	}
	pattern := patterns[cached.PatternIndex]
	pid := fmt.Sprintf("p%d", cached.PatternIndex+1)

	// Pull context lines around the matched line from the source file.
	// We ask for one target line number; the helper returns the window
	// [line-before, line, line+after]. For non-indexed files we fall
	// back to a linear scan (matches Python).
	windowLines, matchedIdx, startLine, err := readLineWindow(
		sourcePath,
		int(cached.LineNumber),
		contextBefore,
		contextAfter,
		useIndex,
	)
	if err != nil {
		return rxtypes.Match{}, nil, err
	}
	var matchedLine string
	if matchedIdx >= 0 && matchedIdx < len(windowLines) {
		matchedLine = windowLines[matchedIdx]
	}

	// Re-derive submatches by running the stored pattern against the
	// matched line. Ignore-case is the only flag that changes matching.
	subs := submatchesFromPattern(pattern, matchedLine, hasFlag(rgExtraArgs, "-i", "--ignore-case"))

	// Build the Match. LineText is a pointer so we can emit JSON `null`
	// when Python would; on cache hits we always have the string, so
	// take its address.
	lineNum := cached.LineNumber
	lineText := matchedLine
	absLine := int(cached.LineNumber) // known on cache hits (complete scans only)

	m := rxtypes.Match{
		Pattern:            pid,
		File:               fileID,
		Offset:             cached.Offset,
		RelativeLineNumber: ptrInt(int(lineNum)),
		AbsoluteLineNumber: absLine,
		LineText:           &lineText,
		Submatches:         subs,
	}

	// Build context lines. Every line in the window becomes a
	// ContextLine; the matched line is included (per Python semantics).
	// Only the matched line's AbsoluteOffset is known; other context
	// lines get -1 (same as Python).
	ctxLines := make([]rxtypes.ContextLine, 0, len(windowLines))
	for i, text := range windowLines {
		ctxLineNum := startLine + i
		off := int64(-1)
		if i == matchedIdx {
			off = cached.Offset
		}
		ctxLines = append(ctxLines, rxtypes.ContextLine{
			RelativeLineNumber: ctxLineNum,
			AbsoluteLineNumber: ctxLineNum,
			LineText:           text,
			AbsoluteOffset:     off,
		})
	}

	return m, ctxLines, nil
}

// readLineWindow returns the window of lines [targetLine-contextBefore,
// targetLine+contextAfter] from sourcePath, the index of the target
// line within the window, and the starting line number of the window.
//
// Implementation strategy:
//
//  1. If useIndex and we have an index loaded, use FindNearestCheckpoint
//     to jump to the nearest checkpoint at or before targetLine, then
//     scan forward line-by-line until we reach the window.
//  2. Otherwise (no index available, or useIndex=false), do a linear
//     scan from byte 0 counting newlines.
//
// Near-start-of-file truncation: if targetLine - contextBefore < 1,
// the window starts at line 1 and matchedIdx shrinks accordingly.
func readLineWindow(
	sourcePath string,
	targetLine, contextBefore, contextAfter int,
	useIndex bool,
) (lines []string, matchedIdx, startLine int, err error) {
	if targetLine < 1 {
		return nil, -1, 0, fmt.Errorf("readLineWindow: target line %d < 1", targetLine)
	}

	// Open the source file. bufio.Scanner iterates by line; we give it
	// a generous buffer so multi-MB log lines don't trip the default
	// 64 KB limit.
	f, err := os.Open(sourcePath)
	if err != nil {
		return nil, -1, 0, fmt.Errorf("readLineWindow: open %s: %w", sourcePath, err)
	}
	defer func() { _ = f.Close() }()

	// Target window bounds.
	startLine = targetLine - contextBefore
	if startLine < 1 {
		startLine = 1
	}
	endLine := targetLine + contextAfter

	// ====================================================================
	// Strategy 1: index-assisted jump
	// ====================================================================
	//
	// At M3 we wire this path conservatively — if the index is absent
	// or can't resolve a checkpoint, fall back to a linear scan. When
	// M2's index builder lands real line-index entries (M3+ work), this
	// hot path activates transparently.
	if useIndex {
		if idx, ldErr := index.LoadForSource(sourcePath); ldErr == nil && idx != nil {
			cp := index.FindNearestCheckpoint(idx, int64(startLine))
			if cp.LineNumber > 0 {
				// Seek to the checkpoint and scan forward.
				if _, sErr := f.Seek(cp.ByteOffset, io.SeekStart); sErr == nil {
					lines, matchedIdx = scanLines(f, int(cp.LineNumber), startLine, endLine, targetLine)
					return lines, matchedIdx, startLine, nil
				}
			}
		}
	}

	// ====================================================================
	// Strategy 2: linear scan from byte 0
	// ====================================================================
	lines, matchedIdx = scanLines(f, 1, startLine, endLine, targetLine)
	return lines, matchedIdx, startLine, nil
}

// scanLines scans from the current reader position, treating the first
// line of output as having line number `currentLine`. It collects lines
// whose line numbers fall in [startLine, endLine], returning them plus
// the index of the `targetLine` within the returned slice.
//
// If EOF hits before reaching startLine, returns an empty slice.
// If EOF hits mid-window, returns whatever was collected (the caller
// treats short windows as "end of file" — same as Python).
func scanLines(r io.Reader, currentLine, startLine, endLine, targetLine int) ([]string, int) {
	sc := bufio.NewScanner(r)
	const maxLineBytes = 16 * 1024 * 1024 // match rgjson buffer
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	var lines []string
	matchedIdx := -1
	ln := currentLine
	for sc.Scan() {
		if ln > endLine {
			break
		}
		if ln >= startLine {
			text := sc.Text()
			lines = append(lines, text)
			if ln == targetLine {
				matchedIdx = len(lines) - 1
			}
		}
		ln++
	}
	// ignore sc.Err: on a partial file we return whatever we have.
	return lines, matchedIdx
}

// submatchesFromPattern re-runs the Python-style regex against the line
// and returns a slice of Submatch byte-position records, sorted by start.
//
// The flags logic exactly mirrors identify.go::compileRegex — if we
// change matching flags in one place we update both.
func submatchesFromPattern(pattern, line string, ignoreCase bool) []rxtypes.Submatch {
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	locs := re.FindAllStringIndex(line, -1)
	subs := make([]rxtypes.Submatch, 0, len(locs))
	for _, l := range locs {
		subs = append(subs, rxtypes.Submatch{
			Text:  line[l[0]:l[1]],
			Start: l[0],
			End:   l[1],
		})
	}
	return subs
}

// ptrInt helper — returns &v.
func ptrInt(v int) *int { return &v }

// ReadSourceLine fetches exactly one line by number from the source
// file, without context. Used by GetContextByLines-style callers that
// only need the matched line itself. Currently unused within M3 but
// kept here to avoid duplicating the index-jump logic when M5 wires
// up the HTTP samples endpoint.
//
// We leave it exported so downstream modules can re-use it.
func ReadSourceLine(sourcePath string, lineNumber int, useIndex bool) (string, error) {
	lines, idx, _, err := readLineWindow(sourcePath, lineNumber, 0, 0, useIndex)
	if err != nil {
		return "", err
	}
	if idx < 0 || idx >= len(lines) {
		return "", errors.New("readSourceLine: line not found")
	}
	return lines[idx], nil
}

// largeFileThresholdBytes returns the size in bytes at which a file
// triggers unified-index + trace cache. Pulls from config so tests can
// override via RX_LARGE_FILE_MB.
func largeFileThresholdBytes() int64 {
	return int64(config.LargeFileMB()) * 1024 * 1024
}
