// reader.go provides a decompression reader factory that auto-detects the compression
// format and returns an io.ReadCloser wrapping the appropriate native decompressor.
//
// All decompression is done in-process using pure Go libraries — no subprocess calls:
//   - gzip:  github.com/klauspost/compress/gzip
//   - zstd:  github.com/klauspost/compress/zstd
//   - xz:    github.com/ulikunitz/xz
//   - bzip2: compress/bzip2 (stdlib)
//
// The caller receives an io.ReadCloser and never needs to know which format was used.
package compression

import (
	"compress/bzip2"
	"fmt"
	"io"
	"os"

	kgzip "github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// NewReader auto-detects the compression format of the file at path and returns
// an io.ReadCloser that yields decompressed bytes.
//
// The caller must close the returned reader when done. Closing it also closes
// the underlying file handle.
//
// For SeekableZstd files this returns a sequential reader that decompresses all
// frames in order (parallel decompression is handled separately by the engine).
func NewReader(path string) (io.ReadCloser, CompressionFormat, error) {
	format, err := Detect(path)
	if err != nil {
		return nil, FormatNone, fmt.Errorf("detect compression: %w", err)
	}
	if format == FormatNone {
		return nil, FormatNone, fmt.Errorf("file is not compressed: %s", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, format, fmt.Errorf("open %s: %w", path, err)
	}

	rc, err := newDecompressor(f, format)
	if err != nil {
		f.Close()
		return nil, format, err
	}

	return rc, format, nil
}

// newDecompressor wraps the raw file in the appropriate decompression reader.
// The returned readCloser closes both the decompressor and the underlying file.
func newDecompressor(f *os.File, format CompressionFormat) (io.ReadCloser, error) {
	switch format {
	case FormatGzip:
		gr, err := kgzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		return &multiCloser{reader: gr, closers: []io.Closer{gr, f}}, nil

	case FormatXZ:
		xr, err := xz.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("xz reader: %w", err)
		}
		// xz.Reader does not implement io.Closer, so we only need to close the file.
		return &multiCloser{reader: xr, closers: []io.Closer{f}}, nil

	case FormatBZ2:
		// stdlib bzip2.NewReader returns io.Reader only (no Close).
		br := bzip2.NewReader(f)
		return &multiCloser{reader: br, closers: []io.Closer{f}}, nil

	case FormatZstd, FormatSeekableZstd:
		// klauspost's zstd decoder handles both regular and seekable zstd
		// when reading sequentially (it treats seekable frames as a normal stream).
		zr, err := zstd.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("zstd reader: %w", err)
		}
		return &zstdReadCloser{reader: zr, file: f}, nil

	default:
		return nil, fmt.Errorf("unsupported compression format: %s", format)
	}
}

// multiCloser wraps an io.Reader plus one or more io.Closers so the caller
// only needs to close a single handle.
type multiCloser struct {
	reader  io.Reader
	closers []io.Closer
}

func (mc *multiCloser) Read(p []byte) (int, error) {
	return mc.reader.Read(p)
}

func (mc *multiCloser) Close() error {
	var firstErr error
	for _, c := range mc.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// zstdReadCloser wraps klauspost's *zstd.Decoder (which uses Close to release
// resources but does not implement io.ReadCloser directly) and the underlying file.
type zstdReadCloser struct {
	reader *zstd.Decoder
	file   *os.File
}

func (z *zstdReadCloser) Read(p []byte) (int, error) {
	return z.reader.Read(p)
}

func (z *zstdReadCloser) Close() error {
	z.reader.Close()    // zstd.Decoder.Close does not return an error.
	return z.file.Close()
}
