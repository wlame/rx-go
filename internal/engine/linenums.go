// linenums.go resolves absolute line numbers for matches found during chunked search.
//
// When ripgrep processes a chunk (not the full file), the line numbers it reports are
// relative to the chunk's data stream (starting from 1). To get absolute line numbers
// in the original file, we need the file's line-offset index.
//
// The approach matches Python's calculate_lines_for_offsets_batch: find the nearest
// index checkpoint before the target offset, then read the file from that checkpoint
// counting newlines until reaching the target offset. Multiple offsets for the same
// file are resolved in a single forward pass for efficiency.
package engine

import (
	"bufio"
	"log/slog"
	"os"
	"sort"

	"github.com/wlame/rx/internal/models"
)

// GetIndex is a function type that retrieves a FileIndex for a given file path.
// Returns nil if no index is available. This indirection lets the engine work
// without importing the index package directly.
type GetIndex func(path string) *models.FileIndex

// matchRef links a match's position in the slice to its byte offset in the file.
type matchRef struct {
	idx    int // index into matches slice
	offset int // byte offset in the file
}

// ResolveLineNumbers updates AbsoluteLineNumber for each match using the file index.
//
// For each file, it collects all match offsets, finds the best index checkpoint,
// then reads the file once counting newlines to resolve exact line numbers.
// This matches Python's calculate_lines_for_offsets_batch approach.
//
// When no index is available for a file, AbsoluteLineNumber is set to -1.
func ResolveLineNumbers(matches []models.Match, fileIDs map[string]string, getIndex GetIndex) []models.Match {
	if getIndex == nil {
		return matches
	}

	// Group match indices by file ID so we can batch-resolve per file.
	byFile := make(map[string][]matchRef)

	for i := range matches {
		m := &matches[i]
		if m.AbsoluteLineNumber > 0 {
			continue // already resolved
		}
		byFile[m.File] = append(byFile[m.File], matchRef{idx: i, offset: m.Offset})
	}

	// Cache index lookups per file ID.
	indexCache := make(map[string]*models.FileIndex)

	for fileID, refs := range byFile {
		filePath, ok := fileIDs[fileID]
		if !ok {
			for _, ref := range refs {
				matches[ref.idx].AbsoluteLineNumber = -1
			}
			continue
		}

		// Look up or fetch the file index.
		idx, cached := indexCache[fileID]
		if !cached {
			idx = getIndex(filePath)
			indexCache[fileID] = idx
		}

		if idx == nil || len(idx.LineIndex) == 0 {
			for _, ref := range refs {
				matches[ref.idx].AbsoluteLineNumber = -1
			}
			continue
		}

		// Batch-resolve all offsets for this file in a single pass.
		resolved := resolveOffsetsFromFile(filePath, idx.LineIndex, refs)

		for _, ref := range refs {
			absLine := resolved[ref.offset]
			matches[ref.idx].AbsoluteLineNumber = absLine
			if absLine > 0 {
				matches[ref.idx].RelativeLineNumber = &absLine
			}
		}
	}

	return matches
}

// resolveOffsetsFromFile resolves byte offsets to exact line numbers by reading
// the file from the nearest index checkpoint. This matches Python's
// calculate_lines_for_offsets_batch algorithm:
//
//  1. Sort target offsets
//  2. Find the index checkpoint just before the first offset
//  3. Open file, seek to checkpoint offset
//  4. Read forward line by line, counting newlines
//  5. When current_offset <= target < current_offset+line_length, record the line number
//  6. Stop when all offsets are resolved
func resolveOffsetsFromFile(filePath string, lineIndex [][]int, refs []matchRef) map[int]int {
	results := make(map[int]int, len(refs))
	for _, ref := range refs {
		results[ref.offset] = -1 // default
	}

	if len(refs) == 0 {
		return results
	}

	// Collect and sort unique offsets.
	offsets := make([]int, 0, len(refs))
	seen := make(map[int]bool, len(refs))
	for _, ref := range refs {
		if !seen[ref.offset] {
			offsets = append(offsets, ref.offset)
			seen[ref.offset] = true
		}
	}
	sort.Ints(offsets)

	// Find the index checkpoint at or before the first needed offset.
	firstOffset := offsets[0]
	checkpointLine, checkpointOffset := findCheckpoint(lineIndex, firstOffset)

	// Open the file and seek to the checkpoint.
	f, err := os.Open(filePath)
	if err != nil {
		slog.Debug("cannot open file for line resolution", "path", filePath, "error", err)
		return results
	}
	defer f.Close()

	if _, err := f.Seek(int64(checkpointOffset), 0); err != nil {
		slog.Debug("seek failed for line resolution", "path", filePath, "error", err)
		return results
	}

	// Read forward line by line, resolving offsets as we pass them.
	// This mirrors Python's "for line_bytes in f:" loop.
	currentLine := checkpointLine
	currentOffset := checkpointOffset
	offsetIdx := 0

	// Skip offsets before our start position (shouldn't happen with correct
	// checkpoint, but defensive).
	for offsetIdx < len(offsets) && offsets[offsetIdx] < checkpointOffset {
		offsetIdx++
	}

	scanner := bufio.NewScanner(f)
	// Use a large buffer — some log lines can be very long.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)

	for scanner.Scan() {
		// scanner.Bytes() does not include the \n delimiter.
		// The actual line length in the file includes the newline.
		lineLen := len(scanner.Bytes()) + 1 // +1 for \n
		lineEndOffset := currentOffset + lineLen

		// Resolve all offsets that fall within this line:
		// currentOffset <= target < lineEndOffset
		for offsetIdx < len(offsets) {
			target := offsets[offsetIdx]
			if target < currentOffset {
				// Offset before current position — skip (shouldn't happen).
				offsetIdx++
			} else if currentOffset <= target && target < lineEndOffset {
				// Target is within this line — record the line number.
				results[target] = currentLine
				offsetIdx++
			} else {
				// Target is beyond this line — move to next line.
				break
			}
		}

		// If all offsets are resolved, stop reading.
		if offsetIdx >= len(offsets) {
			break
		}

		currentOffset = lineEndOffset
		currentLine++
	}

	return results
}

// findCheckpoint finds the index checkpoint at or before the target offset.
// Returns (line_number, byte_offset) of the best checkpoint.
// The line index is a sorted list of [line_number, byte_offset] pairs.
func findCheckpoint(lineIndex [][]int, targetOffset int) (line, offset int) {
	if len(lineIndex) == 0 {
		return 1, 0
	}

	// Binary search for the largest entry whose byte offset <= target.
	lo, hi := 0, len(lineIndex)-1
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if len(lineIndex[mid]) < 2 {
			hi = mid - 1
			continue
		}
		if lineIndex[mid][1] <= targetOffset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	if len(lineIndex[lo]) < 2 {
		return 1, 0
	}

	return lineIndex[lo][0], lineIndex[lo][1]
}
