package logchain

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/wlame/rx-go/internal/paths"
)

// ErrNotAChain reports that a handle names no chain: its directory
// holds fewer than two parts of that name, or is no directory. The
// routes answer it with 404 and the CLI with exit code 3.
var ErrNotAChain = errors.New("not a log chain")

// ErrInvalidHandle reports a handle whose last element is no bare file
// name: empty, ending with a separator, "." or "..". The routes answer
// it with 400 and the CLI with exit code 2.
var ErrInvalidHandle = errors.New("a chain handle must end with the chain's name")

// ErrNotADirectory reports that a path given to List is a file.
var ErrNotADirectory = errors.New("not a directory")

// Resolve finds the chain a handle names: the handle's directory, made
// absolute, and the candidate of that directory whose name is the
// handle's last element.
//
// The checks come first, and nothing is read before they pass: the
// last element must be a bare name (ErrInvalidHandle), the directory
// must lie inside a search root (paths.ValidatePathWithinRoots, whose
// *paths.ErrPathOutsideRoots or *paths.ErrHiddenPath comes back), and
// the name must not be hidden unless hidden entries are on
// (*paths.ErrHiddenPath). Without search roots, as for the CLI without
// --search-root, every directory is allowed. The directory is then
// pinned and listed once (paths.ListDir), and only the entries that can
// belong to the named chain are classified. The candidate keeps the
// pinned directory's stat (Candidate.DirInfo).
//
// The errors are those above, the pin's or the listing's (a directory
// that does not exist wraps fs.ErrNotExist, one the process may not
// read fs.ErrPermission), and ErrNotAChain.
func Resolve(handle string) (Candidate, error) {
	return resolveWith(handle, classifyListed)
}

// classifyListed is the Classify that Resolve checks the listed entries
// with: ClassifyPinned. A test replaces it to make a listing fail to
// read one file (an I/O error, too many open files, or a name that led
// to another file by the time it was opened), which no file on disk can
// be made to do on demand.
var classifyListed Classify = ClassifyPinned

// resolveWith is Resolve with the classifier as a parameter, so a test
// can count which entries are read.
func resolveWith(handle string, classify Classify) (Candidate, error) {
	dir, name, err := splitHandle(handle)
	if err != nil {
		return Candidate{}, err
	}
	dir, err = validateDir(dir)
	if err != nil {
		return Candidate{}, err
	}
	if paths.SkipEntry(name) {
		return Candidate{}, &paths.ErrHiddenPath{Path: handle, Component: name}
	}
	pinned, err := paths.Pin(dir)
	if err != nil {
		return Candidate{}, err
	}
	if !pinned.Info().IsDir() {
		return Candidate{}, ErrNotAChain
	}
	listed, err := paths.ListDir(pinned)
	if err != nil {
		return Candidate{}, err
	}
	for _, c := range groupNamed(dir, EntriesOf(listed), classify, name) {
		if c.Name == name {
			c.DirInfo = pinned.Info()
			return c, nil
		}
	}
	return Candidate{}, ErrNotAChain
}

// splitHandle splits a handle into its directory and its last element,
// which must be a bare file name.
func splitHandle(handle string) (dir, name string, err error) {
	if handle == "" || strings.HasSuffix(handle, string(filepath.Separator)) {
		return "", "", ErrInvalidHandle
	}
	name = filepath.Base(handle)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return "", "", ErrInvalidHandle
	}
	return filepath.Dir(handle), name, nil
}

// validateDir checks a directory against the search roots and returns
// it absolute. Without search roots every directory is allowed.
func validateDir(dir string) (string, error) {
	validated, err := paths.ValidatePathWithinRoots(dir)
	if errors.Is(err, paths.ErrNoSearchRootsConfigured) {
		return filepath.Abs(dir)
	}
	return validated, err
}

// EntriesOf turns a directory listing into Group's entries, leaving out
// the entries the listing refused (a link out of the roots or into a
// hidden entry, a link that leads nowhere, a named pipe). Each entry
// keeps the pin the listing made.
func EntriesOf(listed []paths.ListedEntry) []Entry {
	entries := make([]Entry, 0, len(listed))
	for _, l := range listed {
		if l.Refused != "" {
			continue
		}
		entries = append(entries, Entry{Name: l.Name, Path: l.Path, Info: l.Target.Info(), File: l.Target})
	}
	return entries
}
