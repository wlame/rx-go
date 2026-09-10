package index

import (
	"fmt"
	"os"
)

// FileOwnership reports the permission bits and owning user name that go
// into an index, in the form rx-python writes them.
//
// `permissions` is the last three octal digits of the mode — "644", not
// "0o100644" — which is what `oct(stat.st_mode)[-3:]` produces. `owner`
// is the user name, falling back to the numeric uid when the name cannot
// be resolved, which happens in a container with no passwd entry for the
// uid it runs as.
//
// The owner is nil where the platform does not report a uid rather than
// guessed: an index that claims the wrong owner is worse than one that
// says nothing.
func FileOwnership(info os.FileInfo) (permissions, owner *string) {
	mode := fmt.Sprintf("%03o", info.Mode().Perm())
	return &mode, fileOwner(info)
}
