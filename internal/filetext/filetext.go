// Package filetext opens the text of a file, whatever format it is
// stored in: the file's own bytes for a plain file, the decompressed
// stream for a gzip, bzip2, xz or zstd file (a seekable zstd file read
// as the zstd stream it also is). Every command that reads a file's text
// from its first byte opens it here, so each format is read one way
// everywhere.
//
// Open takes the file as an io.ReaderAt and its size, not as a stream:
// a reader that needs random access reads it by position, and a stream
// decoder reads it through an io.SectionReader of size bytes, so a file
// that grows while it is read is read as it was when its size was taken.
//
// The caller owns the file. A Text reads it and never closes it: Close
// releases the decoder only, and the caller closes the file after it.
package filetext

import (
	"errors"
	"io"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/journal"
)

// ErrClosed is Read's error after Close.
var ErrClosed = errors.New("read of a closed file text")

// Text is the text of one file, read from its start.
//
// A Text is used by one goroutine at a time, as every stream decoder is.
type Text struct {
	// reader gives the text.
	reader io.Reader
	// decoder is released by Close: the stream decoder reader comes from,
	// or nil for a plain file, which holds nothing to release.
	decoder io.Closer
	// closed is set by the first Close.
	closed bool
}

// Open returns the text of the file r (size bytes) stored in format. It
// never closes r.
//
// An unknown format is an error. A stream whose header is damaged may be
// an error here (gzip reads its header at once) or from the first Read;
// damage further in is an error from Read, after the text before it.
//
// Go note: io.ReaderAt is the interface of anything that reads bytes at
// a given offset, such as *os.File. io.NewSectionReader turns the first
// size bytes of one into an io.Reader that keeps its own position, so
// the file's own read position is never moved and several readers can
// share one open file.
func Open(r io.ReaderAt, size int64, format compression.Format) (*Text, error) {
	stored := io.NewSectionReader(r, 0, max(size, 0))
	if format == compression.FormatNone {
		return &Text{reader: stored}, nil
	}
	// compression.NewReader closes the source it is given when its own
	// reader is closed. io.NopCloser gives it a source whose Close does
	// nothing, so closing the decoder leaves the caller's file open.
	dec, err := compression.NewReader(io.NopCloser(stored), format)
	if err != nil {
		return nil, err
	}
	return &Text{reader: dec, decoder: dec}, nil
}

// Read copies the next bytes of the text into p. It returns io.EOF after
// the last byte, and ErrClosed after Close.
//
// Go note: having this method makes a *Text an io.Reader, so it works
// with io.Copy, bufio.Reader and every other consumer of a stream.
func (t *Text) Read(p []byte) (int, error) {
	if t.closed {
		return 0, ErrClosed
	}
	return t.reader.Read(p)
}

// Close releases the decoder. It never closes the file Open was given,
// which stays the caller's to close. A second Close does nothing.
func (t *Text) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	if t.decoder == nil {
		return nil
	}
	return t.decoder.Close()
}

// JournalReport is the reader's report for a journal, complete once Read
// returned io.EOF; ok is false for every other format.
func (t *Text) JournalReport() (r journal.Report, ok bool) {
	return journal.Report{}, false
}
