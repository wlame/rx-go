package webapi

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// zeroInodeStat is a directory's stat as a filesystem that numbers no
// file reports it: inode 0 (some FUSE and network filesystems).
type zeroInodeStat struct {
	os.FileInfo
	sys *syscall.Stat_t
}

// Sys returns the stat structure with inode 0.
func (s zeroInodeStat) Sys() any { return s.sys }

// The index task of a chain in a directory without an inode is keyed by
// the chain's handle: two such directories holding a chain of one name
// get two tasks, never one task that builds only the first chain's
// parts. A directory with an inode keys its chains by device and inode.
func TestChainKeyOf_ADirectoryWithoutAnInodeIsKeyedByTheHandle(t *testing.T) {
	describe := func(dir string, zeroInode bool) *logchain.Description {
		t.Helper()
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if zeroInode {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				t.Skipf("the stat of this platform is %T, with no inode", info.Sys())
			}
			zero := *st
			zero.Ino = 0
			info = zeroInodeStat{FileInfo: info, sys: &zero}
		}
		handle := filepath.Join(dir, "app.log")
		return &logchain.Description{
			Candidate: logchain.Candidate{Dir: dir, Name: "app.log", DirInfo: info},
			Response:  &rxtypes.ChainResponse{Path: handle},
		}
	}
	d1, d2 := t.TempDir(), t.TempDir()
	k1, k2 := chainKeyOf(describe(d1, true)), chainKeyOf(describe(d2, true))
	if k1 != chainTaskKey(filepath.Join(d1, "app.log")) || k2 != chainTaskKey(filepath.Join(d2, "app.log")) {
		t.Fatalf("keys %q and %q, want each chain's handle", k1, k2)
	}
	if k := chainKeyOf(describe(d1, false)); k == k1 {
		t.Fatalf("a directory with an inode is keyed by its handle: %q", k)
	}
}
