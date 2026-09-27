package logchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/trace"
)

// statWithInode is a stat whose inode is replaced: what a filesystem
// that numbers no file (inode 0, as some FUSE and network filesystems
// report) says about a directory.
type statWithInode struct {
	os.FileInfo
	sys *syscall.Stat_t
}

// Sys returns the stat structure with the replaced inode.
func (s statWithInode) Sys() any { return s.sys }

// withInode is info with its inode replaced by inode.
func withInode(t *testing.T, info os.FileInfo, inode uint64) os.FileInfo {
	t.Helper()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("the stat of this platform is %T, with no inode", info.Sys())
	}
	replaced := *st
	replaced.Ino = inode
	return statWithInode{FileInfo: info, sys: &replaced}
}

// A directory's identity is the device and inode its stat records. No
// stat, and inode 0 (which a filesystem that numbers no file gives
// every directory), are no identity: the directory is then keyed by its
// path.
func TestDirectoryIdentity_NoStatOrInodeZeroIsNoIdentity(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := DirectoryIdentity(nil); ok {
		t.Fatal("no stat gave an identity")
	}
	if device, inode, ok := DirectoryIdentity(withInode(t, info, 0)); ok {
		t.Fatalf("inode 0 gave the identity %d:%d", device, inode)
	}
	wantInode, wantDevice, _ := index.InodeAndDevice(info)
	if device, inode, ok := DirectoryIdentity(info); !ok || inode != wantInode || device != wantDevice {
		t.Fatalf("identity %d:%d (%v), want %d:%d", device, inode, ok, wantDevice, wantInode)
	}
}

// twoAppChains writes a chain app.log of two parts into each of two
// directories, each line naming its directory, and resolves both.
func twoAppChains(t *testing.T) (first, second Candidate) {
	t.Helper()
	d1, d2 := t.TempDir(), t.TempDir()
	writeFiles(t, d1, map[string][]byte{"app.log": []byte("hit d1\n"), "app.log.1": []byte("hit d1.1\n")})
	writeFiles(t, d2, map[string][]byte{"app.log": []byte("hit d2\n"), "app.log.1": []byte("hit d2.1\n")})
	return resolveIn(t, d1, "app.log"), resolveIn(t, d2, "app.log")
}

// plannedPaths lists the paths of the files a resolver has planned so
// far, sorted.
func plannedPaths(r *searchResolver) []string {
	var out []string
	for _, f := range r.plan.Files {
		out = append(out, f.Path())
	}
	slices.Sort(out)
	return out
}

// partPaths lists the paths of the parts of the chains given, sorted.
func partPaths(chains ...Candidate) []string {
	var out []string
	for _, c := range chains {
		for _, p := range c.Parts {
			out = append(out, p.Path)
		}
	}
	slices.Sort(out)
	return out
}

// Two chains whose directories give one stat — inode reuse within one
// request, or a directory swapped between two looks at it — are still
// both searched: the first as its chain, the second's parts as files of
// their own. None of them is lost, and none is skipped.
func TestSearch_TwoChainsGivenOneDirectoryStatAreBothSearched(t *testing.T) {
	first, second := twoAppChains(t)
	second.DirInfo = first.DirInfo

	r := newSearchResolver(SearchRequest{})
	for _, c := range []Candidate{first, second} {
		if err := r.addChain(context.Background(), c, nil); err != nil {
			t.Fatalf("add the chain of %s: %v", c.Dir, err)
		}
	}
	if want := partPaths(first, second); len(r.chains) != 1 || !slices.Equal(plannedPaths(r), want) || len(r.plan.Skipped) != 0 {
		t.Fatalf("chains %d, planned %v, skipped %v; want 1 chain and every part of both planned %v",
			len(r.chains), plannedPaths(r), r.plan.Skipped, want)
	}
}

// A directory without an inode (inode 0) has no identity: its chains
// are keyed by their handles, so two directories of a filesystem that
// numbers no file give two chains, each described and searched as one.
func TestSearch_ChainsOfDirectoriesWithoutAnInodeAreKeyedByTheirHandles(t *testing.T) {
	first, second := twoAppChains(t)
	first.DirInfo = withInode(t, first.DirInfo, 0)
	second.DirInfo = withInode(t, second.DirInfo, 0)

	if k1, k2 := chainKey(first), chainKey(second); k1 == k2 || k1 != "path:"+first.Handle() || k2 != "path:"+second.Handle() {
		t.Fatalf("keys %q and %q, want each chain's handle", k1, k2)
	}
	r := newSearchResolver(SearchRequest{})
	for _, c := range []Candidate{first, second} {
		if err := r.addChain(context.Background(), c, nil); err != nil {
			t.Fatalf("add the chain of %s: %v", c.Dir, err)
		}
	}
	if want := partPaths(first, second); len(r.chains) != 2 || !slices.Equal(plannedPaths(r), want) {
		t.Fatalf("chains %d, planned %v, want 2 chains and %v", len(r.chains), plannedPaths(r), want)
	}
}

// A directory of a walked tree swapped for a link to another chain's
// directory once the walk has listed it: its chain is keyed by the
// stat the walk listed it with, never by its path looked at again, so
// it is not mistaken for the other chain and dropped. When the swap
// moves its parts, the parts' pins say so (ErrPartChanged, 409 / exit
// 7); when the swap keeps them (each part a hard link to the other
// directory's), both chains are searched in full.
func TestSearch_ADirectorySwappedAfterTheWalkKeepsItsChain(t *testing.T) {
	for _, tc := range []struct {
		label     string
		hardLinks bool
	}{
		{"the swap moves the parts", false},
		{"the swap keeps the parts", true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			root := t.TempDir()
			d1, d2 := filepath.Join(root, "d1"), filepath.Join(root, "d2")
			for _, d := range []string{d1, d2} {
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			writeChainFiles(t, d1, []chainFile{
				{name: "app.log.1", text: timedLines(chainBase, time.Second, 1, 2, "1")},
				{name: "app.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 3, 2, "active")},
			})
			for _, name := range []string{"app.log.1", "app.log"} {
				var err error
				if tc.hardLinks {
					err = os.Link(filepath.Join(d1, name), filepath.Join(d2, name))
				} else {
					var body []byte
					if body, err = os.ReadFile(filepath.Join(d1, name)); err == nil {
						err = os.WriteFile(filepath.Join(d2, name), body, 0o600)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}

			walk := walkSearchedDirectory
			t.Cleanup(func() { walkSearchedDirectory = walk })
			walkSearchedDirectory = func(dir paths.Pinned, recursive bool) ([]paths.WalkEntry, error) {
				entries, err := walk(dir, recursive)
				if renameErr := os.Rename(d2, d2+".old"); renameErr != nil {
					t.Fatal(renameErr)
				}
				if linkErr := os.Symlink(d1, d2); linkErr != nil {
					t.Fatal(linkErr)
				}
				return entries, err
			}

			paths.Reset()
			res, err := Search(context.Background(), trace.New(), SearchRequest{Paths: []string{root}, Patterns: []string{"LINE"}})
			if !tc.hardLinks {
				if !errors.Is(err, ErrPartChanged) {
					t.Fatalf("search: %v, %+v; want ErrPartChanged, never an answer without d2's chain", err, res)
				}
				return
			}
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			answer := res.Answer
			if len(answer.Chains) != 2 || len(answer.Files) != 4 || len(answer.Matches) != 8 {
				t.Fatalf("chains %+v, files %v, %d matches; want two chains of two parts, every line of both",
					answer.Chains, answer.Files, len(answer.Matches))
			}
			for _, id := range []string{"c1", "c2"} {
				if ref := answer.Chains[id]; len(ref.Parts) != 2 {
					t.Fatalf("chain %s: %+v", id, ref)
				}
			}
		})
	}
}
