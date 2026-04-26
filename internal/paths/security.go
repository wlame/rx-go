// Package paths implements the --search-root sandbox that prevents
// rx-go from reading files outside configured root directories.
//
// Both the HTTP handlers and the CLI entry point call
// ValidatePathWithinRoots for every user-supplied path before any
// filesystem access. The error envelope produced on rejection
// matches Python's exact message string (per user decision 6.9.3,
// the HTTP handler wraps this into a Go-idiomatic error shape).
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The sandbox state is protected by a read-write mutex because the HTTP
// server reads it on every request while a startup goroutine sets it
// exactly once (plus test code that calls Reset for isolation).
var (
	searchRootsMu sync.RWMutex
	searchRoots   []string
)

// ErrPathOutsideRoots is returned when ValidatePathWithinRoots rejects
// a path. Callers can use errors.As to extract the Path + Roots fields
// for structured error responses.
//
// The Error() string is Python-compatible so CLI output parity isn't
// broken; HTTP handlers wrap this into a Go-idiomatic JSON error.
type ErrPathOutsideRoots struct {
	Path  string   // path as supplied by the user (not resolved)
	Roots []string // snapshot of configured roots at the moment of rejection
}

// Error returns the Python-compatible message:
//
//	"Access denied: path '<p>' is outside all search roots: '<r1>', '<r2>'"
func (e *ErrPathOutsideRoots) Error() string {
	quoted := make([]string, len(e.Roots))
	for i, r := range e.Roots {
		quoted[i] = "'" + r + "'"
	}
	return fmt.Sprintf("Access denied: path '%s' is outside all search roots: %s",
		e.Path, strings.Join(quoted, ", "))
}

// ErrNoSearchRootsConfigured is returned when validation runs before
// SetSearchRoots (or after Reset). HTTP handlers treat this as a 500
// (internal misconfiguration); the CLI treats it as "no sandbox, allow
// anything".
var ErrNoSearchRootsConfigured = errors.New("search roots not configured")

// SetSearchRoots validates and stores a list of root directories.
//
// Each input path is:
//  1. required to be non-empty.
//  2. resolved to an absolute path (filepath.Abs).
//  3. checked to exist and be a directory.
//  4. dereferenced for symlinks (filepath.EvalSymlinks) so the
//     stored root is the true filesystem location.
//
// Duplicates are dropped. Empty input yields ErrNoSearchRootsConfigured
// on every later Validate call — callers that want "no sandbox"
// behavior should call Reset() instead.
func SetSearchRoots(roots []string) error {
	resolved := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))

	for _, r := range roots {
		if r == "" {
			return fmt.Errorf("search root cannot be empty")
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			return fmt.Errorf("search root %q: %w", r, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("search root %q: %w", r, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("search root %q is not a directory", r)
		}
		// Follow symlinks so the stored root is the canonical form.
		// Rationale: if a user configures /home/u/logs which is a
		// symlink to /data/logs, later Validate calls for /data/logs/app.log
		// should succeed; storing the resolved form makes the prefix
		// check work transparently.
		canon, err := filepath.EvalSymlinks(abs)
		if err != nil {
			canon = abs // fall back to Abs if EvalSymlinks fails
		}
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		resolved = append(resolved, canon)
	}

	searchRootsMu.Lock()
	searchRoots = resolved
	searchRootsMu.Unlock()
	return nil
}

// GetSearchRoots returns a snapshot of the configured roots. The slice
// is a copy — callers may mutate it safely.
func GetSearchRoots() []string {
	searchRootsMu.RLock()
	defer searchRootsMu.RUnlock()
	out := make([]string, len(searchRoots))
	copy(out, searchRoots)
	return out
}

// Reset clears the sandbox — subsequent Validate calls return
// ErrNoSearchRootsConfigured. Intended for tests.
func Reset() {
	searchRootsMu.Lock()
	searchRoots = nil
	searchRootsMu.Unlock()
}

// ValidatePathWithinRoots checks that path is inside one of the
// configured search roots. Returns the cleaned, absolute form of path
// on success.
//
// Semantics:
//   - Paths that don't exist are still validated (by Abs alone) so that
//     the caller can distinguish "outside sandbox" (ErrPathOutsideRoots)
//     from "doesn't exist" (the caller's own os.Stat will return ENOENT).
//   - Symlinks in the PATH are followed when possible. If EvalSymlinks
//     fails (e.g. the path doesn't exist), we fall back to Abs.
//   - A path that IS a configured root counts as "inside" itself.
//   - Matching is prefix-based on the cleaned, absolute form — the
//     prefix must be followed by the OS path separator to avoid a
//     false positive on e.g. /tmp/roota matching root /tmp/root.
func ValidatePathWithinRoots(path string) (string, error) {
	snapshot := GetSearchRoots()
	if len(snapshot) == 0 {
		return "", ErrNoSearchRootsConfigured
	}

	// Resolve the user-supplied path to an absolute form. This is what we
	// return to the caller — downstream consumers (cache hashers, file
	// open) have always worked with the non-canonical Abs form, so
	// returning the canonical (symlink-resolved) form here would break
	// cache lookups silently on macOS (where /var/... and /private/var/...
	// hash to different cache keys).
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	// For the sandbox prefix check we need the CANONICAL form: search
	// roots are stored canonicalized, and filepath.EvalSymlinks can't
	// handle nonexistent paths, so we walk up to the deepest existing
	// ancestor and re-append the tail. On macOS this collapses
	// /var/folders/... → /private/var/folders/... before comparing.
	canonical, err := resolveExistingAncestor(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}

	sep := string(filepath.Separator)
	for _, root := range snapshot {
		if canonical == root || strings.HasPrefix(canonical, root+sep) {
			return absPath, nil
		}
	}
	return "", &ErrPathOutsideRoots{Path: path, Roots: snapshot}
}

// IsPathWithinRoots is a bool-returning wrapper — useful for UI checks
// where an explicit error is overkill.
func IsPathWithinRoots(path string) bool {
	_, err := ValidatePathWithinRoots(path)
	return err == nil
}

// resolveExistingAncestor returns the canonical absolute form of abs
// with symlinks in the EXISTING portion resolved. Any nonexistent
// trailing components are preserved verbatim.
//
// Motivation: filepath.EvalSymlinks returns ENOENT if any component of
// its input doesn't exist, so a path like /var/folders/.../new.log
// (where /var → /private/var is a symlink and new.log doesn't exist
// yet) cannot be canonicalized by EvalSymlinks directly. We walk up
// toward the root until we find an existing ancestor, EvalSymlinks that
// ancestor, then rejoin the tail components.
//
// On a genuine I/O error (permission denied, etc.) on any ancestor we
// surface it — the caller treats that as a sandbox-internal failure
// rather than silently letting the path through.
func resolveExistingAncestor(abs string) (string, error) {
	current := abs
	var suffix []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			// Found the deepest existing ancestor. Resolve its symlinks
			// then rejoin the tail we accumulated on the way up.
			var canon string
			canon, err = filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				canon = filepath.Join(canon, suffix[i])
			}
			return canon, nil
		}
		if !os.IsNotExist(err) {
			// Permission denied or other non-ENOENT — fail closed.
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Hit the filesystem root without finding anything. On real
			// systems "/" always exists, so this should be unreachable;
			// returning abs unchanged is the safe fallback.
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
