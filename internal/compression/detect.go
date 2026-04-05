// detect.go implements compression format detection via magic bytes and file extensions.
//
// Detection strategy (mirrors Python's compression.py):
//  1. Check file extension first (cheaper than reading bytes).
//  2. Fall back to magic byte signature from the first 6 bytes.
//  3. For zstd files, also probe for seekable zstd footer at end of file.
//
// Compound archives (.tar.gz, .tgz, etc.) are explicitly excluded because decompressing
// them yields tar data, not searchable text.
package compression

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// CompressionFormat identifies the compression type of a file.
type CompressionFormat int

const (
	FormatNone        CompressionFormat = iota // Not compressed
	FormatGzip                                 // gzip (.gz)
	FormatXZ                                   // xz (.xz)
	FormatBZ2                                  // bzip2 (.bz2)
	FormatZstd                                 // Standard zstd (.zst, .zstd)
	FormatSeekableZstd                         // Seekable zstd — has a seek table at end of file
)

// String returns a human-readable name for the compression format.
func (f CompressionFormat) String() string {
	switch f {
	case FormatNone:
		return "none"
	case FormatGzip:
		return "gzip"
	case FormatXZ:
		return "xz"
	case FormatBZ2:
		return "bz2"
	case FormatZstd:
		return "zstd"
	case FormatSeekableZstd:
		return "seekable_zstd"
	default:
		return "unknown"
	}
}

// magicSignature pairs a byte prefix with a compression format. The slice is ordered
// longest-first so that (e.g.) xz's 6-byte magic is checked before bz2's 3-byte one.
var magicSignatures = []struct {
	magic  []byte
	format CompressionFormat
}{
	{[]byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, FormatXZ},  // xz: 6 bytes
	{[]byte{0x28, 0xb5, 0x2f, 0xfd}, FormatZstd},             // zstd: 4 bytes
	{[]byte{0x42, 0x5a, 0x68}, FormatBZ2},                     // bzip2 "BZh": 3 bytes
	{[]byte{0x1f, 0x8b}, FormatGzip},                          // gzip: 2 bytes
}

// extensionMap maps file extensions to their compression format.
var extensionMap = map[string]CompressionFormat{
	".gz":   FormatGzip,
	".gzip": FormatGzip,
	".xz":   FormatXZ,
	".bz2":  FormatBZ2,
	".zst":  FormatZstd, // May be upgraded to SeekableZstd after footer check.
	".zstd": FormatZstd,
}

// compoundArchiveSuffixes are multi-layer archive extensions that should NOT be treated
// as simple compressed files (decompressing yields tar data, not text).
var compoundArchiveSuffixes = []string{
	".tar.gz", ".tgz",
	".tar.zst", ".tzst",
	".tar.xz", ".txz",
	".tar.bz2", ".tbz2", ".tbz",
}

// SeekTableFooterMagic is the 4-byte magic number at the start of the seekable zstd
// footer (last 9 bytes of the file). Little-endian encoding of 0x8F92EAB1.
const SeekTableFooterMagic uint32 = 0x8F92EAB1

// seekTableFooterSize is the footer: 4 byte magic + 4 byte num_frames + 1 byte flags.
const seekTableFooterSize = 9

// Detect determines the compression format of the file at path.
//
// It reads the first 6 bytes for magic-byte detection and, when the file looks like
// standard zstd, also checks the last 9 bytes for a seekable zstd footer.
// Falls back to extension-based detection when magic bytes are inconclusive.
func Detect(path string) (CompressionFormat, error) {
	if isCompoundArchive(path) {
		return FormatNone, nil
	}

	// Try magic bytes first (more reliable than extension).
	f, err := os.Open(path)
	if err != nil {
		// Can't read the file — try extension as last resort.
		return detectByExtension(path), nil
	}
	defer f.Close()

	var header [6]byte
	n, _ := f.Read(header[:])
	if n > 0 {
		for _, sig := range magicSignatures {
			if n >= len(sig.magic) && bytesHasPrefix(header[:n], sig.magic) {
				format := sig.format
				// If it's zstd, check for seekable footer.
				if format == FormatZstd {
					if hasSeekableFooter(f) {
						slog.Debug("compression detected", "path", path, "format", "seekable_zstd", "method", "magic+footer")
						return FormatSeekableZstd, nil
					}
				}
				slog.Debug("compression detected", "path", path, "format", format, "method", "magic")
				return format, nil
			}
		}
	}

	// Magic bytes didn't match — try extension fallback.
	format := detectByExtension(path)
	if format == FormatZstd {
		// Extension says zstd; check footer for seekable variant.
		if hasSeekableFooter(f) {
			slog.Debug("compression detected", "path", path, "format", "seekable_zstd", "method", "extension+footer")
			return FormatSeekableZstd, nil
		}
	}
	if format != FormatNone {
		slog.Debug("compression detected", "path", path, "format", format, "method", "extension")
	}
	return format, nil
}

// IsCompressed is a convenience wrapper that returns true when the file uses any
// recognized compression format (including seekable zstd).
func IsCompressed(path string) bool {
	f, _ := Detect(path)
	return f != FormatNone
}

// detectByExtension returns the compression format implied by the file's extension.
func detectByExtension(path string) CompressionFormat {
	ext := strings.ToLower(filepath.Ext(path))
	if f, ok := extensionMap[ext]; ok {
		return f
	}
	return FormatNone
}

// isCompoundArchive returns true for multi-layer archives like .tar.gz or .tgz.
func isCompoundArchive(path string) bool {
	lower := strings.ToLower(filepath.Base(path))
	for _, suffix := range compoundArchiveSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// hasSeekableFooter reads the last 9 bytes of the file and checks for the seekable
// zstd footer magic (0x8F92EAB1 in little-endian at the start of the footer).
func hasSeekableFooter(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Size() < seekTableFooterSize {
		return false
	}

	var footer [seekTableFooterSize]byte
	n, err := f.ReadAt(footer[:], info.Size()-seekTableFooterSize)
	if err != nil || n < seekTableFooterSize {
		return false
	}

	// Footer magic is stored little-endian in the first 4 bytes of the footer.
	magic := uint32(footer[0]) |
		uint32(footer[1])<<8 |
		uint32(footer[2])<<16 |
		uint32(footer[3])<<24

	return magic == SeekTableFooterMagic
}

// bytesHasPrefix checks whether data starts with prefix.
func bytesHasPrefix(data, prefix []byte) bool {
	if len(data) < len(prefix) {
		return false
	}
	for i, b := range prefix {
		if data[i] != b {
			return false
		}
	}
	return true
}
