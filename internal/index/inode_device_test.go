package index

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The inode and device a stat records name one file: the same through
// two names of it (a hard link), different for another file, as
// os.SameFile tells them apart.
func TestInodeAndDevice_NamesOneFile(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("inode and device are read on linux and darwin only")
	}
	dir := t.TempDir()
	first, link, other := filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log"), filepath.Join(dir, "c.log")
	for _, path := range []string{first, other} {
		if err := os.WriteFile(path, []byte("LINE 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(first, link); err != nil {
		t.Fatal(err)
	}
	stat := func(path string) (uint64, uint64) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		inode, device, ok := InodeAndDevice(info)
		if !ok || inode == 0 {
			t.Fatalf("%s: inode %d device %d ok %v", path, inode, device, ok)
		}
		return inode, device
	}
	inodeA, deviceA := stat(first)
	inodeB, deviceB := stat(link)
	inodeC, deviceC := stat(other)
	if inodeA != inodeB || deviceA != deviceB {
		t.Fatalf("two names of one file: %d/%d and %d/%d", inodeA, deviceA, inodeB, deviceB)
	}
	if inodeA == inodeC && deviceA == deviceC {
		t.Fatalf("two files share inode %d on device %d", inodeA, deviceA)
	}
}
