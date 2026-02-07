package compression

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Magic bytes for various compression formats
var (
	magicGzip  = []byte{0x1f, 0x8b}
	magicZstd  = []byte{0x28, 0xb5, 0x2f, 0xfd}
	magicBzip2 = []byte{0x42, 0x5a, 0x68}
	magicXz    = []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}
	magicLz4   = []byte{0x04, 0x22, 0x4d, 0x18}
)

// Detector detects compression formats
type Detector struct{}

// NewDetector creates a new compression detector
func NewDetector() *Detector {
	return &Detector{}
}

// DetectFile detects the compression format of a file
func (d *Detector) DetectFile(filePath string) (Format, error) {
	// First check by extension
	ext := strings.ToLower(filepath.Ext(filePath))
	formatByExt := d.detectByExtension(ext)

	// Then verify by magic bytes
	file, err := os.Open(filePath)
	if err != nil {
		return FormatUnknown, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Read first 16 bytes for magic detection
	buf := make([]byte, 16)
	n, err := file.Read(buf)
	if err != nil {
		return FormatUnknown, fmt.Errorf("failed to read file header: %w", err)
	}

	formatByMagic := d.detectByMagic(buf[:n])

	// If extension and magic agree, trust it
	if formatByExt == formatByMagic {
		return formatByExt, nil
	}

	// If magic says it's compressed, trust magic over extension
	if formatByMagic.IsCompressed() {
		return formatByMagic, nil
	}

	// If extension says compressed but magic says no, trust magic
	if formatByExt.IsCompressed() && formatByMagic == FormatNone {
		return FormatNone, nil
	}

	// Default to extension if magic is unknown
	if formatByMagic == FormatUnknown {
		return formatByExt, nil
	}

	return formatByMagic, nil
}

// detectByExtension detects format by file extension
func (d *Detector) detectByExtension(ext string) Format {
	switch ext {
	case ".gz", ".gzip":
		return FormatGzip
	case ".zst", ".zstd":
		return FormatZstd
	case ".bz2", ".bzip2":
		return FormatBzip2
	case ".xz":
		return FormatXz
	case ".lz4":
		return FormatLz4
	default:
		return FormatNone
	}
}

// detectByMagic detects format by magic bytes
func (d *Detector) detectByMagic(buf []byte) Format {
	if len(buf) < 2 {
		return FormatNone
	}

	// Check gzip (needs 2 bytes)
	if bytes.HasPrefix(buf, magicGzip) {
		return FormatGzip
	}

	// Check zstd (needs 4 bytes)
	if len(buf) >= 4 && bytes.HasPrefix(buf, magicZstd) {
		return FormatZstd
	}

	// Check bzip2 (needs 3 bytes)
	if len(buf) >= 3 && bytes.HasPrefix(buf, magicBzip2) {
		return FormatBzip2
	}

	// Check xz (needs 6 bytes)
	if len(buf) >= 6 && bytes.HasPrefix(buf, magicXz) {
		return FormatXz
	}

	// Check lz4 (needs 4 bytes)
	if len(buf) >= 4 && bytes.HasPrefix(buf, magicLz4) {
		return FormatLz4
	}

	return FormatNone
}

// IsCompressed checks if a file is compressed
func (d *Detector) IsCompressed(filePath string) bool {
	format, err := d.DetectFile(filePath)
	if err != nil {
		return false
	}
	return format.IsCompressed()
}
