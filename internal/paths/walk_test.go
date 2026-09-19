package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/testutil/linktree"
)

// walkWithin builds the link tree, confines the sandbox to its root and
// walks the root recursively. It returns the tree and what the walk
// reported for each path.
func walkWithin(t *testing.T, includeHidden bool) (linktree.Tree, map[string]WalkEntry) {
	t.Helper()
	tree := linktree.Build(t)
	if err := SetSearchRoots([]string{tree.Root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(Reset)
	SetIncludeHidden(includeHidden)
	t.Cleanup(func() { SetIncludeHidden(false) })

	entries, err := WalkDir(tree.Root, true)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	byPath := map[string]WalkEntry{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	return tree, byPath
}

func TestWalkDir_RefusesWhatANamedPathCheckRefuses(t *testing.T) {
	tree, got := walkWithin(t, false)

	// "" means the walk reports the path as a file to read.
	want := map[string]string{
		tree.In:     "",
		tree.InLink: "",
		tree.Deep:   "",
		// sub is searched under its own path; the link to it is a
		// second way into a directory already searched.
		tree.SubDirLink: "directory already searched through '" + filepath.Dir(tree.Deep) + "'",
		tree.Out:        ReasonLinkOutsideRoots,
		tree.OutDir:     ReasonLinkOutsideRoots,
		tree.Visible:    ReasonLinkHidden,
		tree.Loop:       ReasonLinkLoop,
		tree.Self:       "cannot resolve symlink",
		tree.Dangling:   "cannot resolve symlink",
	}
	if len(got) != len(want) {
		t.Errorf("walk reported %d paths, want %d: %v", len(got), len(want), got)
	}
	for path, wantReason := range want {
		entry, ok := got[path]
		switch {
		case !ok:
			t.Errorf("%s not reported", path)
		case wantReason == "" && entry.Refused != "":
			t.Errorf("%s refused (%q), want it read", path, entry.Refused)
		case !strings.HasPrefix(entry.Refused, wantReason):
			t.Errorf("%s refused with %q, want %q", path, entry.Refused, wantReason)
		}
	}
}

func TestWalkDir_HiddenOnFollowsALinkIntoAHiddenDirectory(t *testing.T) {
	tree, got := walkWithin(t, true)

	for _, path := range []string{tree.Visible, tree.Private} {
		if entry, ok := got[path]; !ok || entry.Refused != "" {
			t.Errorf("%s: reported %v (present %v), want it read with hidden entries on", path, entry, ok)
		}
	}
}

// Two directories whose links point at each other: each is searched
// once, under its own path, and each link is a second way into a
// directory already searched, so the walk ends.
func TestWalkDir_DirectoryLinksThatPointAtEachOtherEnd(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for _, dir := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(base, dir, "x.log"), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "b"), filepath.Join(base, "a", "to-b")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "a"), filepath.Join(base, "b", "to-a")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	Reset()

	// WalkDir runs in a goroutine so the test can stop waiting after a
	// timeout; the channel has room for one result, so the goroutine
	// never blocks on a send nobody receives.
	done := make(chan []WalkEntry, 1)
	go func() {
		entries, _ := WalkDir(base, true)
		done <- entries
	}()
	var entries []WalkEntry
	select {
	case entries = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WalkDir did not end on directory links that point at each other")
	}

	files, secondWays := 0, 0
	for _, entry := range entries {
		switch {
		case entry.Refused == "":
			files++
		case strings.HasPrefix(entry.Refused, "directory already searched through"):
			secondWays++
		}
	}
	// a/x.log and b/x.log; a/to-b and b/to-a are refused.
	if files != 2 || secondWays != 2 {
		t.Errorf("walk reported %d files and %d second ways in, want 2 and 2: %v", files, secondWays, entries)
	}
}

func TestWalkDir_NonRecursiveNeverReportsADirectory(t *testing.T) {
	tree := linktree.Build(t)
	Reset()

	entries, err := WalkDir(tree.Root, false)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Path == tree.SubDirLink || entry.Path == tree.OutDir || entry.Path == tree.Loop {
			t.Errorf("non-recursive walk reported the directory link %s (%q)", entry.Path, entry.Refused)
		}
	}
}

func TestWalkDir_KeepsTheCallersSpellingOfTheDirectory(t *testing.T) {
	tree := linktree.Build(t)
	Reset()

	entries, err := WalkDir(tree.Root+string(filepath.Separator), false)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Path, string(filepath.Separator)+string(filepath.Separator)) {
			t.Errorf("walk reported %s with a doubled separator", entry.Path)
		}
	}
}

// walkWithBudget runs WalkDir in a goroutine and fails the test when it
// takes longer than budget, so a walk whose work grows exponentially
// fails instead of hanging the test run. The channel has room for one
// result, so the goroutine can finish and exit even after the test has
// stopped waiting for it.
func walkWithBudget(t *testing.T, dir string, budget time.Duration) []WalkEntry {
	t.Helper()
	type result struct {
		entries []WalkEntry
		err     error
	}
	done := make(chan result, 1)
	go func() {
		entries, err := WalkDir(dir, true)
		done <- result{entries, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("WalkDir: %v", r.err)
		}
		return r.entries
	case <-time.After(budget):
		t.Fatalf("WalkDir of %s did not finish in %s", dir, budget)
		return nil
	}
}

// Six levels of six links to the next level: following every link would
// reach the last directory 6^6 = 46,656 times. Each directory is walked
// once, so the file is reported once and every other way into a
// directory is reported as refused, with the path it was searched under.
func TestWalkDir_FanOutOfDirectoryLinksWalksEachDirectoryOnce(t *testing.T) {
	fan := linktree.BuildFanOut(t, 6, 6)
	if err := SetSearchRoots([]string{fan.Root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(Reset)

	entries := walkWithBudget(t, fan.Top, 10*time.Second)

	var files []string
	refused := 0
	for _, entry := range entries {
		switch {
		case entry.Refused == "":
			files = append(files, entry.Path)
		case strings.Contains(entry.Refused, "already searched through '"):
			refused++
		default:
			t.Errorf("%s refused for an unexpected reason: %q", entry.Path, entry.Refused)
		}
	}
	if len(files) != 1 || filepath.Base(files[0]) != "f.log" {
		t.Errorf("walk reported %d files, want f.log once", len(files))
	}
	// One link per level is followed; the other five are refused.
	if want := fan.Levels * (fan.Links - 1); refused != want {
		t.Errorf("walk refused %d links as already searched, want %d", refused, want)
	}
}

// A directory reached both directly and through a link is searched once,
// under its own path, whatever the order of the names: the link to it
// is reported with the path the directory was searched under.
func TestWalkDir_DirectoryReachedDirectlyAndThroughALinkIsSearchedOnce(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for _, dir := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "b", "x.log"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// "a" sorts before "b": the walk meets the link before the directory.
	if err := os.Symlink(filepath.Join("..", "b"), filepath.Join(base, "a", "to-b")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := SetSearchRoots([]string{base}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(Reset)

	entries := walkWithBudget(t, base, 10*time.Second)

	byPath := map[string]WalkEntry{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	direct := filepath.Join(base, "b", "x.log")
	if entry, ok := byPath[direct]; !ok || entry.Refused != "" {
		t.Errorf("%s: reported %+v (present %v), want it read", direct, entry, ok)
	}
	if entry, ok := byPath[filepath.Join(base, "a", "to-b", "x.log")]; ok {
		t.Errorf("the file was reported a second time through the link: %+v", entry)
	}
	link := filepath.Join(base, "a", "to-b")
	wantReason := "already searched through '" + filepath.Join(base, "b") + "'"
	if entry := byPath[link]; !strings.Contains(entry.Refused, wantReason) {
		t.Errorf("%s refused with %q, want a reason containing %q", link, entry.Refused, wantReason)
	}
}
