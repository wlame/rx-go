package index

import (
	"sort"

	"github.com/wlame/rx/internal/models"
)

// CalculateLinesForOffsets performs a batch lookup of decompressed byte offsets
// to line numbers using a FileIndex's frame information.
//
// For seekable zstd files, the index stores FrameLineInfo entries with
// first_line/last_line per frame and a decompressed_offset. We use binary
// search within frames to map arbitrary decompressed byte offsets to their
// approximate line numbers.
//
// Returns a map from each input offset to the best-known line number.
// Offsets that cannot be resolved map to -1.
func CalculateLinesForOffsets(index *models.FileIndex, offsets []int64) map[int64]int {
	result := make(map[int64]int, len(offsets))
	for _, off := range offsets {
		result[off] = -1
	}

	if index == nil || len(index.LineIndex) == 0 {
		return result
	}

	// If the index has frame info (seekable zstd), use frame-based lookup.
	if index.Frames != nil && len(*index.Frames) > 0 {
		frames := *index.Frames
		for _, off := range offsets {
			result[off] = lookupLineInFrames(frames, off)
		}
		return result
	}

	// For regular/compressed indexes, use the line_index entries directly.
	// These are sorted [line_number, byte_offset] pairs.
	for _, off := range offsets {
		result[off] = lookupLineInIndex(index.LineIndex, int(off))
	}

	return result
}

// lookupLineInFrames finds the line number for a decompressed byte offset
// by binary-searching the frame list. Each frame covers a contiguous range
// of decompressed bytes [decompressed_offset, decompressed_offset + decompressed_size).
//
// Within a frame, we interpolate the line number based on position within the frame.
func lookupLineInFrames(frames []models.FrameLineInfo, offset int64) int {
	if len(frames) == 0 {
		return -1
	}

	// Binary search for the frame containing this offset.
	// Find the last frame whose DecompressedOffset <= offset.
	idx := sort.Search(len(frames), func(i int) bool {
		return int64(frames[i].DecompressedOffset) > offset
	}) - 1

	if idx < 0 {
		return -1
	}

	frame := frames[idx]
	frameEnd := int64(frame.DecompressedOffset) + int64(frame.DecompressedSize)

	// Check that the offset actually falls within this frame.
	if offset >= frameEnd {
		return -1
	}

	// Interpolate the line number within the frame based on byte position.
	// This is an approximation when we don't have the actual frame content.
	if frame.DecompressedSize == 0 || frame.LineCount == 0 {
		return frame.FirstLine
	}

	posInFrame := offset - int64(frame.DecompressedOffset)
	fraction := float64(posInFrame) / float64(frame.DecompressedSize)
	lineInFrame := int(fraction * float64(frame.LineCount))

	lineNumber := frame.FirstLine + lineInFrame
	if lineNumber > frame.LastLine {
		lineNumber = frame.LastLine
	}
	if lineNumber < frame.FirstLine {
		lineNumber = frame.FirstLine
	}

	return lineNumber
}

// lookupLineInIndex uses the line_index (sorted by byte offset) to find the
// line number for a given byte offset via binary search.
//
// The line_index entries are [line_number, byte_offset] (2-element) or
// [line_number, byte_offset, frame_index] (3-element for seekable zstd).
// We search by the second element (byte_offset).
func lookupLineInIndex(lineIndex [][]int, offset int) int {
	if len(lineIndex) == 0 {
		return -1
	}

	// Binary search for the largest entry whose byte_offset <= offset.
	idx := sort.Search(len(lineIndex), func(i int) bool {
		if len(lineIndex[i]) < 2 {
			return false
		}
		return lineIndex[i][1] > offset
	}) - 1

	if idx < 0 {
		return -1
	}

	if len(lineIndex[idx]) < 2 {
		return -1
	}

	return lineIndex[idx][0]
}

// FindFrameForLine returns the frame index (0-based) that contains the given
// 1-based line number. Returns -1 if the line is out of range or no frames exist.
func FindFrameForLine(index *models.FileIndex, lineNumber int) int {
	if index == nil || index.Frames == nil {
		return -1
	}

	frames := *index.Frames
	for _, frame := range frames {
		if lineNumber >= frame.FirstLine && lineNumber <= frame.LastLine {
			return frame.Index
		}
	}

	return -1
}

// FindFramesForByteRange returns the indices of frames that overlap with the
// decompressed byte range [startOffset, endOffset).
func FindFramesForByteRange(index *models.FileIndex, startOffset, endOffset int64) []int {
	if index == nil || index.Frames == nil {
		return nil
	}

	var result []int
	for _, frame := range *index.Frames {
		frameStart := int64(frame.DecompressedOffset)
		frameEnd := frameStart + int64(frame.DecompressedSize)

		// Check for overlap: frame range intersects query range.
		if frameStart < endOffset && frameEnd > startOffset {
			result = append(result, frame.Index)
		}
	}

	return result
}
