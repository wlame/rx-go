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
	"io"

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
// frame in order and reports which lines each frame holds. The file is
// read through r, size bytes long: an open file the caller checked, so
// no path is looked up again here.
//
// The whole file is decompressed once, which is the only way to count
// lines in it; that cost is why the result is cached. A file whose seek
// table is missing or corrupt is an error rather than a partial index:
// an index that describes the wrong frames is worse than none, because
// every later lookup trusts it.
func Build(r io.ReaderAt, size int64) (*Result, error) {
	return BuildAndCopyText(r, size, io.Discard)
}

// BuildAndCopyText is Build that also writes the file's decompressed
// text to text, one frame after another in file order, as each frame is
// decoded. The concatenated writes are exactly the file's text, so a
// caller that needs to read every line (the analysis does) gets it from
// the same single decompression pass that builds the frame table.
//
// A write that fails stops the build, and the error returned wraps the
// writer's error: a caller reading the copy through an io.Pipe makes the
// build stop by closing its end, and a result is never returned for a
// pass whose copy was cut short.
//
// Go note: io.ReaderAt reads at an explicit position and keeps no
// cursor, so an *os.File passed here can be read by other goroutines at
// the same time without either moving the other's position.
func BuildAndCopyText(r io.ReaderAt, size int64, text io.Writer) (*Result, error) {
	table, err := seekable.ReadSeekTable(r, size)
	if err != nil {
		return nil, fmt.Errorf("read seek table: %w", err)
	}

	decoder := seekable.NewDecoder()
	result := &Result{
		Frames:     make([]rxtypes.FrameLineInfo, 0, len(table.Frames)),
		LineIndex:  make([]rxtypes.LineIndexEntry, 0, len(table.Frames)),
		FrameCount: len(table.Frames),
	}
	// A table of no frames is a file of no text, which `rx compress`
	// writes for an empty input: its index has no frames, no
	// checkpoints and no lines, like the index of an empty plain file.
	// The loop below then never runs. With no frame to measure, the
	// frame size target is the encoder's default, the value rx-python
	// records for such a file.
	result.FrameSizeTarget = seekable.DefaultFrameSize
	if len(table.Frames) > 0 {
		result.FrameSizeTarget = table.Frames[0].DecompressedSize
	}

	// linesBefore is the number of line breaks in the text before the
	// frame being read, so the frame's first byte is on line
	// linesBefore+1. endsAtLineStart says whether the text read so far
	// is empty or ends with a line break, which decides whether a line
	// is still open when the last frame ends.
	linesBefore := int64(0)
	endsAtLineStart := true
	for i, frame := range table.Frames {
		data, dErr := decoder.DecompressFrameAt(r, frame.Index, table)
		if dErr != nil {
			return nil, fmt.Errorf("decompress frame %d: %w", frame.Index, dErr)
		}
		if _, wErr := text.Write(data); wErr != nil {
			return nil, fmt.Errorf("copy text of frame %d: %w", frame.Index, wErr)
		}

		lineBreaks := int64(bytes.Count(data, []byte{'\n'}))
		if len(data) > 0 {
			endsAtLineStart = data[len(data)-1] == '\n'
		}
		isLastFrame := i == len(table.Frames)-1
		linesEnded := linesEndedInFrame(lineBreaks, isLastFrame, endsAtLineStart)

		// INVARIANT: a frame's first line is the line that holds its
		// first byte, and the lines it holds are the ones it ends, so
		// its last line is first + ended - 1. A frame boundary can fall
		// mid-line, and a frame inside a line longer than a frame ends
		// no line at all: it holds zero lines (last = first - 1) and the
		// next frame starts on the same line. Counting such a frame as
		// holding a line numbered every later frame one line too high.
		firstLine := linesBefore + 1
		lastLine := firstLine + linesEnded - 1

		frameIndex := frame.Index
		result.Frames = append(result.Frames, rxtypes.FrameLineInfo{
			Index:              frame.Index,
			CompressedOffset:   frame.CompressedOffset,
			CompressedSize:     frame.CompressedSize,
			DecompressedOffset: frame.DecompressedOffset,
			DecompressedSize:   frame.DecompressedSize,
			FirstLine:          firstLine,
			LastLine:           lastLine,
			LineCount:          linesEnded,
		})
		// The frame's checkpoint names the line holding its first byte,
		// which is not always where that line starts. A frame of no
		// text has no first byte: its offset is the next frame's start
		// (which has its own checkpoint) or the end of the text, where
		// no line is, so it gets no checkpoint.
		if frame.DecompressedSize > 0 {
			result.LineIndex = append(result.LineIndex, rxtypes.LineIndexEntry{
				LineNumber: firstLine,
				ByteOffset: frame.DecompressedOffset,
				FrameIndex: &frameIndex,
			})
		}

		if linesEnded > CheckpointLineInterval {
			result.LineIndex = append(result.LineIndex,
				interiorCheckpoints(data, frame, firstLine)...)
		}

		linesBefore += lineBreaks
		result.LineCount += linesEnded
		result.DecompressedSizeBytes += frame.DecompressedSize
	}

	return result, nil
}

// linesEndedInFrame is the number of lines a frame ends: one per line
// break in it, plus, in the last frame, the line that no break ends
// when the text does not finish with one. That last line can be open
// since an earlier frame, so whether it exists is a property of the
// whole text (textEndsAtLineStart), not of the last frame's bytes.
func linesEndedInFrame(lineBreaks int64, isLastFrame, textEndsAtLineStart bool) int64 {
	if isLastFrame && !textEndsAtLineStart {
		return lineBreaks + 1
	}
	return lineBreaks
}

// interiorCheckpoints records a checkpoint every CheckpointLineInterval
// lines inside one frame, so a lookup lands near its line rather than at
// the frame's start.
//
// The offsets are positions in the decompressed stream, which is what
// every other line index in rx addresses.
//
// INVARIANT: a checkpoint names a line that starts inside this frame.
// When the frame ends with a newline, bytes.Split yields one more,
// empty element that starts at the frame's end; that position is the
// next frame's start (which has its own checkpoint) or, in the last
// frame, the end of the text, where no line starts. It never gets a
// checkpoint.
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
		if byteOffset >= int64(len(data)) {
			break
		}
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
