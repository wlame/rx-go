// linenums.go resolves absolute line numbers for matches found during chunked search.
//
// When ripgrep processes a chunk (not the full file), the line numbers it reports are
// relative to the chunk's data stream (starting from 1). To get absolute line numbers
// in the original file, we need the file's line-offset index.
//
// If an index is available, we use its line_index entries to map byte offsets to absolute
// line numbers. If no index is available, we set AbsoluteLineNumber to -1 (matching the
// Python reference behavior).
package engine

import (
	"github.com/wlame/rx/internal/models"
)

// GetIndex is a function type that retrieves a FileIndex for a given file path.
// Returns nil if no index is available. This indirection lets the engine work
// without importing the index package directly (which is built in Phase 4).
type GetIndex func(path string) *models.FileIndex

// ResolveLineNumbers updates AbsoluteLineNumber for each match using the file index.
//
// For each match, it looks up the file path via fileIDs, retrieves the index via getIndex,
// and uses the index's line_index entries to find the absolute line number for the match's
// byte offset.
//
// When no index is available for a file, AbsoluteLineNumber is set to -1.
//
// The fileIDs map goes from file ID (e.g. "f1") to the absolute file path.
func ResolveLineNumbers(matches []models.Match, fileIDs map[string]string, getIndex GetIndex) []models.Match {
	if getIndex == nil {
		return matches
	}

	// Cache index lookups per file ID to avoid repeated disk reads.
	indexCache := make(map[string]*models.FileIndex)

	for i := range matches {
		m := &matches[i]

		// Skip matches that already have a resolved absolute line number.
		if m.AbsoluteLineNumber > 0 {
			continue
		}

		filePath, ok := fileIDs[m.File]
		if !ok {
			m.AbsoluteLineNumber = -1
			continue
		}

		// Look up or fetch the file index.
		idx, cached := indexCache[m.File]
		if !cached {
			idx = getIndex(filePath)
			indexCache[m.File] = idx // may be nil
		}

		if idx == nil || len(idx.LineIndex) == 0 {
			m.AbsoluteLineNumber = -1
			continue
		}

		// Use the line index to find the absolute line number for this byte offset.
		absLine := lookupLineNumber(idx.LineIndex, m.Offset)
		m.AbsoluteLineNumber = absLine

		// Also update RelativeLineNumber to match (for regular, non-compressed files
		// the relative and absolute line numbers are the same once resolved).
		if absLine > 0 {
			m.RelativeLineNumber = &absLine
		}
	}

	return matches
}

// lookupLineNumber uses the line index to find the absolute line number for a byte offset.
//
// The line index is a sorted list of [line_number, byte_offset] pairs sampled at intervals.
// We binary-search for the closest entry at or before the target offset, then count
// forward from there. Since we don't have the actual file content here, we return
// the interpolated line number based on the nearest index checkpoint.
//
// Returns -1 if the offset cannot be resolved.
func lookupLineNumber(lineIndex [][]int, offset int) int {
	if len(lineIndex) == 0 {
		return -1
	}

	// Binary search for the largest entry whose byte offset <= target offset.
	lo, hi := 0, len(lineIndex)-1
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if len(lineIndex[mid]) < 2 {
			hi = mid - 1
			continue
		}
		if lineIndex[mid][1] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	if len(lineIndex[lo]) < 2 {
		return -1
	}

	entryLine := lineIndex[lo][0]
	entryOffset := lineIndex[lo][1]

	// If the offset matches exactly, return the line number directly.
	if entryOffset == offset {
		return entryLine
	}

	// The offset is between this index entry and the next one. Without reading
	// the file to count newlines, we can only return the checkpoint line number.
	// This is an approximation — exact resolution requires a file read pass
	// (done in Phase 4 via calculate_lines_for_offsets_batch).
	// For now, return the checkpoint as the best available estimate.
	if entryOffset < offset {
		return entryLine
	}

	return -1
}
