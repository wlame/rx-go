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
//
// Each directory is searched at most once per walk, so the work is
// bounded by the number of real directories, however many links lead
// into them. A second way into a directory already searched is refused
// with the path it was searched under; a link back to a directory the
// walk is inside is refused as a loop. Links to directories are followed
// only after every real directory below the walked one, so a directory
// reached both directly and through a link is searched under its own
// path.
//
// Without a sandbox (the CLI without --search-root) there is no root to
// stay inside: a link is followed wherever it leads, as naming it would
// be allowed. Loops, second ways in and unresolvable links are still
// refused.
//
// Entries whose own name is hidden are left out silently, as before and
// as ripgrep does (SkipEntry). An entry that is not a link is not put
// through the root check: a real entry below a checked directory is
// inside the same root by construction. The walk makes that hold even
// while the tree changes under it: each directory is listed through a
// handle checked to be the directory that was pinned (ListDir), and
// every file is reported as a Pinned, so a later read refuses a file
// that is not the one the walk found.

// Reasons a walk gives for not following a symbolic link. The index
// command reports them as skip reasons.
const (
	// ReasonLinkOutsideRoots: the link leads outside every search root.
	ReasonLinkOutsideRoots = "symlink leads outside all search roots"
	// ReasonLinkLoop: the link leads back to a directory the walk is
	// already inside, such as `logs/all -> ..`.
	ReasonLinkLoop = "symlink loop: leads back to a directory the walk is inside"

	// reasonAlreadySearchedFormat takes the path the directory was
	// searched under, earlier in the same walk.
	reasonAlreadySearchedFormat = "directory already searched through '%s'"

	// ReasonLinkHidden: the link leads into a hidden entry. It does not
	// name the entry: the link's target is not the caller's to learn.
	ReasonLinkHidden = "symlink leads into a hidden entry; " +
		"pass --hidden (or set RX_HIDDEN=true) to include hidden files and directories"
	// reasonLinkUnresolvedPrefix starts the reason of a link that
	// cannot be resolved; FailureReason's wording follows, never the
	// resolution error, whose text names the target.
	reasonLinkUnresolvedPrefix = "cannot resolve symlink: "
	// reasonUnreadableEntryPrefix starts the reason of an entry that is
	// not a link and cannot be stated (it vanished); FailureReason's
	// wording follows.
	reasonUnreadableEntryPrefix = "cannot stat entry: "
)

// ListedEntry is one entry of a directory listed by ListDir.
type ListedEntry struct {
	// Name is the entry's name in the directory.
	Name string
	// Path is the listed directory's path, as the caller spelled it,
	// followed by Name.
	Path string
	// Target is what the entry leads to: the entry itself, or the
	// target of a symbolic link. It is the zero Pinned when Refused is
	// set.
	Target Pinned
	// IsLink reports whether the entry is a symbolic link.
	IsLink bool
	// Refused is non-empty for an entry that must not be followed, and
	// says why: a link the named-path check refuses or that cannot be
	// resolved, or an entry that could not be stated.
	Refused string
	// refusedDir records, for a refused link, whether it leads to a
	// directory, so a walk that does not descend can pass over it
	// silently like any other directory.
	refusedDir bool
}

// IsDir reports whether the entry is, or leads to, a directory. For a
// refused link it says where the link leads, when that could be told.
func (e ListedEntry) IsDir() bool {
	if e.Target.IsZero() {
		return e.refusedDir
	}
	return e.Target.Info().IsDir()
}

// ListDir lists the directory dir, sorted by name, leaving out entries
// whose own name is hidden (SkipEntry). Each symbolic link is resolved
// and its target put through the named-path check; every entry that
// passes is returned with its target pinned.
//
// The listing is read through a handle checked to be dir's directory,
// and each entry that is not a link is stated relative to that handle,
// so a directory swapped for another (or for a link) after dir was
// pinned can neither be listed nor lend its files. Such a swap fails
// the listing with an error wrapping ErrFileChanged.
func ListDir(dir Pinned) ([]ListedEntry, error) {
	if dir.IsZero() {
		return nil, errors.New("list: no checked directory")
	}
	// Go note: os.Root holds the open directory; every name given to its
	// methods is resolved relative to that handle, not to a path that
	// could have changed since.
	r, err := os.OpenRoot(dir.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	opened, err := r.Stat(".")
	if err != nil {
		return nil, err
	}
	if !sameFile(dir.info, opened) {
		return nil, dir.changed()
	}
	names, err := readNames(r)
	if err != nil {
		return nil, err
	}

	listed := make([]ListedEntry, 0, len(names))
	for _, name := range names {
		// Hidden entries are skipped before anything else, so a hidden
		// directory is not descended into either.
		if SkipEntry(name) {
			continue
		}
		listed = append(listed, listEntry(r, dir, name))
	}
	return listed, nil
}

// readNames returns the names in the directory r holds, sorted, as
// os.ReadDir sorts them.
func readNames(r *os.Root) ([]string, error) {
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	// -1: every name in one call.
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

// listEntry describes the entry name of the directory r holds, which is
// dir.
func listEntry(r *os.Root, dir Pinned, name string) ListedEntry {
	entry := ListedEntry{Name: name, Path: joinWalkPath(dir.path, name)}
	// Lstat through r: relative to the checked directory handle, and a
	// link is described as a link rather than followed.
	info, err := r.Lstat(name)
	if err != nil {
		entry.Refused = reasonUnreadableEntryPrefix + FailureReason(err)
		return entry
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		if !isRegularOrDir(info) {
			// A named pipe, a socket or a device: never opened, so a
			// pipe cannot hold the walk's reader waiting for a writer.
			entry.Refused = ReasonNotRegularFile
			return entry
		}
		entry.Target = Pinned{path: entry.Path, canonical: filepath.Join(dir.canonical, name), info: info}
		return entry
	}

	entry.IsLink = true
	// Resolve the link from the directory's canonical location: the
	// same link, without walking again through every link of the
	// caller's spelling. Whatever it resolves to is checked on its own.
	canonical, err := filepath.EvalSymlinks(filepath.Join(dir.canonical, name))
	if err != nil {
		entry.Refused = reasonLinkUnresolvedPrefix + FailureReason(err)
		return entry
	}
	target, err := pinCanonical(entry.Path, canonical)
	if err != nil {
		entry.Refused = linkRefusal(err)
		// Only the type is taken from the refused target; nothing of
		// it is read.
		if info, statErr := os.Stat(canonical); statErr == nil {
			entry.refusedDir = info.IsDir()
		}
		return entry
	}
	if !isRegularOrDir(target.info) {
		// A link to a named pipe, a socket or a device.
		entry.Refused = ReasonNotRegularFile
		return entry
	}
	entry.Target = target
	return entry
}

// linkRefusal words the outcome of a link target's check as a reason.
func linkRefusal(err error) string {
	var hidden *ErrHiddenPath
	if errors.As(err, &hidden) {
		return ReasonLinkHidden
	}
	var outside *ErrPathOutsideRoots
	if errors.As(err, &outside) {
		return ReasonLinkOutsideRoots
	}
	return reasonLinkUnresolvedPrefix + FailureReason(err)
}

// WalkEntry is one thing a directory walk reports: a file to read, an
// entry it refused, or a subdirectory it could not list.
type WalkEntry struct {
	// Path is the entry as the walk reached it: the walked directory
	// followed by entry names. Symlinks in it are not resolved, so a
	// file reached through a link is reported under the link's path.
	Path string
	// File is the file to read, pinned to the file the walk checked.
	// Read it through File.Open. It is the zero Pinned when Refused or
	// ReadErr is set.
	File Pinned
	// Dir is the directory File was listed from, as the walk pinned it:
	// the walked directory, a subdirectory the walk entered, or the
	// directory a followed link leads to, under the walk's spelling
	// (Path is Dir.Path followed by the file's name). Dir.Info is the
	// stat the listing was checked against (ListDir refuses a directory
	// that is not the pinned one), so its device and inode name the
	// directory the file was found in, whatever its path leads to by
	// the time the caller looks. A caller that keys what it found by
	// the directory takes this stat rather than pinning the directory's
	// path again. It is the zero Pinned when Refused or ReadErr is set.
	Dir Pinned
	// Refused is non-empty for an entry the walk did not follow, and
	// says why.
	Refused string
	// ReadErr is set for a directory below the walked one that could
	// not be listed. Callers decide whether that fails the walk.
	ReadErr error
}

// WalkDir lists the files a search of dir covers, in a stable order:
// the real directories first, depth first and by name, then the
// directories reached through links, in the order the links were met.
// When recursive is false only the entries directly inside dir are
// considered, and a directory, or a link to one, is passed over without
// being reported.
//
// dir is pinned first (Pin), which checks it again as a named path. The
// error is non-nil only when dir cannot be pinned or listed.
func WalkDir(dir string, recursive bool) ([]WalkEntry, error) {
	top, err := Pin(dir)
	if err != nil {
		return nil, err
	}
	return WalkPinned(top, recursive)
}

// WalkPinned is WalkDir for a directory the caller has already pinned.
func WalkPinned(dir Pinned, recursive bool) ([]WalkEntry, error) {
	w := walker{
		recursive: recursive,
		searched:  map[string]string{dir.canonical: dir.path},
	}
	if err := w.walk(dir); err != nil {
		return nil, err
	}
	// Links to directories wait until every real directory has been
	// walked. Following one can queue more, so the loop reads the length
	// on every round instead of ranging over a copy.
	for i := 0; i < len(w.deferred); i++ {
		w.followLink(w.deferred[i])
	}
	return w.entries, nil
}

// walker carries one walk's settings and results through the recursion.
type walker struct {
	recursive bool
	entries   []WalkEntry
	// searched maps the canonical path of every directory the walk has
	// entered to the path it entered it under. It is what bounds a walk
	// by the number of real directories.
	searched map[string]string
	// deferred holds the links to directories met so far and not yet
	// followed.
	deferred []dirLink
}

// dirLink is a link to a directory, waiting to be followed.
type dirLink struct {
	// target is the directory the link leads to, under the link's path.
	target Pinned
	// from is the canonical path of the directory that holds the link.
	from string
}

// walk lists dir, reports its files and its refused entries, descends
// into its real subdirectories and queues its links to directories.
func (w *walker) walk(dir Pinned) error {
	listed, err := ListDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range listed {
		switch {
		case entry.IsDir() && !w.recursive:
			continue
		case entry.Refused != "":
			w.refuse(entry.Path, entry.Refused)
		case !entry.IsDir():
			// dir is the pin ListDir just checked its handle against, so
			// the file and its directory are reported as one listing saw
			// them.
			w.entries = append(w.entries, WalkEntry{Path: entry.Path, File: entry.Target, Dir: dir})
		case entry.IsLink:
			w.deferred = append(w.deferred, dirLink{target: entry.Target, from: dir.canonical})
		default:
			w.enter(entry.Target)
		}
	}
	return nil
}

// enter walks the directory dir unless the walk has searched it
// already, which a real directory only meets through a bind mount or a
// directory hard link.
func (w *walker) enter(dir Pinned) {
	if first, ok := w.searched[dir.canonical]; ok {
		w.refuse(dir.path, fmt.Sprintf(reasonAlreadySearchedFormat, first))
		return
	}
	w.searched[dir.canonical] = dir.path
	if err := w.walk(dir); err != nil {
		w.entries = append(w.entries, WalkEntry{Path: dir.path, ReadErr: err})
	}
}

// followLink walks the directory a link leads to, or refuses the link:
// as a loop when the directory holds the link, and as a second way in
// when the walk searched the directory elsewhere.
func (w *walker) followLink(link dirLink) {
	target := link.target.canonical
	if _, ok := w.searched[target]; ok && isSameOrAncestor(target, link.from) {
		w.refuse(link.target.path, ReasonLinkLoop)
		return
	}
	w.enter(link.target)
}

// refuse reports path as an entry the walk did not follow.
func (w *walker) refuse(path, reason string) {
	w.entries = append(w.entries, WalkEntry{Path: path, Refused: reason})
}

// isSameOrAncestor reports whether dir is path itself or a directory
// above it. Both are canonical.
func isSameOrAncestor(dir, path string) bool {
	if dir == path {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(path, dir)
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
