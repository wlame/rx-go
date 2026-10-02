package samples

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// textSource is the text of one file as the byte-offset pass reads it.
//
// A byte offset is a position in a file's text. For a plain file the
// text is the file's bytes; for a compressed file it is the
// decompressed stream, which is the coordinate a search of that file
// reports its matches in. The pass that turns offsets into lines is the
// same for every file; only the way it reaches the text differs, and
// that is what this interface hides.
//
// Go note: an interface is a set of methods. Any type that has them
// satisfies the interface without declaring it, so plainText,
// streamedText and seekableText below are all textSources.
type textSource interface {
	// size returns the length of the text in bytes. Only an offset
	// counted back from the end needs it, so it is asked for lazily:
	// for a compressed file without an index it costs a full
	// decompression.
	size() (int64, error)

	// openNear returns the text from the start of a line at or before
	// offset. Where the source can choose its starting line, it leaves
	// room for at least `before` lines ahead of the line holding offset,
	// so the leading context of that line is inside the pass.
	openNear(offset int64, before int) (*textCursor, error)
}

// textCursor is a file's text from the start of a known line onwards.
//
// Go note: embedding io.Reader makes a *textCursor an io.Reader itself;
// its Read is the embedded reader's Read.
type textCursor struct {
	io.Reader
	offset int64        // position of the reader's first byte in the text
	line   int64        // number of the line that starts at offset
	close  func() error // releases the file and any decoder
}

// textSourceFor picks how the offsets pass reads req.Path.
//
// An index the loader cannot provide is treated as absent: an index
// only makes the answer faster, so a missing or unreadable one costs
// time, never correctness.
func textSourceFor(req Request) textSource {
	var idx *rxtypes.UnifiedFileIndex
	if req.IndexLoader != nil {
		idx, _ = req.IndexLoader(req.Path)
	}
	format, _ := compression.DetectFromPath(req.Path)
	if format == compression.FormatNone {
		return plainText{src: req.Source, idx: idx}
	}
	if isSeekable(req.Source) {
		if text, err := seekableTextFor(req.Source, idx); err == nil {
			return text
		}
		// The checkpoints of a seekable file's index sit at frame
		// starts, which are not always line starts, so a stream must
		// not start from one: it reads from the first byte instead.
		return streamedText{src: req.Source, format: compression.FormatSeekableZstd}
	}
	return streamedText{src: req.Source, format: format, idx: idx}
}

// ============================================================================
// Plain file
// ============================================================================

// plainText reads a plain file, seeking to the index checkpoint before
// the first offset when an index exists.
type plainText struct {
	src paths.Pinned
	idx *rxtypes.UnifiedFileIndex
}

func (p plainText) size() (int64, error) {
	info, err := p.src.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (p plainText) openNear(offset int64, before int) (*textCursor, error) {
	startOffset, startLine := int64(0), int64(1)
	if p.idx != nil {
		startOffset, startLine = checkpointBefore(p.idx, offset, before)
	}
	f, err := openFileForSamples(p.src)
	if err != nil {
		return nil, err
	}
	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return &textCursor{Reader: f, offset: startOffset, line: startLine, close: f.Close}, nil
}

// ============================================================================
// Compressed stream: gzip, bzip2, xz, zstd
// ============================================================================

// streamedText reads a compressed file that cannot be entered in the
// middle. Its text is always decompressed from the first byte; an index
// still helps, because its checkpoints are line starts in the text: the
// bytes before the checkpoint are decompressed and dropped without
// being split into lines, and the line count starts at the checkpoint.
//
// The pass that reads the cursor stops once every window is complete,
// so an offset near the start of a large file decompresses only the
// start of it.
type streamedText struct {
	src    paths.Pinned
	format compression.Format
	idx    *rxtypes.UnifiedFileIndex
}

func (s streamedText) size() (int64, error) {
	if s.idx != nil && s.idx.DecompressedSizeBytes != nil {
		return *s.idx.DecompressedSizeBytes, nil
	}
	cursor, err := s.openAt(0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = cursor.close() }()
	return io.Copy(io.Discard, cursor)
}

func (s streamedText) openNear(offset int64, before int) (*textCursor, error) {
	if s.idx == nil {
		return s.openAt(0)
	}
	startOffset, startLine := checkpointBefore(s.idx, offset, before)
	cursor, err := s.openAt(startOffset)
	if err != nil {
		return nil, err
	}
	cursor.line = startLine
	return cursor, nil
}

// openAt returns the text from byte offset of the text, which the caller
// knows to be the start of a line; the returned cursor's line is 1 and
// the caller sets it when offset is not 0.
func (s streamedText) openAt(offset int64) (*textCursor, error) {
	f, err := openFileForSamples(s.src)
	if err != nil {
		return nil, err
	}
	// The decompressor owns f from here: closing it closes f too.
	dec, err := decompressorFor(f, s.format)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, dec, offset); err != nil {
			_ = dec.Close()
			return nil, fmt.Errorf("decompress %s up to byte %d: %w", s.src.Path(), offset, err)
		}
	}
	return &textCursor{Reader: dec, offset: offset, line: 1, close: dec.Close}, nil
}

// openedSeekable is what a seekable file opened for samples offers
// beyond readSeekCloser: reads by position and its size. An *os.File
// has both.
type openedSeekable interface {
	io.ReaderAt
	Stat() (os.FileInfo, error)
}

// decompressorFor returns the text of f, a file in format; closing the
// result closes f. A seekable zstd file is read frame by frame through
// its seek table (seekable.TextReader), so a damaged frame comes back as
// seekable.ErrDamagedFrame naming the frame, as trace and index report
// it, rather than as a zstd stream error. A file without reads by
// position (a test's counting wrapper) is read as a zstd stream, which
// gives the same text.
func decompressorFor(f readSeekCloser, format compression.Format) (io.ReadCloser, error) {
	if format == compression.FormatSeekableZstd {
		if file, ok := f.(openedSeekable); ok {
			info, err := file.Stat()
			if err != nil {
				return nil, err
			}
			table, err := seekable.ReadSeekTable(file, info.Size())
			if err != nil {
				return nil, err
			}
			text := seekable.NewTextReader(file, table)
			return readCloser{Reader: text, close: func() error {
				_ = text.Close()
				return f.Close()
			}}, nil
		}
	}
	return compression.NewReader(f, format)
}

// readCloser joins a reader with what closing it takes.
type readCloser struct {
	io.Reader
	close func() error
}

// Close runs the close function.
func (r readCloser) Close() error { return r.close() }

// ============================================================================
// Seekable zstd with a frame table
// ============================================================================

// seekableText reads a seekable-zstd file from a frame near the offset,
// using the frame table of its index to know which line that frame
// starts in. Only the frames the pass reaches are decompressed, one at
// a time, so one offset costs a frame or two whatever the file's size.
type seekableText struct {
	src     paths.Pinned
	frames  []rxtypes.FrameLineInfo
	table   *seekable.SeekTable
	decoder *seekable.Decoder
}

// seekableTextFor returns a seekableText for path when idx carries a
// frame table that describes the file, and errNoFrameIndex otherwise.
//
// The table is trusted only when it agrees with the file's seek table
// frame by frame, and when its line numbers add up: the last frame's
// last line must be the file's line count. A table that numbered a
// frame without a line break as holding a line fails that check, and
// every line after such a frame would otherwise be numbered one too
// high.
func seekableTextFor(src paths.Pinned, idx *rxtypes.UnifiedFileIndex) (*seekableText, error) {
	if idx == nil || idx.Frames == nil || len(*idx.Frames) == 0 || idx.LineCount == nil {
		return nil, errNoFrameIndex
	}
	frames := *idx.Frames
	if frames[len(frames)-1].LastLine != *idx.LineCount {
		return nil, errNoFrameIndex
	}

	file, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	table, err := seekable.ReadSeekTable(file, info.Size())
	if err != nil || len(table.Frames) != len(frames) {
		return nil, errNoFrameIndex
	}
	for i, frame := range table.Frames {
		if frame.DecompressedOffset != frames[i].DecompressedOffset {
			return nil, errNoFrameIndex
		}
	}
	return &seekableText{src: src, frames: frames, table: table, decoder: seekable.NewDecoder()}, nil
}

func (s *seekableText) size() (int64, error) {
	last := s.table.Frames[len(s.table.Frames)-1]
	return last.DecompressedEnd(), nil
}

// openNear starts the text at the first whole line of a frame before the
// one holding offset.
//
// A frame's first bytes can be the tail of a line that began in an
// earlier frame, so a frame start is not known to be a line start. The
// byte after a frame's first line break is: it starts line
// FirstLine+1 of that frame. The frame chosen is the latest one before
// offset's frame that holds a line break and still leaves `before`
// lines of context ahead of it; frame 0 starts at line 1 and needs no
// skipping.
func (s *seekableText) openNear(offset int64, before int) (*textCursor, error) {
	holding := sort.Search(len(s.table.Frames), func(i int) bool {
		return s.table.Frames[i].DecompressedEnd() > offset
	})
	if holding == len(s.table.Frames) {
		// Past the end of the text: the pass reads the last frame to
		// the end and leaves the offset unanswered.
		holding = len(s.table.Frames) - 1
	}
	if holding == 0 {
		return s.cursorFrom(0, nil, 0, 1), nil
	}

	start := holding - 1
	for start > 0 && (endsNoLine(s.frames[start]) ||
		s.frames[holding].FirstLine-s.frames[start].FirstLine-1 < int64(before)) {
		start--
	}
	if start == 0 {
		return s.cursorFrom(0, nil, 0, 1), nil
	}

	data, err := decodeSeekableFrame(s.decoder, s.src, start, s.table)
	if err != nil {
		return nil, err
	}
	lineBreak := bytes.IndexByte(data, '\n')
	if lineBreak < 0 {
		// The table said this frame ends a line, so it is a frame table
		// that does not describe the file.
		return nil, fmt.Errorf("frame %d of %s holds no line break", start, s.src.Path())
	}
	rest := data[lineBreak+1:]
	offsetAfter := s.frames[start].DecompressedOffset + int64(lineBreak) + 1
	return s.cursorFrom(start+1, rest, offsetAfter, s.frames[start].FirstLine+1), nil
}

// endsNoLine reports whether a frame holds no line break: it lies inside
// a line longer than a frame, or is empty. Such a frame's table entry
// has LastLine = FirstLine-1. Only the last frame can end a line without
// a break, and openNear never starts from the last frame.
func endsNoLine(frame rxtypes.FrameLineInfo) bool {
	return frame.LastLine < frame.FirstLine
}

// cursorFrom returns a cursor that reads pending first and then every
// frame from nextFrame on.
func (s *seekableText) cursorFrom(nextFrame int, pending []byte, offset, line int64) *textCursor {
	reader := &frameReader{text: s, pending: pending, next: nextFrame}
	return &textCursor{Reader: reader, offset: offset, line: line, close: func() error { return nil }}
}

// frameReader is an io.Reader over a seekable file's text that
// decompresses one frame at a time, only when the reader reaches it.
// Each frame is read from the file on its own (the decoder opens the
// file per frame), so there is nothing to close.
type frameReader struct {
	text    *seekableText
	pending []byte // decompressed bytes not yet handed out
	next    int    // the frame to decompress when pending runs out
}

// Read hands out the pending bytes, decompressing the next frame when
// none are left, and returns io.EOF after the last frame.
func (r *frameReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.next >= len(r.text.table.Frames) {
			return 0, io.EOF
		}
		data, err := decodeSeekableFrame(r.text.decoder, r.text.src, r.next, r.text.table)
		if err != nil {
			return 0, err
		}
		r.pending = data
		r.next++
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
