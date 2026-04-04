// validate.go provides file validation utilities for checking existence, readability,
// text content, and size constraints.
package fileutil

import (
	"fmt"
	"io"
	"os"

	"github.com/wlame/rx/internal/config"
)

// ValidateFile checks that a path exists, is a regular file (not a directory),
// and is readable. Returns a descriptive error if any check fails.
func ValidateFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file not found: %s", path)
		}
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied: %s", path)
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if info.IsDir() {
		return fmt.Errorf("path is not a file: %s", path)
	}

	// Verify readability by attempting to open.
	f, err := os.Open(path)
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied: %s", path)
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	f.Close()

	return nil
}

// IsTextFile reads the first 8KB of a file and checks for null bytes.
// Returns true if the file appears to be text (no null bytes found).
// This matches the Python reference implementation's is_text_file behavior exactly.
//
// Returns false (not text) on any read error, matching Python's try/except behavior.
func IsTextFile(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, binaryCheckSize)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false, err
	}
	if n == 0 {
		// Empty file is considered text.
		return true, nil
	}

	return !containsNull(buf[:n]), nil
}

// ValidateFileSize checks that a file's size does not exceed the configured
// large file threshold (RX_LARGE_FILE_MB). Returns nil if within limits.
func ValidateFileSize(path string, cfg config.Config) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	maxBytes := int64(cfg.LargeFileMB) * 1024 * 1024
	if info.Size() > maxBytes {
		return fmt.Errorf("file %s exceeds size limit: %d bytes > %d MB", path, info.Size(), cfg.LargeFileMB)
	}

	return nil
}
