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

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// sharedFileLines are the lines of the files twoChainsSharingAFile
// writes, by the file's path below the base directory: one line each,
// naming its line in its chain.
var sharedFileLines = map[string][]byte{
	"A/app.log.1":    timedLines(chainBase.Add(time.Hour), time.Second, 1, 1, "1"),
	"A/app.log.1.gz": timedLines(chainBase.Add(time.Hour), time.Second, 1, 1, "gz"),
	"A/app.log":      timedLines(chainBase.Add(2*time.Hour), time.Second, 2, 1, "active"),
	"B/y.log":        timedLines(chainBase.Add(2*time.Hour), time.Second, 2, 1, "y"),
}

// twoChainsSharingAFile writes two chains into the directories A and B
// of a new base directory and returns the base:
//
//	A/app.log.1     a part of the chain app.log
//	A/app.log.1.gz  another encoding of A/app.log.1, with a line of its own
//	A/app.log       the active file of app.log
//	B/y.log.1.gz    made by link from A/app.log.1.gz: a part of the chain y.log
//	B/y.log         the active file of y.log
//
// link is os.Link (B/y.log.1.gz is a hard link of the other encoding)
// or os.Symlink (a symbolic link to it). Both chains are indexed, so
// each is ready.
func twoChainsSharingAFile(t *testing.T, link func(oldname, newname string) error) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"A", "B"} {
		if err := os.Mkdir(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeChainFiles(t, filepath.Join(base, "A"), []chainFile{
		{name: "app.log.1", text: sharedFileLines["A/app.log.1"]},
		{name: "app.log.1.gz", text: sharedFileLines["A/app.log.1.gz"], codec: compressedcopy.Gzip},
		{name: "app.log", text: sharedFileLines["A/app.log"]},
	})
	writeChainFiles(t, filepath.Join(base, "B"), []chainFile{{name: "y.log", text: sharedFileLines["B/y.log"]}})
	if err := link(filepath.Join(base, "A", "app.log.1.gz"), filepath.Join(base, "B", "y.log.1.gz")); err != nil {
		t.Fatal(err)
	}
	storeIndexes(t, filepath.Join(base, "A"), "app.log.1")
	storeIndexes(t, filepath.Join(base, "B"), "y.log.1.gz")
	return base
}

// orderFreeAnswer is a chain search's answer reduced to what must not
// depend on the order of its paths, each path below the base directory:
// every match as its file, text, chain handle ("" for a file of its
// own) and chain line, sorted; every skipped file with the first word
// of its reason, sorted; every chain as its handle and its parts in its
// order, sorted.
type orderFreeAnswer struct {
	matches, skipped, chains []string
}

// orderFreeAnswerOf reduces answer to its orderFreeAnswer, with every
// path made relative to base.
func orderFreeAnswerOf(t *testing.T, base string, answer *rxtypes.ChainTraceResponse) orderFreeAnswer {
	t.Helper()
	rel := func(p string) string {
		r, err := filepath.Rel(base, p)
		if err != nil {
			t.Fatalf("%s is not below %s", p, base)
		}
		return filepath.ToSlash(r)
	}
	var out orderFreeAnswer
	for _, m := range answer.Matches {
		handle := ""
		if m.Chain != nil {
			handle = rel(answer.Chains[*m.Chain].Path)
		}
		out.matches = append(out.matches, matchRow(rel(answer.Files[m.File]), *m.LineText, handle, m.ChainLine))
	}
	for _, s := range answer.SkipReasons {
		reason, _, _ := strings.Cut(s.Reason, ":")
		out.skipped = append(out.skipped, rel(s.Path)+" "+reason)
	}
	for _, ref := range answer.Chains {
		parts := make([]string, 0, len(ref.Parts))
		for _, id := range ref.Parts {
			parts = append(parts, rel(answer.Files[id]))
		}
		out.chains = append(out.chains, rel(ref.Path)+": "+strings.Join(parts, ", "))
	}
	for _, list := range [][]string{out.matches, out.skipped, out.chains} {
		slices.Sort(list)
	}
	return out
}

// matchRow is one match of an orderFreeAnswer.
func matchRow(file, text, handle string, chainLine int64) string {
	return file + " | " + strings.TrimSuffix(text, "\n") + " | " + handle + " | " + strconv.FormatInt(chainLine, 10)
}

// requireOrderFreeAnswer searches base for every order of paths, each
// a key of named, and fails the test unless each answer is want.
func requireOrderFreeAnswer(t *testing.T, base string, named map[string]string, orders [][]string, want orderFreeAnswer) {
	t.Helper()
	for _, order := range orders {
		ps := make([]string, 0, len(order))
		for _, key := range order {
			ps = append(ps, named[key])
		}
		got := orderFreeAnswerOf(t, base, searchFor(t, SearchRequest{Paths: ps, Patterns: []string{"LINE"}}).Answer)
		if !slices.Equal(got.matches, want.matches) || !slices.Equal(got.skipped, want.skipped) ||
			!slices.Equal(got.chains, want.chains) {
			t.Fatalf("paths %v:\n got matches %q\n     skipped %q\n     chains  %q\nwant matches %q\n     skipped %q\n     chains  %q",
				order, got.matches, got.skipped, got.chains, want.matches, want.skipped, want.chains)
		}
	}
}

// permutations lists every order of items.
func permutations(items []string) [][]string {
	if len(items) <= 1 {
		return [][]string{slices.Clone(items)}
	}
	var out [][]string
	for i, first := range items {
		rest := slices.Concat(items[:i], items[i+1:])
		for _, tail := range permutations(rest) {
			out = append(out, append([]string{first}, tail...))
		}
	}
	return out
}

// sharedFilePaths are the paths a test of twoChainsSharingAFile names:
// each chain by its handle and by its directory, and B's part
// y.log.1.gz on its own.
func sharedFilePaths(base string) map[string]string {
	return map[string]string{
		"A": filepath.Join(base, "A", "app.log"), "Adir": filepath.Join(base, "A"),
		"B": filepath.Join(base, "B", "y.log"), "Bdir": filepath.Join(base, "B"),
		"Bpart": filepath.Join(base, "B", "y.log.1.gz"),
	}
}

// bothChainsOrders are the orders of paths that name both chains of
// twoChainsSharingAFile: every order of B's part, A and B, by handles
// and by directories, and both orders of the two chains alone.
func bothChainsOrders() [][]string {
	orders := slices.Concat(permutations([]string{"Bpart", "A", "B"}), permutations([]string{"Bpart", "Adir", "Bdir"}))
	return append(orders, []string{"A", "B"}, []string{"B", "A"}, []string{"Adir", "Bdir"}, []string{"Bdir", "Adir"})
}

// rowsOf are the match rows of the one line of each file named, in the
// chain whose handle is given ("" for a file of its own) at the chain
// line the line names (-1 for a file of its own).
func rowsOf(handle string, files ...string) []string {
	var rows []string
	for _, f := range files {
		text := string(sharedFileLines[f])
		if f == "B/y.log.1.gz" {
			text = string(sharedFileLines["A/app.log.1.gz"])
		}
		line := int64(-1)
		if handle != "" {
			line, _ = strconv.ParseInt(globalOf.FindStringSubmatch(text)[1], 10, 64)
		}
		rows = append(rows, matchRow(f, text, handle, line))
	}
	return rows
}

// A part of one chain that is a hard link of another chain's other
// encoding (B/y.log.1.gz and A/app.log.1.gz) is a part of its chain in
// every order of the paths, named on its own or not, by handles or by
// directories: B's chain searches it once, A's other encoding is
// skipped once as duplicate_part, and the answer is the same in every
// order, four matches.
func TestSearch_APartThatIsAHardLinkOfAnotherChainsOtherEncodingIsSearchedInEveryOrder(t *testing.T) {
	base := twoChainsSharingAFile(t, os.Link)
	want := orderFreeAnswer{
		matches: slices.Sorted(slices.Values(slices.Concat(
			rowsOf("A/app.log", "A/app.log.1", "A/app.log"), rowsOf("B/y.log", "B/y.log.1.gz", "B/y.log")))),
		skipped: []string{"A/app.log.1.gz " + ReasonDuplicatePart},
		chains:  []string{"A/app.log: A/app.log.1, A/app.log", "B/y.log: B/y.log.1.gz, B/y.log"},
	}
	requireOrderFreeAnswer(t, base, sharedFilePaths(base), bothChainsOrders(), want)
}

// A part of one chain that is a symbolic link to another chain's other
// encoding is a part of its chain in every order of the paths: the
// file is searched once, as that part, and not named as skipped, since
// it is searched; the answer is the same in every order.
func TestSearch_APartThatIsALinkToAnotherChainsOtherEncodingIsSearchedInEveryOrder(t *testing.T) {
	base := twoChainsSharingAFile(t, os.Symlink)
	want := orderFreeAnswer{
		matches: slices.Sorted(slices.Values(slices.Concat(
			rowsOf("A/app.log", "A/app.log.1", "A/app.log"), rowsOf("B/y.log", "B/y.log.1.gz", "B/y.log")))),
		chains: []string{"A/app.log: A/app.log.1, A/app.log", "B/y.log: B/y.log.1.gz, B/y.log"},
	}
	requireOrderFreeAnswer(t, base, sharedFilePaths(base), bothChainsOrders(), want)
}

// A hard link of another encoding, named on its own in another
// directory and under another name, is a file of its own: two hard
// links are two files, so it is searched, with no chain, and the other
// encoding itself is skipped once. Three matches, in either order.
func TestSearch_AHardLinkOfAnotherEncodingNamedOnItsOwnIsAFileOfItsOwn(t *testing.T) {
	base := twoChainsSharingAFile(t, os.Link)
	want := orderFreeAnswer{
		matches: slices.Sorted(slices.Values(slices.Concat(
			rowsOf("A/app.log", "A/app.log.1", "A/app.log"), rowsOf("", "B/y.log.1.gz")))),
		skipped: []string{"A/app.log.1.gz " + ReasonDuplicatePart},
		chains:  []string{"A/app.log: A/app.log.1, A/app.log"},
	}
	orders := [][]string{{"A", "Bpart"}, {"Bpart", "A"}, {"Adir", "Bpart"}, {"Bpart", "Adir"}}
	requireOrderFreeAnswer(t, base, sharedFilePaths(base), orders, want)
}

// A symbolic link to another encoding, named on its own, is the same
// path as that encoding once every link is resolved: it is not
// searched, and it is named in skipped_files as duplicate_part under
// its own path, beside the encoding, in either order.
func TestSearch_ALinkToAnotherEncodingNamedOnItsOwnIsThatEncoding(t *testing.T) {
	base := twoChainsSharingAFile(t, os.Symlink)
	want := orderFreeAnswer{
		matches: slices.Sorted(slices.Values(rowsOf("A/app.log", "A/app.log.1", "A/app.log"))),
		skipped: []string{"A/app.log.1.gz " + ReasonDuplicatePart, "B/y.log.1.gz " + ReasonDuplicatePart},
		chains:  []string{"A/app.log: A/app.log.1, A/app.log"},
	}
	orders := [][]string{{"A", "Bpart"}, {"Bpart", "A"}, {"Adir", "Bpart"}, {"Bpart", "Adir"}}
	requireOrderFreeAnswer(t, base, sharedFilePaths(base), orders, want)
}

// anEncodingThatIsALink writes the chain app.log into each directory
// of chainDirs (A when none is given) of a new base directory, whose
// other encoding of a part is a symbolic link to a file of the
// directory B, and returns the base:
//
//	A/app.log.1     a part of the chain app.log
//	A/app.log.1.gz  a symbolic link to B/z.gz: another encoding of A/app.log.1
//	A/app.log       the active file of app.log
//	B/z.gz          a gzip file of a line of its own, in no chain
//
// Each chain is indexed, so it is ready.
func anEncodingThatIsALink(t *testing.T, chainDirs ...string) string {
	t.Helper()
	if len(chainDirs) == 0 {
		chainDirs = []string{"A"}
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range append([]string{"B"}, chainDirs...) {
		if err := os.Mkdir(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeChainFiles(t, filepath.Join(base, "B"), []chainFile{
		{name: "z.gz", text: sharedFileLines["A/app.log.1.gz"], codec: compressedcopy.Gzip},
	})
	for _, d := range chainDirs {
		dir := filepath.Join(base, d)
		writeChainFiles(t, dir, []chainFile{
			{name: "app.log.1", text: sharedFileLines["A/app.log.1"]},
			{name: "app.log", text: sharedFileLines["A/app.log"]},
		})
		if err := os.Symlink(filepath.Join(base, "B", "z.gz"), filepath.Join(dir, "app.log.1.gz")); err != nil {
			t.Fatal(err)
		}
		storeIndexes(t, dir, "app.log.1")
	}
	return base
}

// When another encoding of a part is a symbolic link to a file the
// request names on its own, or that a walk of its directory lists, that
// file is the encoding once every link is resolved: it is not searched,
// and it is named in skipped_files as duplicate_part under its own
// path, beside the encoding's, so no path the request reaches is left
// out unnamed. The answer is the same in every order.
func TestSearch_TheTargetOfAnEncodingThatIsALinkIsNamedAsSkipped(t *testing.T) {
	base := anEncodingThatIsALink(t)
	named := map[string]string{
		"A": filepath.Join(base, "A", "app.log"), "Adir": filepath.Join(base, "A"),
		"Bz": filepath.Join(base, "B", "z.gz"), "Bdir": filepath.Join(base, "B"),
	}
	want := orderFreeAnswer{
		matches: slices.Sorted(slices.Values(rowsOf("A/app.log", "A/app.log.1", "A/app.log"))),
		skipped: []string{"A/app.log.1.gz " + ReasonDuplicatePart, "B/z.gz " + ReasonDuplicatePart},
		chains:  []string{"A/app.log: A/app.log.1, A/app.log"},
	}
	orders := slices.Concat(permutations([]string{"A", "Bz"}), permutations([]string{"Adir", "Bz"}),
		permutations([]string{"A", "Bdir"}), permutations([]string{"A", "Bz", "Bdir"}))
	requireOrderFreeAnswer(t, base, named, orders, want)
}

// statWithSize is a stat whose size is replaced and whose other fields,
// the device and inode among them, are the original's.
type statWithSize struct {
	os.FileInfo
	size int64
}

// Size returns the replaced size.
func (s statWithSize) Size() int64 { return s.size }

// Two chains that give one key, whose second has a part that is not
// the holder's part although the pins show its device and inode: a
// file that took the inode the holder's part had (inode reuse within
// one request, of the directory and of the part). The pins then
// disagree on the rest of the file's stat — here its size. That part
// is searched as a file of its own, never taken for the holder's part,
// whose slot reads another file.
//
// No disk reuses an inode on demand: the second chain's directory is
// given the first's stat, its part app.log.1 is a hard link of the
// first's (the device and inode a reused inode shows), and the stat its
// listing recorded is given another size.
func TestSearch_APartThatTookTheInodeOfTheHoldersPartIsAFileOfItsOwn(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	writeFiles(t, d1, map[string][]byte{"app.log": []byte("hit d1\n"), "app.log.1": []byte("hit d1.1\n")})
	writeFiles(t, d2, map[string][]byte{"app.log": []byte("hit d2\n")})
	if err := os.Link(filepath.Join(d1, "app.log.1"), filepath.Join(d2, "app.log.1")); err != nil {
		t.Fatal(err)
	}
	first, second := resolveIn(t, d1, "app.log"), resolveIn(t, d2, "app.log")
	second.DirInfo = first.DirInfo
	for i, part := range second.Parts {
		if part.Name == "app.log.1" {
			second.Parts[i].Info = statWithSize{FileInfo: part.Info, size: part.Info.Size() + 1}
		}
	}

	r := newSearchResolver(SearchRequest{})
	for _, c := range []Candidate{first, second} {
		if err := r.addChain(context.Background(), c, nil); err != nil {
			t.Fatalf("add the chain of %s: %v", c.Dir, err)
		}
	}
	r.finish()
	if want := partPaths(first, second); len(r.chains) != 1 || !slices.Equal(plannedPaths(r), want) || len(r.plan.Skipped) != 0 {
		t.Fatalf("chains %d, planned %v, skipped %v; want 1 chain and every part of both planned %v",
			len(r.chains), plannedPaths(r), r.plan.Skipped, want)
	}
}

// When the other encodings of two chains are links to one file named
// on its own, that file is named in skipped_files once, beside each
// encoding, whichever chain comes first.
func TestSearch_AFileTwoEncodingsLinkToIsNamedAsSkippedOnce(t *testing.T) {
	base := anEncodingThatIsALink(t, "A", "C")
	named := map[string]string{
		"A": filepath.Join(base, "A", "app.log"), "C": filepath.Join(base, "C", "app.log"),
		"Bz": filepath.Join(base, "B", "z.gz"),
	}
	chainRows := func(d string) []string {
		return []string{
			matchRow(d+"/app.log.1", string(sharedFileLines["A/app.log.1"]), d+"/app.log", 1),
			matchRow(d+"/app.log", string(sharedFileLines["A/app.log"]), d+"/app.log", 2),
		}
	}
	want := orderFreeAnswer{
		matches: slices.Sorted(slices.Values(slices.Concat(chainRows("A"), chainRows("C")))),
		skipped: []string{"A/app.log.1.gz " + ReasonDuplicatePart, "B/z.gz " + ReasonDuplicatePart,
			"C/app.log.1.gz " + ReasonDuplicatePart},
		chains: []string{"A/app.log: A/app.log.1, A/app.log", "C/app.log: C/app.log.1, C/app.log"},
	}
	requireOrderFreeAnswer(t, base, named, permutations([]string{"A", "C", "Bz"}), want)
}
