// Package seekableindex builds the line index for a seekable-zstd file.
//
// A seekable `.zst` carries a seek table, so any frame can be
// decompressed on its own. What the table does not say is which lines a
// frame holds, and that is what turns `samples --lines=5000` on a
// compressed log from "decompress frames until the line turns up" into
// "decompress one frame".
//
// The index this produces is the one rx-python writes
// (`src/rx/seekable_index.py::build_index`). rx-python is the reference
// for the cache format and the two backends share one cache directory,
// so the field set, the checkpoint spacing and the frame table are
// reproduced rather than reinvented — an index either backend writes has
// to be readable by the other.
package seekableindex

import (
	"bytes"
	"fmt"
	"os"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// CheckpointLineInterval is how often an extra checkpoint is recorded
// inside a frame that holds many lines. Every frame gets one checkpoint
// for its first line; a frame with more lines than this gets more, so a
// lookup inside a very large frame does not scan the whole frame.
//
// rx-python uses the same value (LINE_INDEX_INTERVAL) and the number is
// part of the shared cache format's shape: an index written by either
// backend has checkpoints in the same places.
const CheckpointLineInterval = 10000

// Result is what walking the frames of a seekable file establishes.
// The caller composes it into a UnifiedFileIndex together with the
// identity fields, which are the same for every file type.
type Result struct {
	Frames                []rxtypes.FrameLineInfo
	LineIndex             []rxtypes.LineIndexEntry
	LineCount             int64
	FrameCount            int
	FrameSizeTarget       int64
	DecompressedSizeBytes int64
}

// Build reads a seekable-zstd file's seek table, decompresses every
// frame in order and reports which lines each frame holds.
//
// The whole file is decompressed once, which is the only way to count
// lines in it; that cost is why the result is cached. A file whose seek
// table is missing or corrupt is an error rather than a partial index:
// an index that describes the wrong frames is worse than none, because
// every later lookup trusts it.
func Build(zstPath string) (*Result, error) {
	file, err := os.Open(zstPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", zstPath, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", zstPath, err)
	}

	table, err := seekable.ReadSeekTable(file, info.Size())
	if err != nil {
		return nil, fmt.Errorf("read seek table of %s: %w", zstPath, err)
	}
	if len(table.Frames) == 0 {
		return nil, fmt.Errorf("%s has an empty seek table", zstPath)
	}

	decoder := seekable.NewDecoder()
	result := &Result{
		Frames:          make([]rxtypes.FrameLineInfo, 0, len(table.Frames)),
		LineIndex:       make([]rxtypes.LineIndexEntry, 0, len(table.Frames)),
		FrameCount:      len(table.Frames),
		FrameSizeTarget: table.Frames[0].DecompressedSize,
	}

	currentLine := int64(1)
	for i, frame := range table.Frames {
		data, dErr := decoder.DecompressFrame(zstPath, frame.Index, table)
		if dErr != nil {
			return nil, fmt.Errorf("decompress frame %d of %s: %w", frame.Index, zstPath, dErr)
		}

		// A frame boundary can fall mid-line, so a frame's line count is
		// the number of newlines it contains: the line that continues
		// into the next frame belongs to whichever frame terminates it.
		// The exception is the last frame, where text after the final
		// newline is a real line that nothing will terminate.
		linesInFrame := int64(bytes.Count(data, []byte{'\n'}))
		isLastFrame := i == len(table.Frames)-1
		if isLastFrame && len(data) > 0 && data[len(data)-1] != '\n' {
			linesInFrame++
		}

		firstLine := currentLine
		lastLine := currentLine
		if linesInFrame > 0 {
			lastLine = currentLine + linesInFrame - 1
		}

		frameIndex := frame.Index
		result.Frames = append(result.Frames, rxtypes.FrameLineInfo{
			Index:              frame.Index,
			CompressedOffset:   frame.CompressedOffset,
			CompressedSize:     frame.CompressedSize,
			DecompressedOffset: frame.DecompressedOffset,
			DecompressedSize:   frame.DecompressedSize,
			FirstLine:          firstLine,
			LastLine:           lastLine,
			LineCount:          lastLine - firstLine + 1,
		})
		result.LineIndex = append(result.LineIndex, rxtypes.LineIndexEntry{
			LineNumber: firstLine,
			ByteOffset: frame.DecompressedOffset,
			FrameIndex: &frameIndex,
		})

		if linesInFrame > CheckpointLineInterval {
			result.LineIndex = append(result.LineIndex,
				interiorCheckpoints(data, frame, firstLine)...)
		}

		currentLine = lastLine + 1
		result.LineCount += linesInFrame
		result.DecompressedSizeBytes += frame.DecompressedSize
	}

	return result, nil
}

// interiorCheckpoints records a checkpoint every CheckpointLineInterval
// lines inside one frame, so a lookup lands near its line rather than at
// the frame's start.
//
// The offsets are positions in the decompressed stream, which is what
// every other line index in rx addresses.
func interiorCheckpoints(
	data []byte,
	frame seekable.FrameInfo,
	firstLine int64,
) []rxtypes.LineIndexEntry {
	var out []rxtypes.LineIndexEntry
	byteOffset := int64(0)
	lineNumber := firstLine
	frameIndex := frame.Index

	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if lineNumber > firstLine && (lineNumber-firstLine)%CheckpointLineInterval == 0 {
			out = append(out, rxtypes.LineIndexEntry{
				LineNumber: lineNumber,
				ByteOffset: frame.DecompressedOffset + byteOffset,
				FrameIndex: &frameIndex,
			})
		}
		// +1 for the newline bytes.Split consumed.
		byteOffset += int64(len(line)) + 1
		lineNumber++
	}
	return out
}
