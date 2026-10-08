package logchain

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// otherEncodingName is the other encoding of the middle part of the
// chain sameFileChain writes.
const otherEncodingName = "app.log.1.gz"

// sameFileChain writes into dir the chain app.log of three parts
// (app.log.2.gz, app.log.1, app.log), nine lines in all, each naming
// its line in the chain, with another encoding of its middle part
// beside it (app.log.1.gz, the same lines gzipped).
func sameFileChain(t *testing.T, dir string) {
	t.Helper()
	first := timedLines(chainBase.Add(time.Hour), time.Second, 4, 3, "1")
	writeChainFiles(t, dir, []chainFile{
		{name: "app.log.2.gz", text: timedLines(chainBase, time.Second, 1, 3, "2"), codec: compressedcopy.Gzip},
		{name: "app.log.1", text: first},
		{name: otherEncodingName, text: first, codec: compressedcopy.Gzip},
		{name: "app.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 7, 3, "active")},
	})
}

// requireEachLineOnceInOneChain fails the test unless answer holds the
// chain of sameFileChain once, ready, with its three parts; names its
// other encoding once in skipped_files, as duplicate_part, and searches
// it under no spelling; and gives each of the chain's nine lines once,
// in that chain, at the chain line the line names.
func requireEachLineOnceInOneChain(t *testing.T, label string, answer *rxtypes.ChainTraceResponse) {
	t.Helper()
	ref, ok := answer.Chains["c1"]
	if len(answer.Chains) != 1 || !ok || ref.State != rxtypes.ChainStateReady || len(ref.Parts) != 3 {
		t.Fatalf("%s: chains %+v, want one ready chain of three parts", label, answer.Chains)
	}
	if len(answer.SkippedFiles) != 1 || !strings.EqualFold(filepath.Base(answer.SkippedFiles[0]), otherEncodingName) ||
		len(answer.SkipReasons) != 1 || !strings.HasPrefix(answer.SkipReasons[0].Reason, ReasonDuplicatePart+":") {
		t.Fatalf("%s: skipped %v (%+v), want %s once as %s", label, answer.SkippedFiles, answer.SkipReasons,
			otherEncodingName, ReasonDuplicatePart)
	}
	for id, path := range answer.Files {
		if strings.EqualFold(filepath.Base(path), otherEncodingName) {
			t.Fatalf("%s: the other encoding %s is searched as %s", label, path, id)
		}
	}
	seen := map[string]bool{}
	for _, m := range answer.Matches {
		text := *m.LineText
		named, _ := strconv.ParseInt(globalOf.FindStringSubmatch(text)[1], 10, 64)
		if seen[text] || m.Chain == nil || *m.Chain != "c1" || m.ChainLine != named {
			t.Fatalf("%s: match %q (twice: %v) of %s in chain %v at chain_line %d", label, text, seen[text],
				answer.Files[m.File], m.Chain, m.ChainLine)
		}
		seen[text] = true
	}
	if len(seen) != 9 || len(answer.Matches) != 9 {
		t.Fatalf("%s: %d matches, %d lines, want the chain's 9 once each", label, len(answer.Matches), len(seen))
	}
}

// One directory reached under two spellings — another case on a
// case-insensitive disk, the macOS default — is one directory: its
// chain gets one entry whichever spelling comes first, each line comes
// once, in the chain, and the other encoding of a part is skipped once
// and searched under neither spelling, whichever comes first. The test
// needs a case-insensitive disk and skips on any other.
func TestSearch_OneDirectoryUnderTwoCaseSpellingsIsSearchedOnce(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, upper := filepath.Join(base, "logs"), filepath.Join(base, "LOGS")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(upper); err != nil {
		t.Skipf("the disk is case-sensitive: %s does not lead to %s (%v)", upper, dir, err)
	}
	sameFileChain(t, dir)
	// The line index is stored by the path as written, so each spelling
	// gets its own: the chain is ready whichever spelling holds it.
	for _, d := range []string{dir, upper} {
		storeIndexes(t, d, "app.log.2.gz", "app.log.1")
	}
	handle, upperHandle := filepath.Join(dir, "app.log"), filepath.Join(upper, "app.log")
	other, upperOther := filepath.Join(dir, otherEncodingName), filepath.Join(upper, otherEncodingName)
	for _, tc := range []struct {
		label string
		paths []string
	}{
		{"the handle under both spellings", []string{handle, upperHandle}},
		{"the handle under both spellings, the other first", []string{upperHandle, handle}},
		{"the handle, then the directory under the other spelling", []string{handle, upper}},
		{"the directory, then the handle under the other spelling", []string{upper, handle}},
		{"the directory under both spellings", []string{dir, upper}},
		{"the other encoding under the other spelling, then the handle", []string{upperOther, handle}},
		{"the handle, then the other encoding under the other spelling", []string{handle, upperOther}},
		{"the other encoding under the other spelling, then the directory", []string{upperOther, dir}},
		{"the directory, then the other encoding under the other spelling", []string{dir, upperOther}},
		{"the handle and the other encoding under one spelling, the directory under the other", []string{other, handle, upper}},
	} {
		requireEachLineOnceInOneChain(t, tc.label, searchFor(t, SearchRequest{Paths: tc.paths, Patterns: []string{"LINE"}}).Answer)
	}
}

// hardLinkAll makes each file of from a hard link in to, under its
// name: the same files under another path, as a bind mount of from
// shows them.
func hardLinkAll(t *testing.T, from, to string) {
	t.Helper()
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Link(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// One directory reached under two paths, on any disk: a bind mount
// shows one directory, and the same files, under a second path. A walk
// of the second path is made to list the directory with the first
// path's stat (a seam: no test can mount), and its files are hard links
// of the first's. The chain gets one entry whichever path comes first,
// each line comes once, in the chain, and the other encoding of a part
// is skipped once.
func TestSearch_OneDirectoryUnderTwoPathsIsSearchedOnce(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, d := range []string{first, second} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sameFileChain(t, first)
	hardLinkAll(t, first, second)
	for _, d := range []string{first, second} {
		storeIndexes(t, d, "app.log.2.gz", "app.log.1")
	}

	walk := walkSearchedDirectory
	t.Cleanup(func() { walkSearchedDirectory = walk })
	walkSearchedDirectory = func(dir paths.Pinned, recursive bool) ([]paths.WalkEntry, error) {
		entries, err := walk(dir, recursive)
		if err != nil || filepath.Base(dir.Path()) != "second" {
			return entries, err
		}
		pinnedFirst, err := paths.Pin(first)
		if err != nil {
			t.Fatal(err)
		}
		listedFirst, err := walk(pinnedFirst, recursive)
		if err != nil || len(listedFirst) == 0 {
			t.Fatalf("walk %s: %v", first, err)
		}
		for i := range entries {
			entries[i].Dir = listedFirst[0].Dir
		}
		return entries, nil
	}

	for _, tc := range []struct {
		label string
		paths []string
	}{
		{"the first path, then the second", []string{first, second}},
		{"the second path, then the first", []string{second, first}},
	} {
		requireEachLineOnceInOneChain(t, tc.label, searchFor(t, SearchRequest{Paths: tc.paths, Patterns: []string{"LINE"}}).Answer)
	}
}

// The other encoding of a part named on its own under another path,
// on any disk: another case of its directory or a bind mount gives the
// same file under a second path, and a hard link in a second directory
// stands in for them here. Whichever comes first, it or the chain's
// handle, the file is skipped once as duplicate_part and never
// searched, so each line comes once, in the chain.
func TestSearch_TheOtherEncodingUnderAnotherPathIsSkippedOnce(t *testing.T) {
	root := t.TempDir()
	dir, elsewhere := filepath.Join(root, "logs"), filepath.Join(root, "elsewhere")
	for _, d := range []string{dir, elsewhere} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sameFileChain(t, dir)
	storeIndexes(t, dir, "app.log.2.gz", "app.log.1")
	other := filepath.Join(elsewhere, otherEncodingName)
	if err := os.Link(filepath.Join(dir, otherEncodingName), other); err != nil {
		t.Fatal(err)
	}
	handle := filepath.Join(dir, "app.log")
	for _, tc := range []struct {
		label string
		paths []string
	}{
		{"the other encoding under another path, then the handle", []string{other, handle}},
		{"the handle, then the other encoding under another path", []string{handle, other}},
		{"the other encoding under another path, then the directory", []string{other, dir}},
		{"the directory, then the other encoding under another path", []string{dir, other}},
	} {
		requireEachLineOnceInOneChain(t, tc.label, searchFor(t, SearchRequest{Paths: tc.paths, Patterns: []string{"LINE"}}).Answer)
	}
}

// A chain is searched with every part it describes, even a part that is
// the same file as another chain's other encoding (a hard link of
// another directory's app.log.1.gz). That other encoding is skipped,
// named once, as its own chain lists it; the part is searched in its
// own chain. Whichever directory comes first, the answer gives the same
// lines in the same chains and names the same skipped file.
func TestSearch_APartThatIsAnotherChainsOtherEncodingStaysAPart(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, d := range []string{first, second} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sameFileChain(t, first)
	// second's chain: its part app.log.1.gz is first's other encoding,
	// its active file its own lines.
	if err := os.Link(filepath.Join(first, otherEncodingName), filepath.Join(second, otherEncodingName)); err != nil {
		t.Fatal(err)
	}
	writeChainFiles(t, second, []chainFile{
		{name: "app.log", text: timedLines(chainBase.Add(3*time.Hour), time.Second, 7, 3, "second")},
	})
	storeIndexes(t, first, "app.log.2.gz", "app.log.1")
	storeIndexes(t, second, otherEncodingName)

	type found struct {
		dir, text string
		inChain   bool
	}
	answerOf := func(paths []string) ([]found, []string) {
		t.Helper()
		answer := searchFor(t, SearchRequest{Paths: paths, Patterns: []string{"LINE"}}).Answer
		if len(answer.Chains) != 2 {
			t.Fatalf("%v: chains %+v, want two", paths, answer.Chains)
		}
		var out []found
		for _, m := range answer.Matches {
			out = append(out, found{dir: filepath.Base(filepath.Dir(answer.Files[m.File])), text: *m.LineText, inChain: m.Chain != nil})
		}
		slices.SortFunc(out, func(a, b found) int { return strings.Compare(a.dir+a.text, b.dir+b.text) })
		return out, answer.SkippedFiles
	}
	firstFound, firstSkipped := answerOf([]string{first, second})
	secondFound, secondSkipped := answerOf([]string{second, first})
	wantSkipped := []string{filepath.Join(first, otherEncodingName)}
	if !slices.Equal(firstSkipped, wantSkipped) || !slices.Equal(secondSkipped, wantSkipped) {
		t.Fatalf("skipped %v and %v, want %v in both orders", firstSkipped, secondSkipped, wantSkipped)
	}
	if !slices.Equal(firstFound, secondFound) || len(firstFound) != 9+6 {
		t.Fatalf("matches differ by order or in number:\n%v\n%v", firstFound, secondFound)
	}
	for _, f := range firstFound {
		if !f.inChain {
			t.Fatalf("match %+v has no chain", f)
		}
	}
}

// Two chains that give one key and whose parts are the same files — one
// directory under two paths — are one chain: the second's parts are
// not planned again, and nothing is skipped. Two chains that give one
// key with parts of their own are both searched
// (TestSearch_TwoChainsGivenOneDirectoryStatAreBothSearched).
func TestSearch_TwoChainsGivenOneKeyWithTheSameFilesAreOneChain(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	writeFiles(t, d1, map[string][]byte{"app.log": []byte("hit d1\n"), "app.log.1": []byte("hit d1.1\n")})
	hardLinkAll(t, d1, d2)
	first, second := resolveIn(t, d1, "app.log"), resolveIn(t, d2, "app.log")
	second.DirInfo = first.DirInfo

	r := newSearchResolver(SearchRequest{})
	for _, c := range []Candidate{first, second} {
		if err := r.addChain(context.Background(), c, nil); err != nil {
			t.Fatalf("add the chain of %s: %v", c.Dir, err)
		}
	}
	r.finish()
	if want := partPaths(first); len(r.chains) != 1 || !slices.Equal(plannedPaths(r), want) || len(r.plan.Skipped) != 0 ||
		len(r.chains[0].files) != 2 {
		t.Fatalf("chains %d, planned %v, skipped %v; want 1 chain of the first's parts %v",
			len(r.chains), plannedPaths(r), r.plan.Skipped, want)
	}
}

// One chain in a directory without an inode (inode 0), reached directly
// and through a link to its directory: the two handles give two keys,
// and the parts' canonical paths show one chain. It gets one entry and
// its parts are searched once.
func TestSearch_AChainWithoutAnInodeReachedThroughALinkHasOneEntry(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, link := filepath.Join(base, "d"), filepath.Join(base, "link")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string][]byte{"app.log": []byte("hit a\n"), "app.log.1": []byte("hit b\n")})
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	direct, linked := resolveIn(t, dir, "app.log"), resolveIn(t, link, "app.log")
	direct.DirInfo = withInode(t, direct.DirInfo, 0)
	linked.DirInfo = withInode(t, linked.DirInfo, 0)

	r := newSearchResolver(SearchRequest{})
	for _, c := range []Candidate{direct, linked} {
		if err := r.addChain(context.Background(), c, nil); err != nil {
			t.Fatalf("add the chain of %s: %v", c.Dir, err)
		}
	}
	r.finish()
	if want := partPaths(direct); len(r.chains) != 1 || !slices.Equal(plannedPaths(r), want) || len(r.chains[0].files) != 2 {
		t.Fatalf("chains %d, planned %v; want 1 chain of the parts %v", len(r.chains), plannedPaths(r), want)
	}
}
