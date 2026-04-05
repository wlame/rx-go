// scan.go implements recursive directory scanning with file classification.
//
// The scanner walks a directory tree using filepath.WalkDir (which uses os.DirEntry to
// avoid redundant stat calls), detects binary files by checking for null bytes in the
// first 8KB, and classifies files as text, binary, or compressed based on magic bytes.
package fileutil

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wlame/rx/internal/config"
)

// CompressionFormat identifies a file's compression type.
type CompressionFormat string

const (
	CompressionNone  CompressionFormat = ""
	CompressionGzip  CompressionFormat = "gzip"
	CompressionZstd  CompressionFormat = "zstd"
	CompressionXZ    CompressionFormat = "xz"
	CompressionBZ2   CompressionFormat = "bz2"
)

// FileClassification represents the type of a scanned file.
type FileClassification string

const (
	ClassText       FileClassification = "text"
	ClassBinary     FileClassification = "binary"
	ClassCompressed FileClassification = "compressed"
)

// FileInfo holds metadata about a scanned file.
type FileInfo struct {
	Path              string             // Absolute path to the file.
	Size              int64              // Size in bytes.
	Classification    FileClassification // text, binary, or compressed.
	CompressionFormat CompressionFormat  // Non-empty only when Classification == ClassCompressed.
}

// binaryCheckSize is how many bytes we read to detect binary content.
// Matches the Python reference implementation's sample_size default of 8192.
const binaryCheckSize = 8192

// hiddenDirPrefixes are directory name prefixes that the scanner skips.
// This matches the Python reference behavior of skipping hidden directories.
var defaultSkipDirs = map[string]bool{
	".git":         true,
	".svn":         true,
	".hg":          true,
	"__pycache__":  true,
	"node_modules": true,
	".tox":         true,
	".mypy_cache":  true,
	".pytest_cache": true,
}

// compressionMagic maps magic byte prefixes to compression formats.
// Order matters: longer prefixes are checked first to avoid false positives
// (e.g., xz has a 6-byte signature).
var compressionMagic = []struct {
	magic  []byte
	format CompressionFormat
}{
	{[]byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, CompressionXZ},  // xz (6 bytes)
	{[]byte{0x28, 0xb5, 0x2f, 0xfd}, CompressionZstd},             // zstd (4 bytes)
	{[]byte{0x42, 0x5a, 0x68}, CompressionBZ2},                     // bz2 "BZh" (3 bytes)
	{[]byte{0x1f, 0x8b}, CompressionGzip},                          // gzip (2 bytes)
}

// ScanDirectory recursively walks a directory tree, classifying each regular file
// as text, binary, or compressed. It skips hidden directories (names starting with '.')
// and common non-source directories (node_modules, __pycache__, etc.).
//
// The max files limit is read from cfg.MaxFiles. When reached, scanning stops early
// and the results collected so far are returned.
func ScanDirectory(dirPath string, cfg config.Config) ([]FileInfo, []string, error) {
	var files []FileInfo
	var skipped []string
	maxFiles := cfg.MaxFiles

	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// If the error is on the root directory itself, propagate it
			// (e.g., the directory doesn't exist). For other entries,
			// skip silently (permission errors on individual files, etc.).
			if path == dirPath {
				return err
			}
			return nil
		}

		// Skip hidden directories and common ignore patterns.
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || defaultSkipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}

		// Only process regular files (skip symlinks, devices, etc.).
		if !d.Type().IsRegular() {
			return nil
		}

		// Enforce max files limit across both lists.
		if len(files)+len(skipped) >= maxFiles {
			return filepath.SkipAll
		}

		info, err := d.Info()
		if err != nil {
			return nil // skip files we can't stat
		}

		fi, isBinary := classifyFile(path, info.Size())
		if isBinary {
			skipped = append(skipped, path)
		} else {
			files = append(files, fi)
		}

		return nil
	})

	if err != nil {
		return files, skipped, err
	}
	return files, skipped, nil
}

// classifyFile reads the first bytes of a file to determine its type.
// Returns the FileInfo and a boolean indicating whether the file should be skipped
// (true for binary files that are not compressed).
func classifyFile(path string, size int64) (FileInfo, bool) {
	fi := FileInfo{
		Path: path,
		Size: size,
	}

	// Read first 8KB for detection.
	header, err := readFileHeader(path, binaryCheckSize)
	if err != nil {
		// Can't read — treat as binary/skipped.
		fi.Classification = ClassBinary
		return fi, true
	}

	// Check for compression magic bytes first. Compressed files look binary
	// (they contain null bytes) but are processable.
	if fmt := detectCompression(header); fmt != CompressionNone {
		fi.Classification = ClassCompressed
		fi.CompressionFormat = fmt
		return fi, false
	}

	// Check for null bytes — the same heuristic Python uses.
	if containsNull(header) {
		fi.Classification = ClassBinary
		return fi, true
	}

	fi.Classification = ClassText
	return fi, false
}

// ClassifyFile determines the type of a single file (text, binary, or compressed).
// This is the exported version of classifyFile for use outside directory scanning —
// e.g., when the engine receives a single file path on the command line.
func ClassifyFile(path string, size int64) FileInfo {
	fi, _ := classifyFile(path, size)
	return fi
}

// DetectCompression checks the first bytes of data for known compression magic bytes.
// Exported so other packages can use it for standalone detection.
func DetectCompression(data []byte) CompressionFormat {
	return detectCompression(data)
}

// detectCompression checks magic bytes against known compression formats.
func detectCompression(data []byte) CompressionFormat {
	for _, m := range compressionMagic {
		if len(data) >= len(m.magic) && bytesEqual(data[:len(m.magic)], m.magic) {
			return m.format
		}
	}
	return CompressionNone
}

// DetectCompressionByPath checks a file's first bytes for known compression formats.
func DetectCompressionByPath(path string) (CompressionFormat, error) {
	// We only need the longest magic signature (6 bytes for xz), but read a bit more.
	header, err := readFileHeader(path, 16)
	if err != nil {
		return CompressionNone, err
	}
	return detectCompression(header), nil
}

// readFileHeader reads the first n bytes from a file.
func readFileHeader(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, n)
	nRead, err := io.ReadAtLeast(f, buf, 1)
	if err != nil {
		return nil, err
	}
	return buf[:nRead], nil
}

// containsNull returns true if data contains a null (0x00) byte.
// This is the same heuristic the Python reference uses for binary detection.
func containsNull(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

// bytesEqual compares two byte slices for equality.
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
