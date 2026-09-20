// Package filekind decides what a file is, for every command and every
// HTTP route alike: the format its bytes are stored in, and whether the
// text they hold is text rx can search, number and index.
//
// One rule, applied from the file's own bytes, never from its name:
//
//   - Format. The first bytes decide (compression.DetectFromReader): a
//     gzip, bzip2, xz or zstd signature names that format, whatever the
//     extension says, and a file with none is plain. A zstd file whose
//     seek table describes it (seekable.ReadSeekTable) is seekable zstd.
//     So a text file named .gz is plain text, and a gzip file named .log
//     is gzip.
//   - Text. The first TextProbeBytes of the file's text decide: the file
//     itself for a plain file, the decompressed stream for a compressed
//     one. A NUL byte there means the file is not text; UTF-16, which
//     writes every ASCII character with a NUL beside it, is refused by
//     this rule and named as UTF-16 when it starts with a byte-order
//     mark. A .tar.gz is not text by this rule:
//     every tar header is padded with NUL bytes.
//
// A file that is not text is refused by every command with the reason
// Kind.NotText gives: `rx trace` and `rx index` skip it and say why,
// `rx samples` and `rx compress` refuse it, and no line index is ever
// built for it.
package filekind

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
)

// TextProbeBytes is how much of a file's text decides whether it is
// text: the first 8 KiB, the amount rx-python's is_text_file reads.
const TextProbeBytes = 8192

// NotTextPrefix starts every reason Kind.NotText gives. A caller that
// only needs to know the file was refused as binary can match on it.
const NotTextPrefix = "not a text file"

// ErrNotText is wrapped by the error of a command that refuses a file
// whose Kind is not text, so a caller can tell the refusal from a
// failure to read the file (errors.Is).
var ErrNotText = errors.New(NotTextPrefix)

// textFlaw names what the probe found in a file's first bytes of text
// that makes it not text.
type textFlaw int

const (
	noFlaw textFlaw = iota
	nulByte
	utf16Text
)

// notTextReasons words each flaw for a plain file and for a compressed
// one, whose probe looked at the decompressed text. Every reason starts
// with NotTextPrefix.
var notTextReasons = map[textFlaw][2]string{
	nulByte: {
		NotTextPrefix + ": a NUL byte in its first 8 KiB",
		NotTextPrefix + ": a NUL byte in the first 8 KiB of its decompressed text",
	},
	utf16Text: {
		NotTextPrefix + ": UTF-16 text, which rx does not decode",
		NotTextPrefix + ": its decompressed text is UTF-16, which rx does not decode",
	},
}

// utf16ByteOrderMarks are the two byte-order marks UTF-16 text starts
// with, big-endian and little-endian.
var utf16ByteOrderMarks = [][]byte{{0xFE, 0xFF}, {0xFF, 0xFE}}

// Kind is what a file is: how its text is stored, and whether that text
// is text.
type Kind struct {
	// Format is how the file's bytes hold its text: FormatNone for a
	// plain file, FormatSeekableZstd for a zstd file whose seek table
	// describes it, otherwise the stream format its signature names.
	Format compression.Format
	// Table is a seekable zstd file's seek table, already checked
	// against the file. Nil for every other format.
	Table *seekable.SeekTable
	// NotText says why the file's text is not text, starting with
	// NotTextPrefix. Empty when it is text.
	NotText string
	// TableMismatch is set for a zstd file that ends with the footer of
	// a seek table that does not describe it (two seekable files joined
	// with `cat`, or a damaged table): the error says why, and the file
	// is read as plain zstd, which gives its whole text. Nil otherwise.
	TableMismatch error
}

// IsText reports whether the file's text is text rx can read.
func (k Kind) IsText() bool { return k.NotText == "" }

// IsCompressed reports whether the file's text is read through a
// decompressor.
func (k Kind) IsCompressed() bool { return k.Format != compression.FormatNone }

// IsSeekable reports whether the file is seekable zstd, whose frames can
// be decompressed one at a time.
func (k Kind) IsSeekable() bool { return k.Format == compression.FormatSeekableZstd }

// CompressionName is the compression format a response reports for the
// file: the stream format, so a seekable zstd file is "zstd" (it is a
// zstd stream to any other tool), and "" for a plain file.
func (k Kind) CompressionName() string {
	if k.IsSeekable() {
		return string(compression.FormatZstd)
	}
	return string(k.Format)
}

// Of decides the Kind of the open file r, size bytes long.
//
// It reads, at most: the first bytes of the file (the signature); a
// zstd file's seek table and one frame header per frame
// (seekable.ReadSeekTable, which bounds its own reads by the file's
// size); and TextProbeBytes of the file's text. A compressed file's
// decoder may read more of the file than the text it yields to produce
// those bytes: a bzip2 block (at most 900 kB of text), one zstd block
// (at most 128 KiB), the whole frames of a seekable file that hold the
// probe, or whatever gzip or xz need to fill it.
//
// A file whose text cannot be read past the signature (a damaged
// stream, a seekable file whose first frame is damaged) is not refused
// here: the probe keeps what it read, and the command that reads the
// file reports the damage, the way it does for damage further in.
//
// Go note: io.ReaderAt reads by position and keeps no cursor, so the
// read position of an *os.File passed as r is left where it was.
func Of(r io.ReaderAt, size int64) Kind {
	kind := FormatOf(r, size)
	kind.NotText = notTextReason(probeText(r, size, kind), kind.IsCompressed())
	return kind
}

// FormatOf decides only the format part of the Kind of the open file r,
// size bytes long: Format and Table, never NotText. It is for a reader
// that already knows the file is text and needs to know how to reach
// it. It reads the signature and, for zstd, the seek table.
func FormatOf(r io.ReaderAt, size int64) Kind {
	format, err := compression.DetectFromReader(io.NewSectionReader(r, 0, size))
	if err != nil || format != compression.FormatZstd {
		// A read error leaves the file plain: the reader that follows
		// meets the same error and reports it.
		return Kind{Format: format}
	}
	table, err := seekable.ReadSeekTable(r, size)
	if errors.Is(err, seekable.ErrSeekTableMismatch) {
		return Kind{Format: compression.FormatZstd, TableMismatch: err}
	}
	if err != nil {
		return Kind{Format: compression.FormatZstd}
	}
	return Kind{Format: compression.FormatSeekableZstd, Table: table}
}

// OfPinned opens src through its pin, decides its Kind (Of) and closes
// it. The error is the open's or the stat's: the file cannot be read
// (fs.ErrPermission for a file the process may not read), or the path
// no longer leads to the file that was checked (paths.ErrFileChanged).
func OfPinned(src paths.Pinned) (Kind, error) {
	f, err := src.Open()
	if err != nil {
		return Kind{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Kind{}, err
	}
	return Of(f, info.Size()), nil
}

// FormatOfFile is FormatOf for an open file, at the size it has now. It
// is for a reader that opened a file the search already classified and
// needs to know how to reach its text again. The error is the stat's.
func FormatOfFile(f *os.File) (Kind, error) {
	info, err := f.Stat()
	if err != nil {
		return Kind{}, err
	}
	return FormatOf(f, info.Size()), nil
}

// FormatOfPinned is FormatOfFile for a pinned file: it opens src
// through its pin, decides its format and closes it. The error is the
// open's or the stat's.
func FormatOfPinned(src paths.Pinned) (Kind, error) {
	f, err := src.Open()
	if err != nil {
		return Kind{}, err
	}
	defer func() { _ = f.Close() }()
	return FormatOfFile(f)
}

// probeText returns the first TextProbeBytes of the file's text, or as
// much of it as can be read: a damaged stream keeps what was read
// before the damage, and the command that reads the file reports it.
func probeText(r io.ReaderAt, size int64, kind Kind) []byte {
	head, _ := ReadTextHead(r, size, kind, TextProbeBytes)
	return head
}

// ReadTextHead returns the first limit bytes of the text of the open
// file r, size bytes long, whose Kind is kind: the file's bytes for a
// plain file, its decompressed stream for any other. A text shorter
// than limit comes back whole, with a nil error.
//
// A read that fails before limit bytes or the end of the text — an I/O
// error, or a compressed stream that is damaged or cut short — returns
// the bytes read before it together with the error, so a caller that
// must describe the whole head never mistakes a broken stream for a
// short text.
//
// It reads at most limit bytes of a plain file. A compressed file's
// decoder may read further into the file than the text it yields, to
// fill its own buffers (see Of for how far).
//
// Go note: r is read by position (io.ReaderAt), so the read position
// of an *os.File passed as r is left where it was.
func ReadTextHead(r io.ReaderAt, size int64, kind Kind, limit int) ([]byte, error) {
	var text io.Reader = io.NewSectionReader(r, 0, size)
	switch {
	case kind.IsSeekable() && kind.Table != nil:
		// A seekable file is read frame by frame where its seek table
		// places each frame, so damage comes back as
		// seekable.ErrDamagedFrame naming the frame, as every other
		// reader of a seekable file reports it.
		frames := seekable.NewTextReader(r, kind.Table)
		defer func() { _ = frames.Close() }()
		text = frames
	case kind.IsCompressed():
		dec, err := compression.NewReader(io.NopCloser(text), kind.Format)
		if err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
		defer func() { _ = dec.Close() }()
		text = dec
	case size < int64(limit):
		// A plain file holds no more text than its size, so the buffer
		// need not be larger.
		limit = int(max(size, 0))
	}
	buf := make([]byte, limit)
	n, err := readUpTo(text, buf)
	return buf[:n], err
}

// readUpTo fills buf from r and returns how many bytes it read. It
// stops at a full buffer or at the end of r, with a nil error, and at
// the first other error, which it returns.
//
// io.ReadFull cannot serve here: it reports the end of a short text as
// io.ErrUnexpectedEOF, the same error a decoder gives for a stream that
// was cut short, so the two could not be told apart.
func readUpTo(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// notTextReason is the reason the probed text is not text, or "" when
// it is. compressed picks the wording for a decompressed text.
func notTextReason(text []byte, compressed bool) string {
	flaw := flawIn(text)
	if flaw == noFlaw {
		return ""
	}
	wording := notTextReasons[flaw]
	if compressed {
		return wording[1]
	}
	return wording[0]
}

// flawIn finds what makes the first bytes of a text not text: a NUL
// byte anywhere. The byte-order mark only names the reason: text that
// starts with a UTF-16 mark and holds a NUL is UTF-16, which writes
// every ASCII character as the character and a NUL. A mark alone
// decides nothing, since a line of a text log can start with the bytes
// FF FE too.
func flawIn(text []byte) textFlaw {
	if bytes.IndexByte(text, 0) < 0 {
		return noFlaw
	}
	for _, mark := range utf16ByteOrderMarks {
		if bytes.HasPrefix(text, mark) {
			return utf16Text
		}
	}
	return nulByte
}

// Err is the refusal of a file that is not text: an error wrapping
// ErrNotText whose message is NotText. It is nil for a text file.
func (k Kind) Err() error {
	if k.IsText() {
		return nil
	}
	return &notTextError{reason: k.NotText}
}

// notTextError is the error Kind.Err returns. Its message is the reason,
// which already starts with ErrNotText's text, and errors.Is finds
// ErrNotText through Unwrap.
type notTextError struct{ reason string }

func (e *notTextError) Error() string { return e.reason }

// Unwrap lets errors.Is(err, ErrNotText) match.
func (e *notTextError) Unwrap() error { return ErrNotText }
