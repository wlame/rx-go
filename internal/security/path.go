// path.go enforces that all file access stays within configured search root directories.
//
// Security properties:
//   - Symlink resolution: all paths resolved via filepath.EvalSymlinks before checking.
//   - Traversal prevention: "../" sequences are normalized by filepath.Clean + EvalSymlinks.
//   - Multiple roots: a path is valid if it falls under ANY configured search root.
//
// This is the Go equivalent of the Python path_security.py module.
package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidatePath checks that a resolved path falls within at least one of the provided
// search roots. It resolves symlinks on both the target path and the search roots
// before comparing, preventing symlink escape attacks and ../ traversal.
//
// For relative paths, each search root is tried as the base directory.
// For absolute paths, the path is resolved directly and checked against all roots.
//
// Returns the resolved absolute path on success, or an error describing the violation.
func ValidatePath(path string, searchRoots []string) (string, error) {
	if len(searchRoots) == 0 {
		return "", fmt.Errorf("no search roots configured")
	}

	for _, root := range searchRoots {
		resolved, err := validateAgainstRoot(path, root)
		if err == nil {
			return resolved, nil
		}
	}

	// Path didn't match any root — build a descriptive error.
	rootList := strings.Join(searchRoots, ", ")
	return "", fmt.Errorf("access denied: path %q is outside all search roots: %s", path, rootList)
}

// validateAgainstRoot checks whether path is within a single search root.
// Returns the resolved absolute path if valid, or an error if not.
func validateAgainstRoot(path, root string) (string, error) {
	// Resolve the root first — follow symlinks and normalize.
	resolvedRoot, err := resolvePathBestEffort(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}

	// Build the candidate path. For relative paths, join with the root.
	var candidate string
	if filepath.IsAbs(path) {
		candidate = path
	} else {
		candidate = filepath.Join(resolvedRoot, path)
	}

	// Resolve the candidate path — this follows symlinks and normalizes ../ sequences.
	// We use resolvePathBestEffort which handles non-existent files by resolving the
	// parent directory and appending the filename.
	resolvedCandidate, err := resolvePathBestEffort(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}

	// Check containment: the resolved candidate must be equal to or a child of the root.
	if isWithin(resolvedCandidate, resolvedRoot) {
		return resolvedCandidate, nil
	}

	return "", fmt.Errorf("path %q resolves to %q which is outside root %q", path, resolvedCandidate, resolvedRoot)
}

// isWithin returns true if child is equal to or a subdirectory/file under parent.
// Both paths must be absolute and clean (no trailing slashes except for root "/").
func isWithin(child, parent string) bool {
	// Exact match (e.g., root itself is valid).
	if child == parent {
		return true
	}

	// Ensure parent ends with separator so "/tmp/rootfoo" doesn't match "/tmp/root".
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}

	return strings.HasPrefix(child, prefix)
}

// resolvePathBestEffort resolves a path by following symlinks and cleaning ../ etc.
// If the path does not exist, it resolves the parent directory (which must exist)
// and appends the final component. This allows validation of not-yet-created files
// within a valid root.
func resolvePathBestEffort(path string) (string, error) {
	// First try to resolve directly — works when the path exists.
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved), nil
	}

	// Path doesn't exist — resolve parent, append base name.
	if os.IsNotExist(err) {
		parent := filepath.Dir(path)
		base := filepath.Base(path)

		resolvedParent, parentErr := filepath.EvalSymlinks(parent)
		if parentErr != nil {
			return "", fmt.Errorf("resolve parent %q: %w", parent, parentErr)
		}

		return filepath.Clean(filepath.Join(resolvedParent, base)), nil
	}

	return "", err
}
