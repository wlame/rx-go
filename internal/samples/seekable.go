package samples

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// errNoFrameIndex says the file has no usable frame table, so the caller
// falls back to streaming the whole archive. It never reaches a user:
// the answer is the same either way, only slower.
var errNoFrameIndex = errors.New("samples: no frame index for this file")

// decodeSeekableFrame decompresses one frame of a seekable file, read
// through its pin. It is a variable so a test can count the frames a
// request decodes.
var decodeSeekableFrame = func(d *seekable.Decoder, src paths.Pinned, frame int, table *seekable.SeekTable) ([]byte, error) {
	f, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return d.DecompressFrameAt(f, frame, table)
}

// resolveSeekableLines answers a line-mode request on a seekable-zstd
// file by decompressing only the frames that hold the wanted lines.
//
// Without this, a `.zst` is read like any other compressed file: the
// whole archive is decompressed from byte zero until the line turns up,
// which on a multi-gigabyte log is the cost the seekable format exists
// to avoid. The frame table in the index says which frame holds a line,
// so the reader can start there.
//
// It returns errNoFrameIndex when there is no index to use, and the
// caller streams instead. An index only ever makes the answer faster.
func resolveSeekableLines(req Request, resp *rxtypes.SamplesResponse) error {
	if req.IndexLoader == nil {
		return errNoFrameIndex
	}
	idx, err := req.IndexLoader(req.Path)
	if err != nil || idx == nil || idx.Frames == nil || len(*idx.Frames) == 0 {
		return errNoFrameIndex
	}
	frames := *idx.Frames

	file, err := req.Source.Open()
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	table, err := seekable.ReadSeekTable(file, info.Size())
	if err != nil {
		return errNoFrameIndex
	}
	// An index built for a different copy of the file would address the
	// wrong frames. The staleness checks catch a rewrite; this catches
	// the case where they passed but the tables still disagree.
	if len(table.Frames) != len(frames) {
		return errNoFrameIndex
	}

	totalLines := int64(0)
	if idx.LineCount != nil {
		totalLines = *idx.LineCount
	}

	decoder := seekable.NewDecoder()
	for _, want := range req.Lines {
		if err := answerOneWindow(req, resp, want, frames, table, decoder, totalLines); err != nil {
			return err
		}
	}
	return nil
}

// answerOneWindow fills the response for a single requested line or
// range.
func answerOneWindow(
	req Request,
	resp *rxtypes.SamplesResponse,
	want OffsetOrRange,
	frames []rxtypes.FrameLineInfo,
	table *seekable.SeekTable,
	decoder *seekable.Decoder,
	totalLines int64,
) error {
	var key string
	var first, last, reported int64

	if want.IsRange() {
		key = fmt.Sprintf("%d-%d", want.Start, *want.End)
		first, last = want.Start, *want.End
	} else {
		line := want.Start
		if line == 0 {
			// Line 0 is not a line: they are numbered from 1.
			resp.Samples["0"] = nil
			resp.Lines["0"] = -1
			return nil
		}
		if line < 0 {
			// A negative line counts back from the last one. The index
			// knows the count, so this costs nothing here.
			line = totalLines + line + 1
			if line < 1 {
				line = 1
			}
		}
		key = strconv.FormatInt(line, 10)
		first = line - int64(req.BeforeContext)
		if first < 1 {
			first = 1
		}
		last = line + int64(req.AfterContext)
		reported = line
	}

	// Pre-populate as unknown, so a window past the end of the file
	// still appears in the response with the -1 and null the plain-file
	// path reports for the same question.
	resp.Samples[key] = nil
	resp.Lines[key] = -1

	lines, offsets, err := readLinesFromFrames(req.Source, frames, table, decoder, first, last)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		// Nothing was collected, so the position is not in the file.
		return nil
	}
	resp.Samples[key] = lines
	if reported > 0 {
		if offset, ok := offsets[reported]; ok {
			resp.Lines[key] = offset
		}
	}
	return nil
}

// readLinesFromFrames decompresses the smallest run of frames that can
// contain lines first..last and returns their text, plus the byte offset
// of each line in the decompressed stream.
//
// The run starts at a frame whose first byte lies on a line before
// `first` (see frameRunFor), so line `first` begins inside the run and
// every wanted line is complete. The run's own first line can be a
// partial one, and it is never in the wanted range.
func readLinesFromFrames(
	src paths.Pinned,
	frames []rxtypes.FrameLineInfo,
	table *seekable.SeekTable,
	decoder *seekable.Decoder,
	first, last int64,
) ([]string, map[int64]int64, error) {
	startFrame, endFrame, ok := frameRunFor(frames, first, last)
	if !ok {
		return []string{}, nil, nil
	}

	startOffset := frames[startFrame].DecompressedOffset
	endOffset := frames[endFrame].DecompressedOffset + frames[endFrame].DecompressedSize
	file, err := src.Open()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := decoder.DecompressRangeAt(context.Background(), file, table, startOffset, endOffset-startOffset)
	if err != nil {
		return nil, nil, err
	}

	// The first newline in the run terminates the run's first line.
	lineNumber := frames[startFrame].FirstLine
	position := startOffset
	collected := []string{}
	offsets := map[int64]int64{}

	reader := bufio.NewReaderSize(bytes.NewReader(data), 256*1024)
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) > 0 {
			if lineNumber >= first && lineNumber <= last {
				collected = append(collected, trimNewline(string(raw)))
				offsets[lineNumber] = position
			}
			position += int64(len(raw))
			lineNumber++
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, nil, readErr
		}
		if lineNumber > last {
			break
		}
	}
	return collected, offsets, nil
}

// frameRunFor picks the contiguous run of frames that has to be
// decompressed to read lines first..last, and reports false when the
// range starts past the end of the file.
//
// A frame's FirstLine is the line holding its first byte, and that line
// may have begun frames earlier: a line longer than a frame spans
// frames that hold no line break. The run therefore starts at the last
// frame whose first byte is on a line before `first` — line `first`
// begins after that byte — or at frame 0 for line 1. For a file whose
// frames end at line breaks that is the frame holding `first` or the
// one before it, so a single line costs one or two frames whatever the
// file's size. The run ends at the frame that ends line `last`.
func frameRunFor(frames []rxtypes.FrameLineInfo, first, last int64) (start, end int, ok bool) {
	if frameHoldingLine(frames, first) < 0 {
		return 0, 0, false
	}
	// sort.Search returns the first frame starting on line `first` or
	// later; the frame before it is the last one starting earlier.
	start = sort.Search(len(frames), func(i int) bool {
		return frames[i].FirstLine >= first
	}) - 1
	if start < 0 {
		start = 0
	}
	end = frameHoldingLine(frames, last)
	if end < 0 {
		end = len(frames) - 1
	}
	return start, end, true
}

// frameHoldingLine returns the index of the frame that ends `line` (the
// first frame whose LastLine reaches it), or -1 when the line is past the
// end of the file. A frame that ends no line has LastLine = FirstLine-1,
// so LastLine never decreases from frame to frame and the binary search
// holds.
func frameHoldingLine(frames []rxtypes.FrameLineInfo, line int64) int {
	found := sort.Search(len(frames), func(i int) bool {
		return frames[i].LastLine >= line
	})
	if found >= len(frames) {
		return -1
	}
	return found
}
