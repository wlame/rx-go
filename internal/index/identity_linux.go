//go:build linux

package index

import (
	"os"
	"syscall"
	"time"
)

// sourceIdentity pulls the inode number and the inode-change time out of
// the OS-specific stat structure behind an os.FileInfo.
//
// Go's os.FileInfo exposes only size, mode, and mtime. Sys() returns the
// raw platform struct, which on Linux is *syscall.Stat_t and carries the
// two fields we need. The field is spelled Ctim here and Ctimespec on
// darwin, which is why this lives in a build-tagged file.
//
// The third return value reports whether the cast succeeded; a
// filesystem that does not supply a Stat_t leaves the caller to fall
// back to size and mtime alone.
func sourceIdentity(info os.FileInfo) (inode uint64, changed time.Time, ok bool) {
	st, cast := info.Sys().(*syscall.Stat_t)
	if !cast {
		return 0, time.Time{}, false
	}
	return st.Ino, time.Unix(st.Ctim.Sec, st.Ctim.Nsec), true
}
