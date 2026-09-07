package trace

import (
	"bufio"
	"errors"
	"io"
	"os"
	"sort"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// resolveLinesFromIndex maps byte offsets to the 1-based line numbers
// of the lines that contain them, for offsets the scan itself could not
// number.
//
// A completed scan needs none of this: the workers count newlines as
// they feed ripgrep, so every match already knows its line. Offsets
// arrive here only when a chunk was canceled part-way through — a
// max_results cap fired, or the request was aborted — which leaves the
// running count short for every chunk after it.
//
// The file's line index is what makes the answer cheap: bisect to the
// checkpoint at or before the first offset and count forward from
// there. Without an index the honest answer is "unknown", and this
// returns nil: counting newlines from byte 0 of a multi-gigabyte file
// is the work the cap was set to avoid. rx-python declines it for the
// same reason (unified_index.calculate_lines_for_offsets_batch).
//
// The read is bounded by the span the caller asked about: it starts at
// the checkpoint before the lowest offset and stops at the line holding
// the highest one.
func resolveLinesFromIndex(path string, offsets []int64) map[int64]int {
	if len(offsets) == 0 {
		return nil
	}
	idx, err := index.LoadForSource(path)
	if err != nil || idx == nil || len(idx.LineIndex) == 0 {
		return nil
	}

	wanted := make([]int64, 0, len(offsets))
	seen := make(map[int64]struct{}, len(offsets))
	for _, off := range offsets {
		if off < 0 {
			continue
		}
		if _, dup := seen[off]; dup {
			continue
		}
		seen[off] = struct{}{}
		wanted = append(wanted, off)
	}
	if len(wanted) == 0 {
		return nil
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i] < wanted[j] })

	// Nearest checkpoint at or before the first offset we need. The
	// binary search in the index package is the single implementation of
	// this lookup; five hand-rolled linear scans used to answer it.
	startLine, startOffset := int64(1), int64(0)
	if entry := index.FindNearestCheckpointForOffset(idx, wanted[0]); entry.LineNumber > 0 {
		startLine, startOffset = entry.LineNumber, entry.ByteOffset
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return nil
	}

	out := make(map[int64]int, len(wanted))
	r := bufio.NewReaderSize(f, 256*1024)
	pos, line, next := startOffset, startLine, 0
	for next < len(wanted) {
		chunk, readErr := r.ReadBytes('\n')
		end := pos + int64(len(chunk))
		// Every wanted offset that falls inside this line takes its number.
		for next < len(wanted) && wanted[next] < end {
			if wanted[next] >= pos {
				out[wanted[next]] = int(line)
			}
			next++
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return out
			}
			break
		}
		pos = end
		line++
	}
	return out
}

// resolveUnknownLineNumbers fills in the line numbers left unknown by a
// canceled scan, in place, for every file that has an index.
//
// fileIDs maps the response's file IDs ("f1") to paths; matches and
// context lines carry the ID, so the lookup goes through it. Anything
// that stays unresolved keeps the unknown marker: a wrong line number
// is worse than an admitted gap.
func resolveUnknownLineNumbers(
	fileIDs map[string]string,
	matches []rxtypes.Match,
	contexts []contextWithFile,
) {
	// Group the unknown offsets by file so each file is read once.
	byFile := map[string][]int64{}
	for _, m := range matches {
		if m.AbsoluteLineNumber < 1 {
			byFile[m.File] = append(byFile[m.File], m.Offset)
		}
	}
	for _, c := range contexts {
		if c.ctx.AbsoluteLineNumber < 1 && c.ctx.AbsoluteOffset >= 0 {
			byFile[c.fileID] = append(byFile[c.fileID], c.ctx.AbsoluteOffset)
		}
	}
	if len(byFile) == 0 {
		return
	}

	resolved := map[string]map[int64]int{}
	for fileID, offsets := range byFile {
		path, ok := fileIDs[fileID]
		if !ok || compression.IsCompressed(path) {
			// Byte offsets in a compressed file address the compressed
			// bytes, which the line index does not describe.
			continue
		}
		if lines := resolveLinesFromIndex(path, offsets); len(lines) > 0 {
			resolved[fileID] = lines
		}
	}
	if len(resolved) == 0 {
		return
	}

	for i := range matches {
		lines, ok := resolved[matches[i].File]
		if !ok || matches[i].AbsoluteLineNumber >= 1 {
			continue
		}
		if line, found := lines[matches[i].Offset]; found {
			matches[i].AbsoluteLineNumber = line
			matches[i].RelativeLineNumber = ptrInt(line)
		}
	}
	for i := range contexts {
		lines, ok := resolved[contexts[i].fileID]
		if !ok || contexts[i].ctx.AbsoluteLineNumber >= 1 {
			continue
		}
		if line, found := lines[contexts[i].ctx.AbsoluteOffset]; found {
			contexts[i].ctx.AbsoluteLineNumber = line
			contexts[i].ctx.RelativeLineNumber = line
		}
	}
}
