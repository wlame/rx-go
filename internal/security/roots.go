package security

import (
	"os"
	"path/filepath"
)

// SearchRootsManager manages allowed search root directories
type SearchRootsManager struct {
	roots []string
}

// NewSearchRootsManager creates a new search roots manager
func NewSearchRootsManager(roots []string) *SearchRootsManager {
	normalizedRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		// Resolve symlinks to ensure consistent path comparison
		if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
			normalizedRoots = append(normalizedRoots, resolved)
		} else {
			normalizedRoots = append(normalizedRoots, absRoot)
		}
	}
	return &SearchRootsManager{roots: normalizedRoots}
}

// GetRoots returns the list of allowed search roots
func (m *SearchRootsManager) GetRoots() []string {
	return m.roots
}

// IsAllowed checks if a path is within allowed search roots
func (m *SearchRootsManager) IsAllowed(path string) bool {
	if len(m.roots) == 0 {
		return true // No restrictions
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	for _, root := range m.roots {
		if allowed, _ := IsWithinRoot(absPath, root); allowed {
			return true
		}
	}

	return false
}

// ValidateAndNormalize validates a path and returns its normalized form
func (m *SearchRootsManager) ValidateAndNormalize(path string) (string, error) {
	absPath, err := NormalizePath(path)
	if err != nil {
		return "", err
	}

	if !m.IsAllowed(absPath) {
		return "", os.ErrPermission
	}

	return absPath, nil
}
