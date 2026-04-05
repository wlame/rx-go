package index

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wlame/rx/internal/models"
)

func makeSeekableIndex() *models.FileIndex {
	frames := []models.FrameLineInfo{
		{Index: 0, DecompressedOffset: 0, DecompressedSize: 1000, FirstLine: 1, LastLine: 50, LineCount: 50},
		{Index: 1, DecompressedOffset: 1000, DecompressedSize: 1000, FirstLine: 51, LastLine: 100, LineCount: 50},
		{Index: 2, DecompressedOffset: 2000, DecompressedSize: 1000, FirstLine: 101, LastLine: 150, LineCount: 50},
	}

	idx := models.NewFileIndex(UnifiedIndexVersion, models.IndexTypeSeekableZstd, "/test.zst", "2024-01-01T00:00:00Z", 3000)
	idx.Frames = &frames
	idx.LineIndex = [][]int{
		{1, 0, 0},
		{51, 1000, 1},
		{101, 2000, 2},
	}
	return &idx
}

func TestCalculateLinesForOffsets_FrameBased(t *testing.T) {
	idx := makeSeekableIndex()

	offsets := []int64{0, 500, 1000, 1500, 2500}
	result := CalculateLinesForOffsets(idx, offsets)

	// Offset 0 should be in frame 0 (line 1).
	assert.Equal(t, 1, result[0])

	// Offset 500 should be roughly in the middle of frame 0.
	assert.True(t, result[500] >= 1 && result[500] <= 50,
		"offset 500 should map to a line in frame 0 (1-50), got %d", result[500])

	// Offset 1000 should be start of frame 1 (line 51).
	assert.Equal(t, 51, result[1000])

	// Offset 1500 should be in frame 1.
	assert.True(t, result[1500] >= 51 && result[1500] <= 100,
		"offset 1500 should map to a line in frame 1 (51-100), got %d", result[1500])

	// Offset 2500 should be in frame 2.
	assert.True(t, result[2500] >= 101 && result[2500] <= 150,
		"offset 2500 should map to a line in frame 2 (101-150), got %d", result[2500])
}

func TestCalculateLinesForOffsets_IndexBased(t *testing.T) {
	// Regular file index with line_index entries (no frames).
	idx := models.NewFileIndex(UnifiedIndexVersion, models.IndexTypeRegular, "/test.log", "2024-01-01T00:00:00Z", 10000)
	idx.LineIndex = [][]int{
		{1, 0},
		{100, 5000},
		{200, 9000},
	}

	offsets := []int64{0, 2500, 5000, 7000, 9000}
	result := CalculateLinesForOffsets(&idx, offsets)

	assert.Equal(t, 1, result[0])
	assert.Equal(t, 1, result[2500])     // Between first and second checkpoint.
	assert.Equal(t, 100, result[5000])   // Exact match on second checkpoint.
	assert.Equal(t, 100, result[7000])   // Between second and third checkpoint.
	assert.Equal(t, 200, result[9000])   // Exact match on third checkpoint.
}

func TestCalculateLinesForOffsets_NilIndex(t *testing.T) {
	offsets := []int64{100, 200}
	result := CalculateLinesForOffsets(nil, offsets)

	assert.Equal(t, -1, result[100])
	assert.Equal(t, -1, result[200])
}

func TestCalculateLinesForOffsets_EmptyOffsets(t *testing.T) {
	idx := makeSeekableIndex()
	result := CalculateLinesForOffsets(idx, nil)
	assert.Empty(t, result)
}

func TestCalculateLinesForOffsets_OffsetBeyondFile(t *testing.T) {
	idx := makeSeekableIndex()
	offsets := []int64{9999} // Beyond the 3000 bytes of data.
	result := CalculateLinesForOffsets(idx, offsets)
	assert.Equal(t, -1, result[9999])
}

func TestCalculateLinesForOffsets_FrameBoundary(t *testing.T) {
	idx := makeSeekableIndex()

	// Test exact frame boundary offsets.
	offsets := []int64{999, 1000, 1999, 2000}
	result := CalculateLinesForOffsets(idx, offsets)

	// 999 is last byte of frame 0.
	assert.True(t, result[999] >= 1 && result[999] <= 50)

	// 1000 is first byte of frame 1.
	assert.Equal(t, 51, result[1000])

	// 1999 is last byte of frame 1.
	assert.True(t, result[1999] >= 51 && result[1999] <= 100)

	// 2000 is first byte of frame 2.
	assert.Equal(t, 101, result[2000])
}

func TestFindFrameForLine(t *testing.T) {
	idx := makeSeekableIndex()

	assert.Equal(t, 0, FindFrameForLine(idx, 1))
	assert.Equal(t, 0, FindFrameForLine(idx, 25))
	assert.Equal(t, 0, FindFrameForLine(idx, 50))
	assert.Equal(t, 1, FindFrameForLine(idx, 51))
	assert.Equal(t, 1, FindFrameForLine(idx, 100))
	assert.Equal(t, 2, FindFrameForLine(idx, 101))
	assert.Equal(t, 2, FindFrameForLine(idx, 150))
	assert.Equal(t, -1, FindFrameForLine(idx, 151))
	assert.Equal(t, -1, FindFrameForLine(idx, 0))
}

func TestFindFrameForLine_NilIndex(t *testing.T) {
	assert.Equal(t, -1, FindFrameForLine(nil, 1))
}

func TestFindFramesForByteRange(t *testing.T) {
	idx := makeSeekableIndex()

	// Range spanning first two frames.
	frames := FindFramesForByteRange(idx, 500, 1500)
	assert.Equal(t, []int{0, 1}, frames)

	// Range within single frame.
	frames = FindFramesForByteRange(idx, 100, 500)
	assert.Equal(t, []int{0}, frames)

	// Range spanning all frames.
	frames = FindFramesForByteRange(idx, 0, 3000)
	assert.Equal(t, []int{0, 1, 2}, frames)

	// Range beyond all frames.
	frames = FindFramesForByteRange(idx, 5000, 6000)
	assert.Nil(t, frames)
}

func TestLookupLineInFrames_EmptyFrames(t *testing.T) {
	assert.Equal(t, -1, lookupLineInFrames(nil, 100))
}

func TestLookupLineInIndex_EmptyIndex(t *testing.T) {
	assert.Equal(t, -1, lookupLineInIndex(nil, 100))
}
