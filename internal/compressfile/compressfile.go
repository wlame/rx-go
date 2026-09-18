// Package compressfile turns one file into a seekable zstd file. It is
// the single code path behind `rx compress` and `POST /v1/compress`, so
// both answer the same way for the same input.
//
// The output always holds the input's text. A plain file is encoded as
// it is; a gzip, bzip2, xz or plain zstd file is decompressed on the fly
// and its text encoded, so a trace of the output finds the same matches,
// line numbers and offsets as a trace of the decompressed file. The
// decompressed text is streamed through the encoder and never held in
// memory whole.
//
// Three inputs are refused before anything is written (see Check): a
// compound archive such as .tar.gz, whose text is a tar stream rather
// than lines; a file that is already seekable zstd, unless the caller
// asks to re-encode it (for another frame size, say); and an output path
// that names the input file itself, which would be truncated before it
// is read.
//
// Path validation against the search roots and the "output already
// exists" rule stay with the callers: each reports them in its own
// words and with its own status.
package compressfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
)

// Errors Check returns. Callers match them with errors.Is to choose an
// exit code or an HTTP status, and add the hint that fits their surface
// (a CLI flag or a request field).
var (
	// ErrCompoundArchive refuses a .tar.gz and its kin: decompressing
	// one yields a tar stream, not lines of text.
	ErrCompoundArchive = errors.New("compound archives (tar.gz, etc.) are not supported")
	// ErrAlreadySeekable refuses a seekable zstd input the caller did
	// not ask to re-encode: rx already reads it as it is.
	ErrAlreadySeekable = errors.New("already a seekable zstd file")
	// ErrOutputIsInput refuses an output path that resolves to the input
	// file. Creating the output would truncate the input before it is
	// read.
	ErrOutputIsInput = errors.New("the output path is the input file")
)

// Options describes one compression.
type Options struct {
	// InputPath and OutputPath are the paths the caller has already
	// validated against the search roots.
	InputPath  string
	OutputPath string
	// FrameSize, Level and Workers tune the encoder; zero means the
	// encoder's default (seekable.EncoderConfig).
	FrameSize int
	Level     int
	Workers   int
	// ReencodeSeekable lets a seekable zstd input through: its text is
	// decompressed and encoded again with this call's frame size and
	// level. `rx compress --force` and a request with "force": true set
	// it.
	ReencodeSeekable bool
}

// Result describes the file Compress wrote.
type Result struct {
	// InputFormat is the format the input was read as;
	// compression.FormatNone for a plain file.
	InputFormat compression.Format
	// CompressedSize is the size of the output file in bytes.
	CompressedSize int64
	// DecompressedSize is the size of the text the output holds: the
	// input's size for a plain file, its decompressed size otherwise.
	DecompressedSize int64
	// FrameCount is the number of zstd frames in the output.
	FrameCount int
}

// Ratio is DecompressedSize / CompressedSize truncated to two decimals,
// the `compression_ratio` both rx compress and the compress task report
// (a value >= 1 when the data shrank). It is 0 for an empty output.
func (r Result) Ratio() float64 {
	if r.CompressedSize <= 0 {
		return 0
	}
	ratio := float64(r.DecompressedSize) / float64(r.CompressedSize)
	return float64(int(ratio*100)) / 100
}

// outputSuffix ends the name of every file rx compress writes.
const outputSuffix = ".zst"

// DefaultOutputName is the file name rx compress and POST /v1/compress
// give the output when the caller names none; the caller puts it beside
// the input or in the requested directory.
//
// The output holds the input's text, so a compression suffix the input
// name ends with (.gz, .gzip, .bz2, .bzip2, .xz, .zst, .zstd, in any
// case) is replaced by ".zst": app.log.gz becomes app.log.zst. Any
// other name gets ".zst" appended: app.log becomes app.log.zst. A plain
// zstd input named app.log.zst therefore names itself; Check refuses
// that pair, so the input is never overwritten.
//
// The suffix alone decides, not the file's bytes: a name says nothing
// about which part of it is a format, so app.log holding gzip bytes
// keeps its ".log".
func DefaultOutputName(inputPath string) string {
	name := filepath.Base(inputPath)
	if compression.FormatFromExtension(name) != compression.FormatNone {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	return name + outputSuffix
}

// Check reports why inputPath cannot be compressed to outputPath, or nil
// when it can. It reads at most the input's seek table and one frame
// header per frame (whether it is seekable already) and writes nothing,
// so a caller can refuse a request before it starts any work.
func Check(inputPath, outputPath string, reencodeSeekable bool) error {
	if compression.IsCompoundArchive(inputPath) {
		return ErrCompoundArchive
	}
	if seekable.IsSeekable(inputPath) && !reencodeSeekable {
		return ErrAlreadySeekable
	}
	if isSameFile(inputPath, outputPath) {
		return ErrOutputIsInput
	}
	return nil
}

// Compress writes the seekable zstd form of opts.InputPath's text to
// opts.OutputPath, creating or truncating it, and fsyncs it.
//
// It runs Check first and returns its error unchanged. Any later error
// is wrapped with the step that failed; a decompression error (a
// truncated or corrupt input) keeps its cause, so errors.Is finds it.
// On a failure after the output was created, the partial output is
// removed: it has no seek table and is not a usable file.
//
// ctx cancels the encoding between frame batches.
func Compress(ctx context.Context, opts Options) (Result, error) {
	if err := Check(opts.InputPath, opts.OutputPath, opts.ReencodeSeekable); err != nil {
		return Result{}, err
	}
	text, format, err := openText(opts.InputPath)
	if err != nil {
		return Result{}, err
	}
	// Closing the text reader also closes the input file it reads from.
	defer func() { _ = text.Close() }()

	dst, err := os.Create(opts.OutputPath)
	if err != nil {
		return Result{}, fmt.Errorf("create output: %w", err)
	}
	tbl, err := encodeInto(ctx, text, dst, opts)
	// Close before the size is read and before a failed output is
	// removed; a Close error after a good fsync loses no data.
	_ = dst.Close()
	if err != nil {
		_ = os.Remove(opts.OutputPath)
		return Result{}, err
	}

	info, err := os.Stat(opts.OutputPath)
	if err != nil {
		return Result{}, fmt.Errorf("stat output: %w", err)
	}
	return Result{
		InputFormat:      format,
		CompressedSize:   info.Size(),
		DecompressedSize: decompressedSize(tbl),
		FrameCount:       len(tbl.Frames),
	}, nil
}

// encodeInto streams text through the seekable encoder into dst and
// fsyncs dst.
func encodeInto(ctx context.Context, text io.Reader, dst *os.File, opts Options) (*seekable.SeekTable, error) {
	enc := seekable.NewEncoder(seekable.EncoderConfig{
		FrameSize: opts.FrameSize,
		Level:     opts.Level,
		Workers:   opts.Workers,
	})
	tbl, err := enc.EncodeStream(ctx, text, dst)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	if err := dst.Sync(); err != nil {
		return nil, fmt.Errorf("fsync output: %w", err)
	}
	return tbl, nil
}

// openText opens path as a stream of its text and reports the format it
// was read as. A plain file is read up to the size it had when it was
// opened, so a log that grows during the encoding is encoded as it was
// at the start, the way a trace plans its chunks.
//
// The path is pinned first (paths.Pin), which checks it against the
// search roots and the hidden rule again: an HTTP compression runs as a
// background task, well after its request's check, and a link
// retargeted in between must not be copied into the root.
func openText(path string) (io.ReadCloser, compression.Format, error) {
	pinned, err := paths.Pin(path)
	if err != nil {
		return nil, compression.FormatNone, err
	}
	src, err := pinned.Open()
	if err != nil {
		return nil, compression.FormatNone, fmt.Errorf("open input: %w", err)
	}
	info, err := src.Stat()
	if err != nil {
		_ = src.Close()
		return nil, compression.FormatNone, fmt.Errorf("stat input: %w", err)
	}
	format, err := inputFormat(path, src, info.Size())
	if err != nil {
		_ = src.Close()
		return nil, compression.FormatNone, fmt.Errorf("detect input format: %w", err)
	}
	if format == compression.FormatNone {
		return readCloser{Reader: io.NewSectionReader(src, 0, info.Size()), Closer: src}, format, nil
	}
	// compression.NewReader owns src from here on: closing the reader it
	// returns closes src too. On its error, src is still ours to close.
	text, err := compression.NewReader(src, format)
	if err != nil {
		_ = src.Close()
		return nil, compression.FormatNone, fmt.Errorf("open %s input: %w", format, err)
	}
	return text, format, nil
}

// inputFormat names the format of the open file r, size bytes long and
// named path: seekable zstd when it ends with a seek table that
// describes it, otherwise what compression.DetectFromOpenFile finds
// from the name or the magic bytes. Nothing is looked up by path.
func inputFormat(path string, r io.ReaderAt, size int64) (compression.Format, error) {
	if seekable.IsSeekableFile(path, r, size) {
		return compression.FormatSeekableZstd, nil
	}
	return compression.DetectFromOpenFile(path, r)
}

// readCloser joins a reader with the file it reads from.
type readCloser struct {
	io.Reader
	io.Closer
}

// isSameFile reports whether output names the input file: the same path
// after cleaning, or, when both exist, the same file reached another way
// (a symbolic or hard link).
func isSameFile(input, output string) bool {
	if filepath.Clean(input) == filepath.Clean(output) {
		return true
	}
	inInfo, err := os.Stat(input)
	if err != nil {
		return false
	}
	outInfo, err := os.Stat(output)
	if err != nil {
		return false
	}
	return os.SameFile(inInfo, outInfo)
}

// decompressedSize is the length of the text the frames of tbl hold.
func decompressedSize(tbl *seekable.SeekTable) int64 {
	if len(tbl.Frames) == 0 {
		return 0
	}
	return tbl.Frames[len(tbl.Frames)-1].DecompressedEnd()
}
