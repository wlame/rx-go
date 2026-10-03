package paths

import (
	"errors"
	"io/fs"
)

// Wordings FailureReason gives. Each is a fixed text: none carries a
// path, a search root or an error's own text, so a reason shown for a
// path a walk met says nothing about what the sandbox keeps out of
// reach, such as where a refused link leads.
const (
	// ReasonPermissionDenied: the process may not read the file or list
	// the directory. Every command words it this way.
	ReasonPermissionDenied = "permission denied"
	// ReasonNotFound: the path leads nowhere (it was removed, or it is a
	// link to nothing).
	ReasonNotFound = "no such file or directory"
	// ReasonOutsideRoots: the path is outside every search root.
	ReasonOutsideRoots = "outside all search roots"
	// ReasonHidden: the path has a hidden component and hidden entries
	// are off.
	ReasonHidden = "hidden entry; pass --hidden (or set RX_HIDDEN=true) to include hidden files and directories"
	// ReasonChanged: the path leads to another file than the one the
	// check saw.
	ReasonChanged = "the path leads to another file than the one that was checked"
	// ReasonUnreadable is the wording of any other failure to reach a
	// file. The error itself goes to the log, not into an answer.
	ReasonUnreadable = "cannot be read"
)

// failureReasons maps what an error is (errors.Is on a sentinel, or
// errors.As on one of this package's error types) to its wording. The
// first entry that matches wins.
var failureReasons = []struct {
	matches func(error) bool
	reason  string
}{
	{func(err error) bool { return errors.Is(err, fs.ErrPermission) }, ReasonPermissionDenied},
	{func(err error) bool { return errors.Is(err, ErrFileChanged) }, ReasonChanged},
	{func(err error) bool { var e *ErrPathOutsideRoots; return errors.As(err, &e) }, ReasonOutsideRoots},
	{func(err error) bool { var e *ErrHiddenPath; return errors.As(err, &e) }, ReasonHidden},
	{func(err error) bool { return errors.Is(err, fs.ErrNotExist) }, ReasonNotFound},
}

// FailureReason words why a path could not be checked, opened, listed or
// read, for an answer that names the path: one of the fixed Reason*
// texts above, never the error's own text. An OS error's text names the
// path it failed on, which for a link is its target, possibly outside
// every search root; a sandbox error names the roots. Neither belongs in
// an answer about a path the caller did not name.
func FailureReason(err error) string {
	for _, known := range failureReasons {
		if known.matches(err) {
			return known.reason
		}
	}
	return ReasonUnreadable
}
