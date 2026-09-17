package webapi

// Over HTTP a directory search may only read what naming the same path
// would let it read, exactly as on the CLI: a symbolic link is followed
// when its target is inside a search root and not hidden, and listed as
// skipped otherwise. The tree listing leaves out the links a caller
// could not open by name.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/linktree"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// sandboxedLinkTree builds the link tree and serves its root, with
// hidden entries off.
func sandboxedLinkTree(t *testing.T) linktree.Tree {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	tree := linktree.Build(t)
	if err := paths.SetSearchRoots([]string{tree.Root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	paths.SetIncludeHidden(false)
	return tree
}

func sortedStrings(values ...string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func TestTrace_DirectoryWalkStaysInsideTheSearchRoot(t *testing.T) {
	tree := sandboxedLinkTree(t)
	ts := newServerWithRipgrep(t)

	cases := []struct {
		name        string
		query       url.Values
		wantFiles   []string
		wantSkipped []string
		wantMatches int
	}{
		{
			name:        "recursive",
			query:       url.Values{"path": {tree.Root}, "regexp": {"NEEDLE"}},
			wantFiles:   sortedStrings(tree.In, tree.InLink, tree.Deep),
			wantSkipped: sortedStrings(tree.Out, tree.OutDir, tree.Visible, tree.Loop, tree.Self, tree.Dangling, tree.SubDirLink),
			wantMatches: 3,
		},
		{
			name:        "no_recursive",
			query:       url.Values{"path": {tree.Root}, "regexp": {"NEEDLE"}, "no_recursive": {"true"}},
			wantFiles:   sortedStrings(tree.In, tree.InLink),
			wantSkipped: sortedStrings(tree.Out, tree.Visible, tree.Self, tree.Dangling),
			wantMatches: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := getTrace(t, ts.URL, tc.query)

			files := []string{}
			for _, path := range body.Files {
				files = append(files, path)
			}
			slices.Sort(files)
			if !slices.Equal(files, tc.wantFiles) {
				t.Errorf("searched %v, want %v", files, tc.wantFiles)
			}
			if got := sortedStrings(body.SkippedFiles...); !slices.Equal(got, tc.wantSkipped) {
				t.Errorf("skipped %v, want %v", got, tc.wantSkipped)
			}
			if len(body.Matches) != tc.wantMatches {
				t.Errorf("got %d matches, want %d", len(body.Matches), tc.wantMatches)
			}
			for _, m := range body.Matches {
				text := ""
				if m.LineText != nil {
					text = *m.LineText
				}
				if text != "LINE 1 inside NEEDLE" && text != "LINE 1 deep NEEDLE" {
					t.Errorf("the walk returned %q from %s", text, body.Files[m.File])
				}
			}
		})
	}
}

// POST /v1/index takes one file. A directory is refused, and no index is
// stored for anything a link inside it leads to.
func TestIndexPost_DirectoryStoresNoIndexBehindItsLinks(t *testing.T) {
	tree := sandboxedLinkTree(t)
	ts := newTestServer(t)
	zero := 0
	payload, _ := json.Marshal(rxtypes.IndexRequest{Path: tree.Root, Threshold: &zero})

	resp, err := http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a directory", resp.StatusCode)
	}
	for _, source := range []string{tree.Out, tree.Secret, tree.Secret2, tree.Visible, tree.Private} {
		if _, err := os.Stat(index.GetCachePath(source)); err == nil {
			t.Errorf("an index was stored for %s", source)
		}
	}
}

// The listing shows what a caller can open by name: a link to a
// directory inside the root is a directory, and a link that leads out of
// the root, into a hidden entry or nowhere is not listed.
func TestTree_ListsOnlyTheLinksACallerCanOpen(t *testing.T) {
	tree := sandboxedLinkTree(t)
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/tree?path=" + url.QueryEscape(tree.Root))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body rxtypes.TreeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	types := map[string]string{}
	for _, entry := range body.Entries {
		types[entry.Name] = entry.Type
	}
	want := map[string]string{
		"in.log":     "file",
		"inlink.log": "file",
		"sub":        "directory",
		"subdirlink": "directory",
		"loop":       "directory",
	}
	if len(types) != len(want) {
		t.Errorf("listed %v, want %v", types, want)
	}
	for name, wantType := range want {
		if types[name] != wantType {
			t.Errorf("%s listed as %q, want %q (listing: %v)", name, types[name], wantType, types)
		}
	}
	if body.TotalEntries != len(want) {
		t.Errorf("total_entries %d, want %d", body.TotalEntries, len(want))
	}
}

// fanOutBudget bounds one request over the fan-out tree: six levels of
// six links to the next level, 6^6 = 46,656 paths to one file when every
// link is followed the way it is met.
const fanOutBudget = 10 * time.Second

// sandboxedFanOut builds the fan-out tree and serves its root.
func sandboxedFanOut(t *testing.T) linktree.FanOut {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	fan := linktree.BuildFanOut(t, 6, 6)
	if err := paths.SetSearchRoots([]string{fan.Root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return fan
}

// getWithBudget GETs url and decodes the 200 body into out, failing the
// test when the answer takes longer than fanOutBudget.
func getWithBudget(t *testing.T, url string, out any) {
	t.Helper()
	client := &http.Client{Timeout: fanOutBudget}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestTrace_FanOutOfDirectoryLinksSearchesEachDirectoryOnce(t *testing.T) {
	fan := sandboxedFanOut(t)
	ts := newServerWithRipgrep(t)

	var body rxtypes.TraceResponse
	query := url.Values{"path": {fan.Top}, "regexp": {"NEEDLE"}}
	getWithBudget(t, ts.URL+"/v1/trace?"+query.Encode(), &body)

	if len(body.Matches) != 1 || len(body.Files) != 1 {
		t.Errorf("%d matches in %d files, want f.log searched once", len(body.Matches), len(body.Files))
	}
	if want := fan.Levels * (fan.Links - 1); len(body.SkippedFiles) != want {
		t.Errorf("skipped %d paths, want %d", len(body.SkippedFiles), want)
	}
}

// The tree lists one level per request: every link of the level is a
// way to browse into the next one, so all six are listed, each with the
// six links of the level below as its children.
func TestTree_FanOutOfDirectoryLinksListsOneLevel(t *testing.T) {
	fan := sandboxedFanOut(t)
	ts := newTestServer(t)

	var body rxtypes.TreeResponse
	getWithBudget(t, ts.URL+"/v1/tree?path="+url.QueryEscape(fan.Top), &body)

	if len(body.Entries) != fan.Links {
		t.Fatalf("listed %d entries, want %d", len(body.Entries), fan.Links)
	}
	for _, entry := range body.Entries {
		if entry.Type != "directory" || entry.ChildrenCount == nil || *entry.ChildrenCount != fan.Links {
			t.Errorf("%s listed as %q with children %v, want a directory of %d", entry.Name, entry.Type, entry.ChildrenCount, fan.Links)
		}
	}
}
