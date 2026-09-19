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
// that names the input file itself.
//
// The output is written to a temporary file in its directory and put
// under its name only once it is complete and synced to disk (see
// Compress). The name therefore holds either what it held before or a
// whole output, never a part of one, and two compressions to one name
// cannot mix their bytes.
//
// Path validation against the search roots and the early "output
// already exists" refusal stay with the callers: each reports them in
// its own words and with its own status.
package compressfile

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
)

// Errors Check returns. Callers match them with errors.Is to choose an
// exit code or an HTTP status, and add the hint that fits their surface
// (a CLI flag or a request field).
var (
	// ErrNotText refuses an input whose text is not text (filekind): a
	// binary file, a .tar.gz (its text is a tar stream), UTF-16. The
	// error Check returns wraps it and says why.
	ErrNotText = filekind.ErrNotText
	// ErrAlreadySeekable refuses a seekable zstd input the caller did
	// not ask to re-encode: rx already reads it as it is.
	ErrAlreadySeekable = errors.New("already a seekable zstd file")
	// ErrOutputIsInput refuses an output path that resolves to the input
	// file: the input would be replaced by its own compressed form.
	ErrOutputIsInput = errors.New("the output path is the input file")
	// ErrOutputExists refuses to replace an output file the caller did
	// not ask to overwrite.
	ErrOutputExists = errors.New("output file already exists")
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
	// Overwrite replaces a file that already holds the output name.
	// Without it, Compress returns ErrOutputExists when anything holds
	// that name by the time the finished output is put there, a
	// symbolic link included, even one that leads nowhere.
	Overwrite bool
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
// when it can. It opens the input through its pin (paths.Pin), reads at
// most its seek table and one frame header per frame (whether it is
// seekable already) and writes nothing, so a caller can refuse a request
// before it starts any work. An input that cannot be pinned or opened
// returns that error.
func Check(inputPath, outputPath string, reencodeSeekable bool) error {
	src, err := openPinned(inputPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	_, err = checkOpened(inputPath, src, outputPath, reencodeSeekable)
	return err
}

// checkOpened applies the rules of Check that look at the input to src,
// the input already opened through its pin, and returns what the input
// is (filekind.Of, read through src) for the encoding to read its text.
func checkOpened(inputPath string, src *os.File, outputPath string, reencodeSeekable bool) (filekind.Kind, error) {
	info, err := src.Stat()
	if err != nil {
		return filekind.Kind{}, fmt.Errorf("stat input: %w", err)
	}
	kind := filekind.Of(src, info.Size())
	if err := kind.Err(); err != nil {
		return kind, err
	}
	if kind.IsSeekable() && !reencodeSeekable {
		return kind, ErrAlreadySeekable
	}
	if isSameFile(inputPath, info, outputPath) {
		return kind, ErrOutputIsInput
	}
	return kind, nil
}

// Compress writes the seekable zstd form of opts.InputPath's text to
// opts.OutputPath.
//
// It applies Check's rules first and returns their error unchanged. The
// output is encoded into a new temporary file in the output's directory
// (createTemp), synced to disk, and only then put under its name
// (publishOutput): with opts.Overwrite it replaces whatever held the
// name, a symbolic link included, which is replaced rather than written
// through; without it the name must still be free at that moment, or
// the result is an error wrapping ErrOutputExists. So the name holds
// either what it held before or a whole output, and of two compressions
// to one name each writes its own file: they never mix.
//
// Any later error is wrapped with the step that failed; a decompression
// error (a truncated or corrupt input) keeps its cause, so errors.Is
// finds it. On every failure the temporary file is removed and the
// output name is left as it was. A process killed outright (SIGKILL)
// leaves its temporary file, a hidden name starting with ".rx-compress-",
// and nothing under the output name.
//
// ctx cancels the encoding between frame batches.
func Compress(ctx context.Context, opts Options) (Result, error) {
	src, err := openPinned(opts.InputPath)
	if err != nil {
		return Result{}, err
	}
	kind, refusal := checkOpened(opts.InputPath, src, opts.OutputPath, opts.ReencodeSeekable)
	if refusal != nil {
		_ = src.Close()
		return Result{}, refusal
	}
	text, format, err := openText(src, kind)
	if err != nil {
		return Result{}, err
	}
	// Closing the text reader also closes the input file it reads from.
	defer func() { _ = text.Close() }()

	// SECURITY: every later step on the output (create the temporary
	// file, link or rename it, remove it) goes through dir, the output's
	// directory opened from the search root without passing a link. A
	// directory swapped for a link after the caller's check is refused
	// here, and nothing can redirect the writes once dir is open.
	dir, err := paths.OpenDir(filepath.Dir(opts.OutputPath))
	if err != nil {
		return Result{}, fmt.Errorf("open output directory: %w", err)
	}
	defer func() { _ = dir.Close() }()

	tbl, size, err := writeOutput(ctx, dir, filepath.Base(opts.OutputPath), text, opts)
	if errors.Is(err, ErrOutputExists) {
		return Result{}, fmt.Errorf("%w: %s", ErrOutputExists, opts.OutputPath)
	}
	if err != nil {
		return Result{}, err
	}
	return Result{
		InputFormat:      format,
		CompressedSize:   size,
		DecompressedSize: decompressedSize(tbl),
		FrameCount:       len(tbl.Frames),
	}, nil
}

// Temporary output files are named tempPrefix, a random part and
// tempSuffix. The leading dot makes them hidden, so a directory search
// skips them by default while they are written.
const (
	tempPrefix = ".rx-compress-"
	tempSuffix = ".tmp"
	// tempAttempts bounds the random names tried before giving up. A
	// random part is 26 base32 characters (crypto/rand.Text), so a
	// second attempt is already unlikely ever to be needed.
	tempAttempts = 8
)

// writeOutput encodes text into a new temporary file in dir, syncs it,
// and puts it under name. It returns the seek table and the size of the
// file it wrote, taken from the open file (fstat), so the size describes
// this file even when another writer replaces name right after.
func writeOutput(ctx context.Context, dir *os.Root, name string, text io.Reader, opts Options) (*seekable.SeekTable, int64, error) {
	tmp, tmpName, err := createTemp(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("create output: %w", err)
	}
	// The deferred call runs on every way out of this function. After a
	// rename the temporary name no longer exists and Remove finds
	// nothing; after a link it is a second name of the finished output.
	// In every other case it is an unfinished file. Its error is
	// dropped: on a failure the error reported is the one that stopped
	// the compression, and on success the output is whole wherever the
	// leftover name is.
	defer func() { _ = dir.Remove(tmpName) }()

	tbl, err := encodeInto(ctx, text, tmp, opts)
	if err != nil {
		_ = tmp.Close()
		return nil, 0, err
	}
	info, statErr := tmp.Stat()
	closeErr := tmp.Close()
	if statErr != nil {
		return nil, 0, fmt.Errorf("stat output: %w", statErr)
	}
	if closeErr != nil {
		return nil, 0, fmt.Errorf("close output: %w", closeErr)
	}
	if err := publishOutput(dir, tmpName, name, opts.Overwrite); err != nil {
		return nil, 0, err
	}
	return tbl, info.Size(), nil
}

// createTemp creates a new, empty temporary file in dir and returns it
// open for writing, with its name. O_EXCL makes the create fail rather
// than open anything that already holds the name, a symbolic link
// included. The mode is what os.Create gives (0666 less the umask), the
// mode the output had when it was created in place.
func createTemp(dir *os.Root) (*os.File, string, error) {
	for range tempAttempts {
		name := tempPrefix + rand.Text() + tempSuffix
		f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, name, nil
	}
	return nil, "", fmt.Errorf("no free temporary name after %d attempts", tempAttempts)
}

// publishOutput puts the finished temporary file tmpName under name,
// both in dir.
//
// With overwrite it renames: rename replaces the directory entry name
// in one step, whatever it is, and never follows a symbolic link there.
// Without overwrite it makes name a hard link to the file, which fails
// with "file exists" when anything holds name, so of two writers racing
// for a free name exactly one wins. The caller removes tmpName.
func publishOutput(dir *os.Root, tmpName, name string, overwrite bool) error {
	if overwrite {
		if err := dir.Rename(tmpName, name); err != nil {
			return fmt.Errorf("rename output into place: %w", err)
		}
		return nil
	}
	err := linkOutput(dir, tmpName, name)
	if errors.Is(err, fs.ErrExist) {
		return ErrOutputExists
	}
	if err != nil {
		// Some file systems have no hard links (FAT, exFAT, some network
		// shares). Claim the name another way.
		return claimAndRename(dir, tmpName, name)
	}
	return nil
}

// linkOutput puts the finished output under its name when the caller
// did not ask to overwrite. Tests replace it to act as a file system
// without hard links.
var linkOutput = func(dir *os.Root, oldname, newname string) error {
	return dir.Link(oldname, newname)
}

// claimAndRename is publishOutput without hard links: it creates name
// empty with O_EXCL, which fails when anything holds it, and then
// renames the finished file over that claim. Between the two steps,
// microseconds apart, name is an empty file; a process killed exactly
// then leaves it.
func claimAndRename(dir *os.Root, tmpName, name string) error {
	claim, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if errors.Is(err, fs.ErrExist) {
		return ErrOutputExists
	}
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	claimed, statErr := claim.Stat()
	_ = claim.Close()
	if err := dir.Rename(tmpName, name); err != nil {
		// Remove the empty claim, but only while name is still it.
		if current, lstatErr := dir.Lstat(name); statErr == nil && lstatErr == nil && os.SameFile(claimed, current) {
			_ = dir.Remove(name)
		}
		return fmt.Errorf("rename output into place: %w", err)
	}
	return nil
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

// openPinned opens path for reading through its pin (paths.Pin), which
// checks it against the search roots and the hidden rule again: an HTTP
// compression runs as a background task, well after its request's
// check, and a link retargeted in between must not be copied into the
// root. The caller closes the file.
func openPinned(path string) (*os.File, error) {
	pinned, err := paths.Pin(path)
	if err != nil {
		return nil, err
	}
	src, err := pinned.Open()
	if err != nil {
		return nil, fmt.Errorf("open input: %w", err)
	}
	return src, nil
}

// openText turns src, the input opened through its pin, into a stream
// of its text and reports the format it was read as: kind's, which
// checkOpened decided from src. A plain file is read up to the size it
// had when it was opened, so a log that grows during the encoding is
// encoded as it was at the start, the way a trace plans its chunks.
//
// openText owns src: closing the reader it returns closes src, and on
// an error src is already closed.
func openText(src *os.File, kind filekind.Kind) (io.ReadCloser, compression.Format, error) {
	info, err := src.Stat()
	if err != nil {
		_ = src.Close()
		return nil, compression.FormatNone, fmt.Errorf("stat input: %w", err)
	}
	format := kind.Format
	if !kind.IsCompressed() {
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

// readCloser joins a reader with the file it reads from.
type readCloser struct {
	io.Reader
	io.Closer
}

// isSameFile reports whether output names the input file: the same path
// after cleaning, or, when the output exists, the same file reached
// another way (a symbolic or hard link). input is the stat of the input
// as it was opened, so the input is not looked up by path again.
func isSameFile(inputPath string, input os.FileInfo, output string) bool {
	if filepath.Clean(inputPath) == filepath.Clean(output) {
		return true
	}
	outInfo, err := os.Stat(output)
	if err != nil {
		return false
	}
	return os.SameFile(input, outInfo)
}

// decompressedSize is the length of the text the frames of tbl hold.
func decompressedSize(tbl *seekable.SeekTable) int64 {
	if len(tbl.Frames) == 0 {
		return 0
	}
	return tbl.Frames[len(tbl.Frames)-1].DecompressedEnd()
}
