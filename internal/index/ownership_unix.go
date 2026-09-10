//go:build linux || darwin

package index

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// fileOwner resolves the uid a unix stat reports to a user name.
func fileOwner(info os.FileInfo) *string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	uid := strconv.FormatUint(uint64(stat.Uid), 10)
	if u, err := user.LookupId(uid); err == nil {
		return &u.Username
	}
	return &uid
}
