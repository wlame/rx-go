package samples

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrOffsetsOnCompressed is returned when byte offsets are asked of a
// compressed file. A byte offset in the compressed bytes does not name
// a position in the text, and the offsets a search reports are in the
// decompressed stream, which cannot be seeked to. Line numbers work.
var ErrOffsetsOnCompressed = errors.New(
	"byte offsets are not supported for compressed files; use lines instead")

// resolveCompressedLines answers a line-mode request by streaming the
// file through its decompressor once, keeping only the lines the
// request asks for.
//
// There is no index into a compressed stream to seek with, so the pass
// is sequential and the whole file goes past. A negative line number
// costs a second pass, since the last line cannot be known before the
// end is reached.
func resolveCompressedLines(
	req Request,
	format compression.Format,
	resp *rxtypes.SamplesResponse,
) error {
	if len(req.Offsets) > 0 {
		return ErrOffsetsOnCompressed
	}

	needTotalLines := false
	for _, v := range req.Lines {
		if !v.IsRange() && v.Start < 0 {
			needTotalLines = true
			break
		}
	}
	var totalLines int64
	if needTotalLines {
		n, err := streamCountLines(req.Path, format)
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
	windows := make([]window, 0, len(req.Lines))
	for _, v := range req.Lines {
		if v.IsRange() {
			key := fmt.Sprintf("%d-%d", v.Start, *v.End)
			windows = append(windows, window{key: key, start: v.Start, end: *v.End})
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
		windows = append(windows, window{
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

	f, err := os.Open(req.Path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec, err := compression.NewReader(f, format)
	if err != nil {
		return err
	}
	defer func() { _ = dec.Close() }()

	// The byte offsets recorded here are positions in the decompressed
	// stream, which is the coordinate system a search of the same file
	// reports its matches in. Reporting -1 left the two surfaces
	// speaking different languages about the same line.
	r := bufio.NewReaderSize(dec, 256*1024)
	var lineNum, pos int64
	for {
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
}

// trimNewline drops the line terminator a reader keeps, so the text
// matches what the plain-file resolver returns for the same line.
func trimNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}

// streamCountLines decompresses the file and counts its lines without
// keeping any of the content.
func streamCountLines(path string, format compression.Format) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	dec, err := compression.NewReader(f, format)
	if err != nil {
		return 0, err
	}
	defer func() { _ = dec.Close() }()

	var n int64
	buf := make([]byte, 64*1024)
	for {
		read, readErr := dec.Read(buf)
		if read > 0 {
			n += int64(bytes.Count(buf[:read], []byte{'\n'}))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return n, nil
			}
			return 0, readErr
		}
	}
}
