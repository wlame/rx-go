//go:build linux

package index

import (
	"os"
	"syscall"
	"time"
)

// sourceIdentity pulls the inode, the device and the inode-change time
// out of the OS-specific stat structure behind an os.FileInfo.
//
// Go's os.FileInfo exposes only size, mode, and mtime. Sys() returns the
// raw platform struct, which on Linux is *syscall.Stat_t and carries the
// fields we need. The ctime field is spelled Ctim here and Ctimespec on
// darwin, which is why this lives in a build-tagged file.
//
// The second return value reports whether the cast succeeded; a
// filesystem that does not supply a Stat_t leaves the caller to fall
// back to size and mtime alone.
func sourceIdentity(info os.FileInfo) (statIdentity, bool) {
	st, cast := info.Sys().(*syscall.Stat_t)
	if !cast {
		return statIdentity{}, false
	}
	return statIdentity{
		inode: st.Ino,
		// The conversion is a no-op on the 64-bit platforms rx builds
		// for, and widens the 32-bit Dev of a few others.
		device:  uint64(st.Dev),
		changed: time.Unix(st.Ctim.Sec, st.Ctim.Nsec),
	}, true
}
