package paths

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Hidden entries — names beginning with a dot — are not served by
// default.
//
// The case this exists for: `rx serve` with no --search-root serves the
// current directory. Started from a home directory, that used to include
// ~/.ssh, ~/.aws and ~/.gnupg, readable through /v1/samples by anyone
// who could reach the port. The sandbox was working exactly as designed;
// the default root was the problem.
//
// The rule matches ripgrep's, which this tool's users already know: a
// hidden entry is one whose name starts with a dot, and --hidden opts
// back in.
//
// It is enforced here, in the sandbox, rather than in the directory
// walkers. Omitting hidden entries from a listing hides them from
// someone browsing; it does nothing about a caller who already knows the
// path and asks for it directly.
var includeHidden atomic.Bool

// SetIncludeHidden turns hidden entries on or off for every path check.
// Set once at startup from --hidden / RX_HIDDEN.
func SetIncludeHidden(include bool) { includeHidden.Store(include) }

// IncludeHidden reports whether hidden entries are currently served.
func IncludeHidden() bool { return includeHidden.Load() }

// ErrHiddenPath is returned when a path is inside the sandbox but
// reaches a hidden entry while hidden access is off.
//
// It is deliberately a different type from ErrPathOutsideRoots: both
// produce a 403, but one means "not yours to read" and the other means
// "turn on --hidden if you meant it", and a user who cannot tell them
// apart will chase the wrong problem.
type ErrHiddenPath struct {
	Path      string // path as supplied by the user
	Component string // the first hidden component found, e.g. ".ssh"
}

func (e *ErrHiddenPath) Error() string {
	return fmt.Sprintf(
		"Access denied: path '%s' contains hidden component '%s'; "+
			"pass --hidden (or set RX_HIDDEN=true) to include hidden files and directories",
		e.Path, e.Component)
}

// isHiddenName reports whether a single path component is hidden.
//
// "." and ".." are excluded: filepath.Clean removes them, so they never
// reach a real comparison, and treating ".." as hidden would give a
// confusing message for a traversal attempt the sandbox already catches.
func isHiddenName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return strings.HasPrefix(name, ".")
}

// hiddenComponentBelow returns the first hidden component of canonical
// that lies strictly below root, or "" if there is none.
//
// Components of the root itself are never checked. A root the operator
// named — `--search-root=~/.local/share/logs` — is a deliberate choice,
// and refusing to serve the thing you were pointed at would be absurd.
func hiddenComponentBelow(root, canonical string) string {
	rel, err := filepath.Rel(root, canonical)
	if err != nil || rel == "." {
		return ""
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if isHiddenName(part) {
			return part
		}
	}
	return ""
}

// SkipEntry reports whether a directory entry with the given name should
// be left out of a listing or a scan.
//
// Directory walkers call this so the rule is written once. It is the
// listing half of the same policy ValidatePathWithinRoots enforces for
// direct access: without it a hidden file would be unreachable by name
// yet still appear in the tree, which is the confusing half of both
// worlds.
func SkipEntry(name string) bool {
	return !IncludeHidden() && isHiddenName(name)
}
