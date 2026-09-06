//go:build darwin

package index

import (
	"os"
	"syscall"
	"time"
)

// sourceIdentity pulls the inode number and the inode-change time out of
// the OS-specific stat structure behind an os.FileInfo.
//
// Identical to the Linux version except that darwin spells the
// inode-change timespec Ctimespec rather than Ctim, which is the whole
// reason these two files are separated by build tags.
func sourceIdentity(info os.FileInfo) (inode uint64, changed time.Time, ok bool) {
	st, cast := info.Sys().(*syscall.Stat_t)
	if !cast {
		return 0, time.Time{}, false
	}
	return st.Ino, time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec), true
}
