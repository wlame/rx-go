package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidatePath checks if a path is safe and accessible
func ValidatePath(path string, searchRoots []string) error {
	// Clean the path
	cleanPath := filepath.Clean(path)

	// Check for path traversal attempts
	if strings.Contains(cleanPath, "..") {
		return fmt.Errorf("path traversal detected: %s", path)
	}

	// Get absolute path
	absPath, err := filepath.Abs(cleanPath)
	if err != nil {
		return fmt.Errorf("failed to resolve absolute path: %w", err)
	}

	// Check if path exists
	if _, err := os.Stat(absPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("path does not exist: %s", path)
		}
		return fmt.Errorf("failed to access path: %w", err)
	}

	// Check if path is within allowed search roots
	if len(searchRoots) > 0 {
		allowed := false
		for _, root := range searchRoots {
			absRoot, err := filepath.Abs(root)
			if err != nil {
				continue
			}

			// Check if path is under this root
			if strings.HasPrefix(absPath, absRoot) {
				allowed = true
				break
			}
		}

		if !allowed {
			return fmt.Errorf("path %s is not within allowed search roots", path)
		}
	}

	return nil
}

// NormalizePath normalizes a path and returns the absolute path
func NormalizePath(path string) (string, error) {
	cleanPath := filepath.Clean(path)
	absPath, err := filepath.Abs(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute path: %w", err)
	}

	// Resolve symlinks for consistent path comparison
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		return resolved, nil
	}

	return absPath, nil
}

// IsWithinRoot checks if a path is within a given root directory
func IsWithinRoot(path, root string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}

	// Ensure both paths end with separator for proper prefix matching
	if !strings.HasSuffix(absRoot, string(os.PathSeparator)) {
		absRoot += string(os.PathSeparator)
	}

	if !strings.HasSuffix(absPath, string(os.PathSeparator)) && !strings.HasPrefix(absPath+string(os.PathSeparator), absRoot) {
		// Not a directory and doesn't start with root
		return strings.HasPrefix(absPath, absRoot[:len(absRoot)-1]), nil
	}

	return strings.HasPrefix(absPath, absRoot), nil
}
