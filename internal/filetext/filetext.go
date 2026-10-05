// Package filetext opens the text of a file, whatever format it is
// stored in: the file's own bytes for a plain file, the decompressed
// stream for a gzip, bzip2, xz or zstd file (a seekable zstd file read
// as the zstd stream it also is), and for a systemd journal the text
// package journal renders, entry by entry, as journalctl prints it.
// Every command that reads a file's text from its first byte opens it
// here, so each format is read one way everywhere.
//
// Open takes the file as an io.ReaderAt and its size, not as a stream:
// the journal reader needs random access and reads by position, and a
// stream decoder reads the file through an io.SectionReader of size
// bytes, so a file that grows while it is read is read as it was when
// its size was taken.
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
	// decoder is released by Close: the stream decoder or the journal
	// reader that reader is, or nil for a plain file, which holds
	// nothing to release.
	decoder io.Closer
	// journal is the reader of a journal's text, whose report
	// JournalReport gives; nil for every other format.
	journal *journal.TextReader
	// closed is set by the first Close.
	closed bool
}

// Open returns the text of the file r (size bytes) stored in format. It
// never closes r.
//
// An unknown format is an error. A stream whose header is damaged may be
// an error here (gzip reads its header at once) or from the first Read;
// damage further in is an error from Read, after the text before it. A
// journal whose header rx cannot read is an error wrapping
// journal.ErrUnreadable; a damaged entry further in is no error: the
// journal reader leaves it out and counts it (JournalReport).
//
// Go note: io.ReaderAt is the interface of anything that reads bytes at
// a given offset, such as *os.File. io.NewSectionReader turns the first
// size bytes of one into an io.Reader that keeps its own position, so
// the file's own read position is never moved and several readers can
// share one open file.
func Open(r io.ReaderAt, size int64, format compression.Format) (*Text, error) {
	size = max(size, 0)
	switch format {
	case compression.FormatNone:
		return &Text{reader: io.NewSectionReader(r, 0, size)}, nil
	case compression.FormatJournal:
		// The journal reader takes the file itself: it follows the
		// journal's offsets, reading by position, and never closes r.
		text, err := journal.NewTextReader(r, size, journal.Options{})
		if err != nil {
			return nil, err
		}
		return &Text{reader: text, decoder: text, journal: text}, nil
	}
	// compression.NewReader closes the source it is given when its own
	// reader is closed. io.NopCloser gives it a source whose Close does
	// nothing, so closing the decoder leaves the caller's file open.
	dec, err := compression.NewReader(io.NopCloser(io.NewSectionReader(r, 0, size)), format)
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
	if t.journal == nil {
		return journal.Report{}, false
	}
	return t.journal.Report(), true
}
