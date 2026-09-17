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
			wantFiles:   sortedStrings(tree.In, tree.InLink, tree.Deep, tree.DeepByLink),
			wantSkipped: sortedStrings(tree.Out, tree.OutDir, tree.Visible, tree.Loop, tree.Self, tree.Dangling),
			wantMatches: 4,
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
