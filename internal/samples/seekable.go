package samples

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// errNoFrameIndex says the file has no usable frame table, so the caller
// falls back to streaming the whole archive. It never reaches a user:
// the answer is the same either way, only slower.
var errNoFrameIndex = errors.New("samples: no frame index for this file")

// decodeSeekableFrame decompresses one frame of a seekable file. It is
// a variable so a test can count the frames a request decodes.
var decodeSeekableFrame = func(d *seekable.Decoder, path string, frame int, table *seekable.SeekTable) ([]byte, error) {
	return d.DecompressFrame(path, frame, table)
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

	file, err := os.Open(req.Path)
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

	lines, offsets, err := readLinesFromFrames(req.Path, frames, table, decoder, first, last)
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
// The run starts one frame earlier than the frame holding `first`,
// because a line is attributed to the frame containing its terminating
// newline and its text may begin in the frame before. Starting there
// makes every wanted line complete; the run's own first line is the
// partial one, and it is never in the wanted range.
func readLinesFromFrames(
	path string,
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
	data, err := decoder.DecompressRange(context.Background(), path, table, startOffset, endOffset-startOffset)
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
// The run begins one frame before the frame holding `first`, because a
// line is attributed to the frame containing its terminating newline and
// its text may begin in the frame before. That extra frame is what makes
// the wanted lines complete, and it is the only overhead: for a single
// line the run is two frames whatever the file's size.
func frameRunFor(frames []rxtypes.FrameLineInfo, first, last int64) (start, end int, ok bool) {
	start = frameHoldingLine(frames, first)
	if start < 0 {
		return 0, 0, false
	}
	if start > 0 {
		start--
	}
	end = frameHoldingLine(frames, last)
	if end < 0 {
		end = len(frames) - 1
	}
	return start, end, true
}

// frameHoldingLine returns the index of the frame whose line range
// contains `line`, or -1 when the line is past the end of the file.
func frameHoldingLine(frames []rxtypes.FrameLineInfo, line int64) int {
	found := sort.Search(len(frames), func(i int) bool {
		return frames[i].LastLine >= line
	})
	if found >= len(frames) {
		return -1
	}
	return found
}
