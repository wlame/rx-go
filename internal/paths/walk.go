package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A directory walk reads only what naming the same path would let a
// caller read.
//
// ValidatePathWithinRoots guards every path a caller names. A walk of a
// directory reaches paths nobody named, and a symbolic link among them
// can lead anywhere: out of every search root, or into a hidden
// directory. So a walk resolves each link it meets and puts the target
// through the same check a named path gets (checkCanonical):
//
//   - inside a search root, and not hidden unless --hidden: the link is
//     followed. A link to a file is read under the link's own path; a
//     link to a directory is descended, when the walk is recursive.
//   - outside every root, or into a hidden entry: the link is refused,
//     with a reason, and the walk goes on.
//   - a link that cannot be resolved (it leads nowhere, or to itself)
//     is refused too.
//   - a link back to a directory the walk is already inside is refused,
//     so a loop cannot make the walk run for ever.
//
// Without a sandbox (the CLI without --search-root) there is no root to
// stay inside: a link is followed wherever it leads, as naming it would
// be allowed. Loops and unresolvable links are still refused.
//
// Entries whose own name is hidden are left out silently, as before and
// as ripgrep does (SkipEntry). An entry that is not a link is never
// checked: a real entry below a validated directory is inside the same
// root by construction.

// Reasons a walk gives for not following a symbolic link. The index
// command reports them as skip reasons.
const (
	// ReasonLinkOutsideRoots: the link leads outside every search root.
	ReasonLinkOutsideRoots = "symlink leads outside all search roots"
	// ReasonLinkLoop: the link leads back to a directory the walk is
	// already inside, such as `logs/all -> ..`.
	ReasonLinkLoop = "symlink loop: leads back to a directory the walk is inside"

	// reasonLinkHiddenFormat takes the hidden component, as the error
	// for a named hidden path does.
	reasonLinkHiddenFormat = "symlink leads into hidden entry '%s'; " +
		"pass --hidden (or set RX_HIDDEN=true) to include hidden files and directories"
	// reasonLinkUnresolvedFormat takes the resolution error.
	reasonLinkUnresolvedFormat = "cannot resolve symlink: %v"
)

// EntryTarget describes where one directory entry leads.
type EntryTarget struct {
	// IsDir reports whether the entry is, or leads to, a directory.
	IsDir bool
	// Canonical is the location a symlink leads to, every symlink on
	// the way resolved. It is empty for an entry that is not a symlink.
	Canonical string
	// Refused is non-empty when the entry is a symlink that must not
	// be followed, and says why. IsDir is meaningful only when the
	// target could be resolved.
	Refused string
}

// ResolveEntry tells where the directory entry at path leads and
// whether a walk or a listing may follow it. entry is what os.ReadDir
// returned for path; its type says whether path is a symlink without
// another system call.
//
// An entry that is not a symlink costs nothing: it is what it is.
func ResolveEntry(path string, entry fs.DirEntry) EntryTarget {
	if entry.Type()&fs.ModeSymlink == 0 {
		return EntryTarget{IsDir: entry.IsDir()}
	}
	// EvalSymlinks follows every link on the way, so a chain of links
	// ends at the real file. It fails on a link that leads nowhere and
	// on a cycle of links ("too many links"), never loops.
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return EntryTarget{Refused: fmt.Sprintf(reasonLinkUnresolvedFormat, err)}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return EntryTarget{Refused: fmt.Sprintf(reasonLinkUnresolvedFormat, err)}
	}
	return EntryTarget{
		IsDir:     info.IsDir(),
		Canonical: canonical,
		Refused:   targetRefusal(path, canonical),
	}
}

// targetRefusal applies the named-path check to a symlink's resolved
// target and words the outcome as a reason; "" means "follow it".
func targetRefusal(path, canonical string) string {
	roots := GetSearchRoots()
	if len(roots) == 0 {
		// No sandbox: nothing to stay inside.
		return ""
	}
	err := checkCanonical(path, canonical, roots)
	if err == nil {
		return ""
	}
	var hidden *ErrHiddenPath
	if errors.As(err, &hidden) {
		return fmt.Sprintf(reasonLinkHiddenFormat, hidden.Component)
	}
	return ReasonLinkOutsideRoots
}

// WalkEntry is one thing a directory walk reports: a file to read, a
// symlink it refused, or a subdirectory it could not list.
type WalkEntry struct {
	// Path is the entry as the walk reached it: the walked directory
	// followed by entry names. Symlinks in it are not resolved, so a
	// file reached through a link is reported under the link's path.
	Path string
	// Refused is non-empty for a symlink the walk did not follow, and
	// says why.
	Refused string
	// ReadErr is set for a directory below the walked one that could
	// not be listed. Callers decide whether that fails the walk.
	ReadErr error
}

// WalkDir lists the files a search of dir covers, in a stable order:
// os.ReadDir's (by name), depth first. When recursive is false only
// the entries directly inside dir are considered, and a directory, or a
// link to one, is passed over without being reported.
//
// dir itself is not checked here: the caller has already validated it
// as a named path. The error is non-nil only when dir cannot be listed.
func WalkDir(dir string, recursive bool) ([]WalkEntry, error) {
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	w := walker{recursive: recursive}
	if err := w.walk(dir, []string{canonical}); err != nil {
		return nil, err
	}
	return w.entries, nil
}

// walker carries one walk's settings and results through the recursion.
type walker struct {
	recursive bool
	entries   []WalkEntry
}

// walk lists dir and recurses into its subdirectories. ancestors holds
// the canonical path of dir and of every directory above it in this
// walk; a link that leads to one of them would start the walk over
// again, which is how a loop is recognized.
func (w *walker) walk(dir string, ancestors []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// Hidden entries are skipped before anything else, so a hidden
		// directory is not descended into either.
		if SkipEntry(entry.Name()) {
			continue
		}
		path := joinWalkPath(dir, entry.Name())
		target := ResolveEntry(path, entry)
		if target.IsDir && !w.recursive {
			continue
		}
		if target.Refused != "" {
			w.entries = append(w.entries, WalkEntry{Path: path, Refused: target.Refused})
			continue
		}
		if !target.IsDir {
			w.entries = append(w.entries, WalkEntry{Path: path})
			continue
		}

		// A real subdirectory's canonical path is its parent's plus its
		// name; a linked one's was resolved above.
		canonical := target.Canonical
		if canonical == "" {
			canonical = filepath.Join(ancestors[len(ancestors)-1], entry.Name())
		}
		if slices.Contains(ancestors, canonical) {
			w.entries = append(w.entries, WalkEntry{Path: path, Refused: ReasonLinkLoop})
			continue
		}
		// A fresh slice per descent: sibling directories never share
		// (and overwrite) one backing array for their ancestor lists.
		below := make([]string, 0, len(ancestors)+1)
		below = append(below, ancestors...)
		below = append(below, canonical)
		if err := w.walk(path, below); err != nil {
			w.entries = append(w.entries, WalkEntry{Path: path, ReadErr: err})
		}
	}
	return nil
}

// joinWalkPath appends name to dir without cleaning dir, so the paths a
// walk reports keep the caller's spelling of the directory ("./logs",
// a trailing slash) apart from the one separator added.
func joinWalkPath(dir, name string) string {
	if strings.HasSuffix(dir, string(filepath.Separator)) {
		return dir + name
	}
	return dir + string(filepath.Separator) + name
}
