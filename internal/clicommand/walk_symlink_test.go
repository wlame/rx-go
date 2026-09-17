package clicommand

// A directory search may only read what naming the same path would let
// it read. A symbolic link inside a search root is followed when its
// target passes the checks a named path gets — inside a root, and not
// hidden unless --hidden — and is listed as skipped otherwise. These
// tests hold `rx trace` and `rx index` to that rule over one tree whose
// links lead out of the root, into a hidden directory, to an ancestor,
// to themselves and to nothing.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/linktree"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// sandboxedLinkTree builds the link tree and confines rx to its root,
// with hidden entries off, as `rx --search-root=<root>` does.
func sandboxedLinkTree(t *testing.T) linktree.Tree {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	tree := linktree.Build(t)
	if err := paths.SetSearchRoots([]string{tree.Root}); err != nil {
		t.Fatalf("set search roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	paths.SetIncludeHidden(false)
	return tree
}

// traceDirJSON runs `rx trace NEEDLE <dir> --json` plus extra flags and
// decodes the answer. A walk that never ends fails the test instead of
// hanging it.
func traceDirJSON(t *testing.T, dir string, extra ...string) rxtypes.TraceResponse {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs(append([]string{"NEEDLE", dir, "--json"}, extra...))

	// The command runs in a goroutine so the test can give up on it
	// after a timeout; the buffered channel lets the goroutine finish
	// and exit even when nobody is left to receive its result.
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("rx trace on the directory did not finish in 30 s")
	}

	var resp rxtypes.TraceResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	return resp
}

// searchedFiles lists the files of a trace answer, sorted.
func searchedFiles(resp rxtypes.TraceResponse) []string {
	files := make([]string, 0, len(resp.Files))
	for _, path := range resp.Files {
		files = append(files, path)
	}
	slices.Sort(files)
	return files
}

// matchedTexts lists the line text of every match, sorted.
func matchedTexts(resp rxtypes.TraceResponse) []string {
	texts := make([]string, 0, len(resp.Matches))
	for _, m := range resp.Matches {
		if m.LineText != nil {
			texts = append(texts, *m.LineText)
		}
	}
	slices.Sort(texts)
	return texts
}

func sorted(values ...string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func TestTraceCommand_DirectoryWalkStaysInsideTheSearchRoot(t *testing.T) {
	tree := sandboxedLinkTree(t)

	resp := traceDirJSON(t, tree.Root)

	wantFiles := sorted(tree.In, tree.InLink, tree.Deep, tree.DeepByLink)
	if got := searchedFiles(resp); !slices.Equal(got, wantFiles) {
		t.Errorf("searched %v, want %v", got, wantFiles)
	}
	wantTexts := []string{
		"LINE 1 deep NEEDLE", "LINE 1 deep NEEDLE",
		"LINE 1 inside NEEDLE", "LINE 1 inside NEEDLE",
	}
	if got := matchedTexts(resp); !slices.Equal(got, wantTexts) {
		t.Errorf("matched %v, want %v", got, wantTexts)
	}
	// Every link the walk refused is reported, so a caller can see that
	// part of the tree was not searched.
	wantSkipped := sorted(tree.Out, tree.OutDir, tree.Visible, tree.Loop, tree.Self, tree.Dangling)
	if got := sorted(resp.SkippedFiles...); !slices.Equal(got, wantSkipped) {
		t.Errorf("skipped %v, want %v", got, wantSkipped)
	}
}

// With --no-recursive a directory is not entered, and a link to one is
// a directory too: it is neither searched nor listed.
func TestTraceCommand_NoRecursiveWalkStaysInsideTheSearchRoot(t *testing.T) {
	tree := sandboxedLinkTree(t)

	resp := traceDirJSON(t, tree.Root, "--no-recursive")

	wantFiles := sorted(tree.In, tree.InLink)
	if got := searchedFiles(resp); !slices.Equal(got, wantFiles) {
		t.Errorf("searched %v, want %v", got, wantFiles)
	}
	wantTexts := []string{"LINE 1 inside NEEDLE", "LINE 1 inside NEEDLE"}
	if got := matchedTexts(resp); !slices.Equal(got, wantTexts) {
		t.Errorf("matched %v, want %v", got, wantTexts)
	}
	wantSkipped := sorted(tree.Out, tree.Visible, tree.Self, tree.Dangling)
	if got := sorted(resp.SkippedFiles...); !slices.Equal(got, wantSkipped) {
		t.Errorf("skipped %v, want %v", got, wantSkipped)
	}
}

// --hidden lets a walk into hidden entries, through a link as by name.
func TestTraceCommand_HiddenFlagLetsTheWalkFollowALinkIntoAHiddenDirectory(t *testing.T) {
	tree := sandboxedLinkTree(t)
	paths.SetIncludeHidden(true)
	t.Cleanup(func() { paths.SetIncludeHidden(false) })

	resp := traceDirJSON(t, tree.Root, "--no-recursive")

	if got := searchedFiles(resp); !slices.Contains(got, tree.Visible) {
		t.Errorf("searched %v, want %s among them with --hidden", got, tree.Visible)
	}
	if got := matchedTexts(resp); !slices.Contains(got, "LINE 1 PRIVATE NEEDLE") {
		t.Errorf("matched %v, want the line behind %s with --hidden", got, tree.Visible)
	}
}

// Without --search-root there is no root to stay inside: a walk follows
// a link wherever it leads, as naming the link would.
func TestTraceCommand_WalkWithoutASandboxFollowsLinksOutOfTheDirectory(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	tree := linktree.Build(t)
	paths.Reset()

	resp := traceDirJSON(t, tree.Root, "--no-recursive")

	if got := matchedTexts(resp); !slices.Contains(got, "LINE 1 SECRET NEEDLE") {
		t.Errorf("matched %v, want the line behind %s without a sandbox", got, tree.Out)
	}
	// A link to a directory is still not a file.
	if got := searchedFiles(resp); slices.Contains(got, tree.OutDir) || slices.Contains(got, tree.Loop) {
		t.Errorf("searched %v: a link to a directory was taken for a file", got)
	}
}

// indexedPaths lists the source paths of an `rx index --json` answer.
func indexedPaths(t *testing.T, result map[string]any) []string {
	t.Helper()
	entries, _ := result["indexed"].([]any)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		fields, _ := entry.(map[string]any)
		path, _ := fields["path"].(string)
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// skipReasons maps each skipped path of an `rx index --json` answer to
// its reason.
func skipReasons(result map[string]any) map[string]string {
	out := map[string]string{}
	items, _ := result["skip_reasons"].([]any)
	for _, item := range items {
		fields, _ := item.(map[string]any)
		path, _ := fields["path"].(string)
		reason, _ := fields["reason"].(string)
		out[path] = reason
	}
	return out
}

// requireNoIndexFor fails when the index cache holds an entry for any
// of the given paths.
func requireNoIndexFor(t *testing.T, sources ...string) {
	t.Helper()
	for _, source := range sources {
		if _, err := os.Stat(index.GetCachePath(source)); err == nil {
			t.Errorf("an index was stored for %s", source)
		}
	}
}

func TestIndexCommand_RecursiveWalkStaysInsideTheSearchRoot(t *testing.T) {
	tree := sandboxedLinkTree(t)
	zero := 0

	result := runIndexJSON(t, indexParams{
		paths: []string{tree.Root}, recursive: true, threshold: &zero,
	})

	wantIndexed := sorted(tree.In, tree.InLink, tree.Deep, tree.DeepByLink)
	if got := indexedPaths(t, result); !slices.Equal(got, wantIndexed) {
		t.Errorf("indexed %v, want %v", got, wantIndexed)
	}
	reasons := skipReasons(result)
	for path, wantWord := range map[string]string{
		tree.Out:      "outside",
		tree.OutDir:   "outside",
		tree.Visible:  ".private",
		tree.Loop:     "loop",
		tree.Self:     "resolve",
		tree.Dangling: "resolve",
	} {
		if reason, ok := reasons[path]; !ok || !strings.Contains(reason, wantWord) {
			t.Errorf("skip reason for %s is %q, want one that says %q", path, reason, wantWord)
		}
	}
	requireNoIndexFor(t, tree.Out, tree.Secret, tree.Secret2, tree.Visible, tree.Private)
}

func TestIndexCommand_NonRecursiveWalkNeverTakesALinkedDirectoryForAFile(t *testing.T) {
	tree := sandboxedLinkTree(t)
	zero := 0

	result := runIndexJSON(t, indexParams{paths: []string{tree.Root}, threshold: &zero})

	wantIndexed := sorted(tree.In, tree.InLink)
	if got := indexedPaths(t, result); !slices.Equal(got, wantIndexed) {
		t.Errorf("indexed %v, want %v", got, wantIndexed)
	}
	reasons := skipReasons(result)
	for _, dir := range []string{tree.OutDir, tree.SubDirLink, tree.Loop} {
		if reason, ok := reasons[dir]; ok {
			t.Errorf("the directory link %s was listed as a skipped file (%q)", dir, reason)
		}
	}
	requireNoIndexFor(t, tree.Out, tree.Secret, tree.Visible, tree.Private)
}
