// Package compression handles file-type detection for gzip/xz/bz2/zstd
// and their seekable-zstd variant. No decompression is performed here —
// higher-level callers pair Detect() with a stream reader from
// internal/seekable (for seekable zstd) or the klauspost/compress
// readers (for everything else).
//
// Why separate detection from decompression: the trace engine decides
// on a whole different code path based on the detected format (e.g.
// seekable-zstd uses per-frame parallelism; plain gzip uses a single
// streaming reader). The detector runs once per file; the decompressors
// run many times per request.
package compression

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Format enumerates the compression formats rx-go recognizes.
// The string values match Python's CompressionFormat enum exactly so
// they can be serialized into UnifiedFileIndex.compression_format.
type Format string

// Format constants. Keep values in sync with rx-python/src/rx/compression.py.
const (
	FormatNone         Format = ""
	FormatGzip         Format = "gzip"
	FormatZstd         Format = "zstd"
	FormatXz           Format = "xz"
	FormatBz2          Format = "bz2"
	FormatSeekableZstd Format = "seekable_zstd"
)

// magicBytes are the leading-byte signatures for each format. We probe
// at most 6 bytes (the longest magic, xz's FD 37 7A 58 5A 00).
//
// Ordering inside the slice matches Python's dict iteration order which
// is declaration order in CPython 3.7+.
type magicEntry struct {
	bytes  []byte
	format Format
}

var magicTable = []magicEntry{
	{[]byte{0x1f, 0x8b}, FormatGzip},
	{[]byte{0x28, 0xb5, 0x2f, 0xfd}, FormatZstd},
	{[]byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, FormatXz},
	{[]byte{0x42, 0x5a, 0x68}, FormatBz2}, // "BZh"
}

// extensionMap translates a file suffix (lowercase, including leading
// dot) to a Format. Mirrors Python's EXTENSION_MAP.
var extensionMap = map[string]Format{
	".gz":    FormatGzip,
	".gzip":  FormatGzip,
	".zst":   FormatZstd,
	".zstd":  FormatZstd,
	".xz":    FormatXz,
	".bz2":   FormatBz2,
	".bzip2": FormatBz2,
}

// compoundSuffixes enumerates "compound archive" suffixes. These are
// tarballs inside a compression frame; the Go port refuses to treat
// them as simple compressed text because decompression yields a tar
// binary, not searchable text. Caller should skip these entirely.
var compoundSuffixes = []string{
	".tar.gz",
	".tgz",
	".tar.zst",
	".tzst",
	".tar.xz",
	".txz",
	".tar.bz2",
	".tbz2",
	".tbz",
}

// ErrUnknownFormat is returned by helpers that require a known format.
var ErrUnknownFormat = errors.New("unknown compression format")

// IsCompoundArchive reports whether path ends in a compound-archive
// suffix like .tar.gz. Case-insensitive.
func IsCompoundArchive(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	for _, suf := range compoundSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// DetectFromPath returns the compression format for a file.
// Algorithm (matches Python):
//  1. If it's a compound archive, return FormatNone (caller should skip).
//  2. Match by extension; if a known extension matches, return that.
//  3. Fall back to reading the first 6 bytes and matching the magic table.
//
// Any I/O error during the magic-byte probe yields FormatNone with
// a wrapped error — the caller can decide whether to treat it as
// "skip this file" or propagate.
func DetectFromPath(path string) (Format, error) {
	if IsCompoundArchive(path) {
		return FormatNone, nil
	}

	if f := detectByExtension(path); f != FormatNone {
		return f, nil
	}

	// Extension didn't help — try magic bytes.
	f, err := os.Open(path)
	if err != nil {
		return FormatNone, err
	}
	defer func() {
		// Close error on a read-only file handle has no correctness impact.
		_ = f.Close()
	}()

	return DetectFromReader(f)
}

// DetectFromReader probes the first 6 bytes of r and returns the
// matching format, or FormatNone if nothing matches. On read errors
// returns FormatNone + err.
func DetectFromReader(r io.Reader) (Format, error) {
	buf := make([]byte, 6)
	n, err := io.ReadFull(r, buf)
	// io.ErrUnexpectedEOF is fine here — we still check the prefix we got.
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return FormatNone, err
	}
	buf = buf[:n]
	for _, entry := range magicTable {
		if len(buf) >= len(entry.bytes) && bytesEqual(buf[:len(entry.bytes)], entry.bytes) {
			return entry.format, nil
		}
	}
	return FormatNone, nil
}

// IsCompressed returns true if path's detected format is anything
// other than FormatNone. Convenience wrapper.
func IsCompressed(path string) bool {
	f, err := DetectFromPath(path)
	if err != nil {
		return false
	}
	return f != FormatNone
}

// detectByExtension applies extensionMap only. Returns FormatNone when
// no mapping exists. Called from DetectFromPath as a fast-path.
func detectByExtension(path string) Format {
	ext := strings.ToLower(filepath.Ext(path))
	if f, ok := extensionMap[ext]; ok {
		return f
	}
	return FormatNone
}

// bytesEqual is a tiny wrapper to avoid an import of bytes.Equal in a
// file that otherwise doesn't need it. The compiler lowers this to the
// same code as bytes.Equal.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
