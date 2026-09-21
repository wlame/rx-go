package samples

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

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

// streamCountLines decompresses the file and counts its lines without
// keeping any of the content.
//
// A line is counted at its line break, and a last line that no break
// ends is counted too: it is a line, and -1 has to name it as it does
// in the plain copy of the same text.
func streamCountLines(ctx context.Context, src paths.Pinned, format compression.Format) (int64, error) {
	cursor, err := streamedText{src: src, format: format}.openAt(0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = cursor.close() }()
	dec := withContext(ctx, cursor)

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
