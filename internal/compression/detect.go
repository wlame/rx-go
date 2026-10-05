// Package compression names the compression formats rx reads (gzip, xz,
// bzip2, zstd and its seekable variant), recognizes a format by its
// signature bytes (DetectFromReader) and opens a stream decoder for each
// (NewReader).
//
// What a file is, as every command sees it, is decided by package
// filekind, which uses DetectFromReader for the format and adds the
// seek-table check and the text check.
package compression

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
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
// dot) to a Format, for naming files only (FormatFromExtension).
var extensionMap = map[string]Format{
	".gz":    FormatGzip,
	".gzip":  FormatGzip,
	".zst":   FormatZstd,
	".zstd":  FormatZstd,
	".xz":    FormatXz,
	".bz2":   FormatBz2,
	".bzip2": FormatBz2,
}

// magicProbeBytes is how many bytes from the start of a file the magic
// table needs: its longest entry.
const magicProbeBytes = 6

// zstdSkippableMagic and zstdSkippableMask recognize a zstd skippable
// frame: its magic number is any of 0x184D2A50 to 0x184D2A5F (the low
// four bits are free), stored little-endian. A zstd stream may start
// with one: pzstd puts one before each frame, and a seekable file of no
// text is nothing but the skippable frame holding its seek table.
const (
	zstdSkippableMagic uint32 = 0x184D2A50
	zstdSkippableMask  uint32 = 0xFFFFFFF0
)

// DetectFromReader reads the first bytes of r and returns the format
// their signature names, or FormatNone when none matches. Only the bytes
// decide: a file's name says nothing about what it holds, so a text file
// named .gz is FormatNone and a gzip file named .log is FormatGzip. A
// zstd stream that starts with a skippable frame is FormatZstd. A read
// error returns FormatNone with the error.
func DetectFromReader(r io.Reader) (Format, error) {
	buf := make([]byte, magicProbeBytes)
	n, err := io.ReadFull(r, buf)
	// io.ErrUnexpectedEOF is fine here — we still check the prefix we got.
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return FormatNone, err
	}
	buf = buf[:n]
	for _, entry := range magicTable {
		if bytes.HasPrefix(buf, entry.bytes) {
			return entry.format, nil
		}
	}
	if len(buf) >= 4 && binary.LittleEndian.Uint32(buf)&zstdSkippableMask == zstdSkippableMagic {
		return FormatZstd, nil
	}
	return FormatNone, nil
}

// FormatFromExtension names the format path's last extension stands
// for (".gz", ".gzip", ".bz2", ".bzip2", ".xz", ".zst", ".zstd", in any
// case), or FormatNone when it stands for none. It reads no bytes, so
// it says nothing about what the file holds, and detection never uses
// it: rx compress uses it to name its output.
func FormatFromExtension(path string) Format {
	ext := strings.ToLower(filepath.Ext(path))
	if f, ok := extensionMap[ext]; ok {
		return f
	}
	return FormatNone
}
