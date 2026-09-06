package samples

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ErrOffsetsOnCompressed is returned when byte offsets are asked of a
// compressed file. A byte offset in the compressed bytes does not name
// a position in the text, and the offsets a search reports are in the
// decompressed stream, which cannot be seeked to. Line numbers work.
var ErrOffsetsOnCompressed = errors.New(
	"byte offsets are not supported for compressed files; use lines instead")

// maxCompressedLineBytes bounds one line read out of a decompressed
// stream. Same 64 MB ceiling the HTTP path used before this moved.
const maxCompressedLineBytes = 64 * 1024 * 1024

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
		windows = append(windows, window{key: key, start: start, end: line + int64(req.AfterContext)})
		// Pre-populate so a window past the end of the file still
		// appears in the response, empty.
		resp.Samples[key] = []string{}
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

	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 64*1024), maxCompressedLineBytes)
	var lineNum int64
	for sc.Scan() {
		lineNum++
		for _, w := range windows {
			if lineNum >= w.start && lineNum <= w.end {
				resp.Samples[w.key] = append(resp.Samples[w.key], sc.Text())
			}
		}
	}
	return sc.Err()
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
