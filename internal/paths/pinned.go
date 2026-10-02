package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A check of a path and a later open of the same path are two separate
// look-ups. Between them, a user who can write inside a search root can
// retarget a symbolic link, or replace a file or a directory with one,
// and the open then reads a file the check never saw: outside every
// root, or hidden.
//
// Pinned closes that gap. Pin checks a path and records the identity
// (device and inode) of the file it leads to at that moment; every
// later open goes through Pinned.Open, which looks at the file it
// actually opened and refuses it when it is not that file. Whatever a
// path leads to by the time of the read, only the checked file is read.
//
// The identity itself is taken through an os.Root opened at the search
// root (see statInsideRoot): os.Root resolves a path one component at a
// time and refuses to leave the directory it was opened at, so a
// directory swapped for a link to /etc while the check runs cannot make
// the check record a file outside the root.

// ErrFileChanged reports that a path no longer leads to the file that
// was checked: it was replaced, or a symbolic link on the way to it was
// retargeted, after the check.
var ErrFileChanged = errors.New("file changed after it was checked")

// Pinned is a file a caller may read, pinned to the file its path led
// to when it was checked.
//
// The zero value pins nothing, and opening it fails. A Pinned is made
// by Pin, by WalkDir for each file it reports, and by ListDir for each
// entry it lists; its fields are unexported so no other code can make
// one that skipped the check.
type Pinned struct {
	// path is the caller's spelling: what the user named, or the walked
	// directory followed by entry names. It is what is opened and what
	// is reported back.
	path string
	// canonical is where path led when it was checked, every symlink
	// resolved. The walk uses it to recognize a directory it has
	// already searched.
	canonical string
	// info is the file's stat at the check. Its device and inode are the
	// identity os.SameFile compares.
	info os.FileInfo
}

// Path returns the path as the caller spelled it.
func (p Pinned) Path() string { return p.path }

// Canonical returns where the path led when it was checked, with every
// symbolic link resolved.
func (p Pinned) Canonical() string { return p.canonical }

// Info returns the stat taken when the path was checked.
func (p Pinned) Info() os.FileInfo { return p.info }

// IsZero reports whether p pins nothing.
func (p Pinned) IsZero() bool { return p.info == nil }

// Open opens the path for reading and returns the file only when it is
// the file that was checked. Otherwise the file is closed unread and the
// error wraps ErrFileChanged.
//
// The path is opened under the caller's spelling, not the canonical
// one, so a link retargeted since the check is refused rather than read
// at its old target: everything else rx looks up by path (the trace
// cache, the line index) then describes the file that is read.
func (p Pinned) Open() (*os.File, error) {
	if p.IsZero() {
		return nil, errors.New("open: no checked file")
	}
	f, err := os.Open(p.path)
	if err != nil {
		return nil, err
	}
	// Stat on an open *os.File asks about the open file descriptor
	// itself (fstat), not about the path, so no later change to the
	// path can affect the answer.
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(p.info, opened) {
		_ = f.Close()
		return nil, p.changed()
	}
	return f, nil
}

// Stat returns the current stat of the path when it still leads to the
// file that was checked, and an error wrapping ErrFileChanged when it
// does not. Use it for a current size or modification time; the bytes
// are read through Open.
func (p Pinned) Stat() (os.FileInfo, error) {
	if p.IsZero() {
		return nil, errors.New("stat: no checked file")
	}
	info, err := os.Stat(p.path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(p.info, info) {
		return nil, p.changed()
	}
	return info, nil
}

// changed is the error for a path that no longer leads to p's file.
func (p Pinned) changed() error {
	return fmt.Errorf("%w: %s", ErrFileChanged, p.path)
}

// Pin checks path the way ValidatePathWithinRoots does — inside a search
// root, and no hidden component unless hidden entries are on — and
// records the identity of the file it leads to. Without a sandbox (no
// search roots, the CLI without --search-root) nothing is refused and
// only the identity is recorded.
//
// Unlike ValidatePathWithinRoots, the path must exist: there is nothing
// to pin otherwise. The errors are *ErrPathOutsideRoots, *ErrHiddenPath
// or the error of the look-up that failed.
func Pin(path string) (Pinned, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Pinned{}, fmt.Errorf("resolve %q: %w", path, err)
	}
	// EvalSymlinks follows every link on the way, so a chain of links
	// ends at the real file; it fails on a link that leads nowhere and
	// on a cycle of links ("too many links"), and never loops.
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Pinned{}, err
	}
	return pinCanonical(path, canonical)
}

// pinCanonical checks canonical, the location path leads to with every
// link resolved, against the search roots and records its identity.
func pinCanonical(path, canonical string) (Pinned, error) {
	roots := GetSearchRoots()
	if len(roots) > 0 {
		if err := checkCanonical(path, canonical, roots); err != nil {
			return Pinned{}, err
		}
	}
	info, err := statCanonical(canonical, roots)
	if err != nil {
		return Pinned{}, err
	}
	// canonical has no symbolic link left in it. Finding one there now
	// means the path was changed while it was being checked.
	if info.Mode()&fs.ModeSymlink != 0 {
		return Pinned{}, fmt.Errorf("%w: %s", ErrFileChanged, path)
	}
	return Pinned{path: path, canonical: canonical, info: info}, nil
}

// statCanonical stats canonical without following a final link. Inside
// a sandbox the stat goes through statInsideRoot, so it cannot describe
// a file outside the root that holds canonical.
func statCanonical(canonical string, roots []string) (os.FileInfo, error) {
	for _, root := range roots {
		if rel, ok := relativeToRoot(root, canonical); ok {
			return statInsideRoot(root, rel)
		}
	}
	// No sandbox: there is no root to stay inside.
	return os.Lstat(canonical)
}

// relativeToRoot returns canonical relative to root, "." for root
// itself, and false when canonical is not inside root.
func relativeToRoot(root, canonical string) (string, bool) {
	if canonical == root {
		return ".", true
	}
	prefix := root + string(filepath.Separator)
	if strings.HasPrefix(canonical, prefix) {
		return canonical[len(prefix):], true
	}
	return "", false
}

// statInsideRoot stats rel below root through an os.Root.
//
// Go note: os.Root (Go 1.24+) holds an open directory and resolves every
// name relative to it one component at a time, following a symbolic
// link only while it stays inside that directory. A component that was
// swapped for a link leading out of root since the canonical path was
// computed makes the stat fail instead of describing the file outside.
func statInsideRoot(root, rel string) (os.FileInfo, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return r.Lstat(rel)
}
