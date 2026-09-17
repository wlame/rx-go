package samples

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// resolveCompressedLines answers a line-mode request by streaming the
// file through its decompressor once, keeping only the lines the
// request asks for, and stopping after the last of them.
//
// A compressed stream cannot be entered in the middle, so the pass
// always decompresses from the first byte; how far it goes is what the
// request decides. idx, the file's line index or nil, helps twice: its
// line count resolves a line counted from the end, which otherwise costs
// a first pass over the whole stream to count the lines, and its
// checkpoint before the first wanted line lets the pass drop the bytes
// before it without splitting them into lines. idx must be an index
// whose checkpoints are line starts in the text, which a seekable
// file's (at frame starts) are not.
func resolveCompressedLines(
	req Request,
	format compression.Format,
	idx *rxtypes.UnifiedFileIndex,
	resp *rxtypes.SamplesResponse,
) error {
	needTotalLines := false
	for _, v := range req.Lines {
		if !v.IsRange() && v.Start < 0 {
			needTotalLines = true
			break
		}
	}
	var totalLines int64
	if needTotalLines {
		n, err := streamLineCount(req.Source, format, idx)
		if err != nil {
			return err
		}
		totalLines = n
	}

	// Resolve negative line numbers, then turn each request into the
	// window of lines it wants, keyed the way the response reports it.
	type window struct {
		key        string
		start, end int64
		// line is the line the caller asked about, which is the one
		// whose byte offset the response reports. A range asks about
		// no single line and leaves it zero.
		line int64
	}
	//
	// Two positions with the same key (a line asked for twice, or N and
	// the negative position that resolves to N) ask for the same window,
	// so it is kept once; a second copy would read every line of the
	// window into the answer twice.
	windows := make([]window, 0, len(req.Lines))
	hasWindow := make(map[string]bool, len(req.Lines))
	addWindow := func(w window) {
		if !hasWindow[w.key] {
			hasWindow[w.key] = true
			windows = append(windows, w)
		}
	}
	for _, v := range req.Lines {
		if v.IsRange() {
			key := fmt.Sprintf("%d-%d", v.Start, *v.End)
			addWindow(window{key: key, start: v.Start, end: *v.End})
			resp.Samples[key] = []string{}
			resp.Lines[key] = -1
			continue
		}
		// A negative line counts back from the last one, and the key
		// reports the line it resolved to.
		line := v.Start
		if line == 0 {
			// Line 0 is not a line: they are numbered from 1. Answered
			// as unknown, the same way the plain-file path answers it.
			resp.Samples["0"] = nil
			resp.Lines["0"] = -1
			continue
		}
		if line < 0 {
			line = totalLines + line + 1
			if line < 1 {
				line = 1
			}
		}
		start := line - int64(req.BeforeContext)
		if start < 1 {
			start = 1
		}
		key := strconv.FormatInt(line, 10)
		addWindow(window{
			key: key, start: start, end: line + int64(req.AfterContext), line: line,
		})
		// Pre-populate so a window past the end of the file still
		// appears in the response, empty.
		// Pre-populate as unknown; the pass below fills in whatever the
		// file actually has. A line it never reaches keeps -1 and null,
		// which is what the plain-file path reports for a position past
		// the end.
		resp.Samples[key] = nil
		resp.Lines[key] = -1
	}

	if len(windows) == 0 {
		// Only line 0 was asked for, which needs no reading.
		return nil
	}
	firstLine, lastLine := windows[0].start, windows[0].end
	for _, w := range windows[1:] {
		firstLine = min(firstLine, w.start)
		lastLine = max(lastLine, w.end)
	}

	cursor, err := openStreamAtLine(req.Source, format, idx, firstLine)
	if err != nil {
		return err
	}
	defer func() { _ = cursor.close() }()

	// The byte offsets recorded here are positions in the decompressed
	// stream, which is the coordinate system a search of the same file
	// reports its matches in. Reporting -1 left the two surfaces
	// speaking different languages about the same line.
	r := bufio.NewReaderSize(cursor, 64*1024)
	lineNum, pos := cursor.line-1, cursor.offset
	for lineNum < lastLine {
		raw, readErr := r.ReadBytes('\n')
		if len(raw) > 0 {
			lineNum++
			text := trimNewline(string(raw))
			for _, w := range windows {
				if lineNum >= w.start && lineNum <= w.end {
					resp.Samples[w.key] = append(resp.Samples[w.key], text)
				}
				if lineNum == w.line {
					resp.Lines[w.key] = pos
				}
			}
			pos += int64(len(raw))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
	return nil
}

// openStreamAtLine returns the text of a compressed stream from the
// start of a line at or before line: the index checkpoint before it
// when idx has one, else the first byte.
func openStreamAtLine(src paths.Pinned, format compression.Format, idx *rxtypes.UnifiedFileIndex, line int64) (*textCursor, error) {
	text := streamedText{src: src, format: format}
	offset, startLine := chooseSeekOrigin(idx, line)
	cursor, err := text.openAt(offset)
	if err != nil {
		return nil, err
	}
	cursor.line = startLine
	return cursor, nil
}

// streamLineCount returns the number of lines in a compressed stream:
// the index's count when there is an index, else a count over the whole
// decompressed stream.
func streamLineCount(src paths.Pinned, format compression.Format, idx *rxtypes.UnifiedFileIndex) (int64, error) {
	if idx != nil && idx.LineCount != nil {
		return *idx.LineCount, nil
	}
	return streamCountLines(src, format)
}

// trimNewline drops the line terminator a reader keeps, so the text
// matches what the plain-file resolver returns for the same line.
func trimNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}

// streamCountLines decompresses the file and counts its lines without
// keeping any of the content.
//
// A line is counted at its line break, and a last line that no break
// ends is counted too: it is a line, and -1 has to name it as it does
// in the plain copy of the same text.
func streamCountLines(src paths.Pinned, format compression.Format) (int64, error) {
	cursor, err := streamedText{src: src, format: format}.openAt(0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = cursor.close() }()
	dec := cursor

	var n int64
	// endsAtLineStart is true while the text read so far is empty or
	// ends with a line break, so no line is open at the end of it.
	endsAtLineStart := true
	buf := make([]byte, 64*1024)
	for {
		read, readErr := dec.Read(buf)
		if read > 0 {
			chunk := buf[:read]
			n += int64(bytes.Count(chunk, []byte{'\n'}))
			endsAtLineStart = bytes.HasSuffix(chunk, []byte{'\n'})
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if !endsAtLineStart {
					n++
				}
				return n, nil
			}
			return 0, readErr
		}
	}
}
