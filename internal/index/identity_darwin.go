//go:build darwin

package index

import (
	"os"
	"syscall"
	"time"
)

// sourceIdentity pulls the inode, the device and the inode-change time
// out of the OS-specific stat structure behind an os.FileInfo.
//
// Identical to the Linux version except for two spellings: darwin
// names the inode-change timespec Ctimespec rather than Ctim, and its
// device number is an int32 rather than a uint64. That is the whole
// reason these two files are separated by build tags. The device is
// widened through uint32 so a number with its top bit set is stored as
// the same positive value every time.
func sourceIdentity(info os.FileInfo) (statIdentity, bool) {
	st, cast := info.Sys().(*syscall.Stat_t)
	if !cast {
		return statIdentity{}, false
	}
	return statIdentity{
		inode:   st.Ino,
		device:  uint64(uint32(st.Dev)), //nolint:gosec // G115: the device number is 32 opaque bits; reading them unsigned keeps one value per device
		changed: time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec),
	}, true
}
