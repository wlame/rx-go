package frontend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// localBuildVersion is the stamp a local `just build` of rx-viewer
// writes into version.json: a describe string, not a release version.
const localBuildVersion = "v0.6.0-6-g03013b5"

// writeLocalBuild fills dir with a viewer build as `just build` leaves
// it: index.html, assets/ and version.json, and no .metadata.json.
func writeLocalBuild(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"index.html":    "<html>local build</html>",
		"assets/app.js": "console.log('local')",
		"version.json":  `{"version": "` + localBuildVersion + `", "buildDate": "2026-10-06T17:02:01Z", "commit": "03013b5"}`,
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// treeDigest maps every entry under dir to a digest of its content ("dir"
// for a directory), so a test sees a file changed, added or removed. A
// missing dir has the nil map; an existing one always holds ".".
func treeDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		tree[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// localPathManager returns a manager whose directory comes from
// RX_FRONTEND_PATH, as `rx serve` builds it, asking apiBase for releases.
func localPathManager(t *testing.T, dir, apiBase string) *Manager {
	t.Helper()
	t.Setenv("RX_FRONTEND_PATH", dir)
	t.Setenv("RX_FRONTEND_URL", "")
	t.Setenv("RX_FRONTEND_VERSION", "")
	return NewManager(Config{APIBase: apiBase, Logger: silentLog()})
}

// requireNothingAsked fails when the fake GitHub saw any request.
func requireNothingAsked(t *testing.T, gh *fakeGitHub) {
	t.Helper()
	if lists, downloads := gh.listHits.Load(), gh.downloadHits.Load(); lists != 0 || downloads != 0 {
		t.Errorf("GitHub asked %d times for releases and %d for a bundle, want none", lists, downloads)
	}
}

// RX_FRONTEND_PATH names a directory the operator manages, such as a
// local build of rx-viewer: it is served as it is, whatever GitHub
// publishes and whatever the check would say.
func TestEnsure_LocalPathIsServedAsItIs(t *testing.T) {
	gh := newFakeGitHub(t, []fakeRelease{{tag: "v0.6.0"}})
	dir := t.TempDir()
	writeLocalBuild(t, dir)
	before := treeDigest(t, dir)
	m := localPathManager(t, dir, gh.srv.URL)
	want := Served{Version: localBuildVersion, Reason: ServedLocalPath, Dir: dir}

	for name, run := range map[string]func(context.Context) (Served, error){
		"Ensure": m.Ensure,
		"Update": m.Update,
	} {
		served, err := run(context.Background())
		if err != nil || served != want {
			t.Errorf("%s = %+v, %v; want %+v, nil", name, served, err, want)
		}
	}
	if got := m.Cached(); got != want {
		t.Errorf("Cached = %+v, want %+v", got, want)
	}
	requireNothingAsked(t, gh)
	if after := treeDigest(t, dir); !maps.Equal(before, after) {
		t.Errorf("the directory changed:\nbefore %v\nafter  %v", before, after)
	}
	if !m.IsAvailable() {
		t.Error("the local build must be served")
	}
}

// A local path that holds no build serves no viewer: nothing is
// downloaded into it, and a missing directory is not created.
func TestEnsure_LocalPathWithoutABuildServesNoViewer(t *testing.T) {
	for name, dir := range map[string]string{
		"empty":   t.TempDir(),
		"missing": filepath.Join(t.TempDir(), "not-there"),
	} {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t, []fakeRelease{{tag: "v0.6.0"}})
			before := treeDigest(t, dir)
			m := localPathManager(t, dir, gh.srv.URL)

			served, err := m.Ensure(context.Background())

			if served.Reason != ServedNone {
				t.Errorf("served = %+v, want none", served)
			}
			if err == nil || !strings.Contains(err.Error(), "RX_FRONTEND_PATH "+dir+" holds no viewer build") {
				t.Errorf("err = %v, want one naming RX_FRONTEND_PATH and the directory", err)
			}
			requireNothingAsked(t, gh)
			if after := treeDigest(t, dir); !maps.Equal(before, after) {
				t.Errorf("the directory changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// RX_FRONTEND_URL and RX_FRONTEND_VERSION would download into the
// directory; beside RX_FRONTEND_PATH they are ignored, with a warning.
func TestEnsure_LocalPathIgnoresTheDownloadOverrides(t *testing.T) {
	gh := newFakeGitHub(t, []fakeRelease{{tag: "v0.6.0"}})
	dir := t.TempDir()
	writeLocalBuild(t, dir)
	before := treeDigest(t, dir)
	t.Setenv("RX_FRONTEND_PATH", dir)
	t.Setenv("RX_FRONTEND_URL", gh.srv.URL+"/download/v0.6.0/dist.tar.gz")
	t.Setenv("RX_FRONTEND_VERSION", "v0.6.0")
	m := NewManager(Config{APIBase: gh.srv.URL, Logger: silentLog()})

	served, err := m.Ensure(context.Background())

	if served.Reason != ServedLocalPath {
		t.Errorf("served = %+v, want the local build", served)
	}
	if err == nil || !strings.Contains(err.Error(), "RX_FRONTEND_URL and RX_FRONTEND_VERSION are ignored") {
		t.Errorf("err = %v, want one naming the ignored variables", err)
	}
	requireNothingAsked(t, gh)
	if after := treeDigest(t, dir); !maps.Equal(before, after) {
		t.Errorf("the directory changed:\nbefore %v\nafter  %v", before, after)
	}
}
