package webapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/frontend"
	"github.com/wlame/rx-go/internal/tasks"
)

// A viewer build named by RX_FRONTEND_PATH is served as the build left
// it, with no metadata of its own: / answers its index.html and a
// static file answers its bytes.
func TestStatic_LocalPathBuildIsServedAsItIs(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"index.html":    "<html>local build</html>",
		"assets/app.js": "console.log('local')",
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
	t.Setenv("RX_FRONTEND_PATH", dir)
	t.Setenv("RX_FRONTEND_URL", "")
	t.Setenv("RX_FRONTEND_VERSION", "")
	fm := frontend.NewManager(frontend.Config{})
	if _, err := fm.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	ts := httptest.NewServer(NewServer(Config{
		AppVersion:  "unit-test",
		Frontend:    fm,
		TaskManager: tasks.New(tasks.Config{}),
	}))
	t.Cleanup(ts.Close)

	for urlPath, want := range map[string]string{
		"/":              files["index.html"],
		"/assets/app.js": files["assets/app.js"],
	} {
		resp, err := http.Get(ts.URL + urlPath)
		if err != nil {
			t.Fatalf("GET %s: %v", urlPath, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", urlPath, err)
		}
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Errorf("GET %s = %d %q, want 200 %q", urlPath, resp.StatusCode, body, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".metadata.json")); !os.IsNotExist(err) {
		t.Errorf(".metadata.json appeared in the directory (stat error %v)", err)
	}
}
