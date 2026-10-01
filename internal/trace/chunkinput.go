package trace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
)

// chunkCopyBufferBytes is the buffer the chunk feeder reads the file
// with. 64 KB is a good tradeoff: larger wastes memory per concurrent
// worker, smaller makes more syscalls.
const chunkCopyBufferBytes = 64 * 1024

// chunkInput describes what one chunk worker hands to ripgrep: the
// lines just before the chunk (the lead-in), the chunk itself, and then
// the lines just after it (the tail, written by feedChunk).
//
// ripgrep sees only its own input, so without the lead-in and the tail
// a match on the first or last lines of a chunk would lose the part of
// its window that lies in the chunk beside it, and a trace would answer
// differently from the trace cache, which reads windows from the file.
// The lines around the chunk are context only: a match there belongs to
// the neighboring chunk, which reports it.
type chunkInput struct {
	task FileTask
	// leadIn holds the contextBefore lines that end where the chunk
	// starts (fewer at the start of the file).
	leadIn []byte
	// leadStart is where leadIn starts in the file.
	leadStart int64
	// leadLines is the number of line breaks in leadIn, which is how far
	// ripgrep's line numbers run ahead of the chunk's own.
	leadLines int
}

// fileOffset turns a position in ripgrep's input into one in the file.
func (in chunkInput) fileOffset(rgOffset int64) int64 {
	return in.leadStart + rgOffset
}

// chunkLine turns ripgrep's line number into the line's number counted
// from the chunk's first line: 1 for that line, 0 or below for a line
// of the lead-in, and past the chunk's last line for one of the tail.
func (in chunkInput) chunkLine(rgLine int) int {
	return rgLine - in.leadLines
}

// owns reports whether the line starting at offset is the chunk's own,
// the rule that keeps each match in exactly one chunk.
func (in chunkInput) owns(offset int64) bool {
	return offset >= in.task.Offset && offset < in.task.EndOffset()
}

// chunkFeed is what feedChunk measured of the chunk's own bytes, the
// lead-in and the tail left out.
type chunkFeed struct {
	newlines int64 // line breaks in the chunk
	copied   int64 // bytes of the chunk handed to ripgrep
}

// feedChunk writes the chunk's input to w: the lead-in, the chunk's own
// bytes, and then the file from the chunk's end up to and including its
// contextAfter-th line break, or to its end.
//
// A failed write means ripgrep stopped reading (it exited, or the scan
// was canceled) and comes back wrapped in errRipgrepStoppedReading; the
// counts are what was handed over until then.
func feedChunk(src io.ReaderAt, input chunkInput, contextAfter int, w io.Writer) (chunkFeed, error) {
	var fed chunkFeed
	if err := writeToRipgrep(w, input.leadIn); err != nil {
		return fed, err
	}

	buf := make([]byte, chunkCopyBufferBytes)
	section := io.NewSectionReader(src, input.task.Offset, input.task.Count)
	for {
		n, readErr := section.Read(buf)
		if n > 0 {
			fed.newlines += int64(bytes.Count(buf[:n], newlineBytes))
			fed.copied += int64(n)
			if err := writeToRipgrep(w, buf[:n]); err != nil {
				return fed, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fed, readErr
		}
	}

	if contextAfter <= 0 {
		return fed, nil
	}
	// The section reader stops at the end of the file by itself, so the
	// length only has to be large enough.
	tail := io.NewSectionReader(src, input.task.EndOffset(), math.MaxInt64-input.task.EndOffset())
	return fed, copyThroughLineBreaks(w, tail, contextAfter, buf)
}

// copyThroughLineBreaks copies r to w up to and including r's n-th line
// break, or to its end when it holds fewer. It reads at most one buffer
// past that line break.
func copyThroughLineBreaks(w io.Writer, r io.Reader, n int, buf []byte) error {
	for {
		read, readErr := r.Read(buf)
		data := buf[:read]
		for i, b := range data {
			if b != '\n' {
				continue
			}
			n--
			if n == 0 {
				return writeToRipgrep(w, data[:i+1])
			}
		}
		if err := writeToRipgrep(w, data); err != nil {
			return err
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// writeToRipgrep writes p to ripgrep's input. An error means ripgrep no
// longer reads it.
func writeToRipgrep(w io.Writer, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if _, err := w.Write(p); err != nil {
		return fmt.Errorf("%w: %w", errRipgrepStoppedReading, err)
	}
	return nil
}

// readLinesBefore returns the lines lines that end where offset starts,
// fewer when the file has fewer before it, and nothing when lines is 0
// or offset is 0. offset is the first byte of a line, so the byte before
// it is the line break that ends the last of them.
func readLinesBefore(r io.ReaderAt, offset int64, lines int) ([]byte, error) {
	start, err := startOfLinesBefore(r, offset, lines)
	if err != nil || start == offset {
		return nil, err
	}
	out := make([]byte, offset-start)
	if _, err := r.ReadAt(out, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out, nil
}

// startOfLinesBefore finds where the first of those lines starts: just
// after the (lines+1)-th line break counting back from offset-1, or byte
// 0 when there are not that many. It reads the file backward one buffer
// at a time, so a long line costs more reads, never more memory.
func startOfLinesBefore(r io.ReaderAt, offset int64, lines int) (int64, error) {
	if lines <= 0 || offset <= 0 {
		return offset, nil
	}
	buf := make([]byte, chunkCopyBufferBytes)
	breaks := 0
	for end := offset; end > 0; {
		start := max(0, end-int64(len(buf)))
		block := buf[:end-start]
		if _, err := r.ReadAt(block, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for i := len(block) - 1; i >= 0; i-- {
			if block[i] != '\n' {
				continue
			}
			breaks++
			if breaks == lines+1 {
				return start + int64(i) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}
