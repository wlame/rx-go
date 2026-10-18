package webapi

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/webapi/assets"
)

// docsPagePaths are the two documentation pages rx serves itself.
var docsPagePaths = []string{"/docs", "/redoc"}

// referencedURLs lists the src= and href= values of an HTML body.
var referencedURLs = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// getBody GETs path from ts and returns the status, the headers and the body.
func getBody(t *testing.T, base, path string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// The pages reference nothing but this origin, so they render on a
// network with no route to the internet: every src= and href= is a
// same-origin path, and no scheme or host appears in either body.
func TestDocsPages_ReferenceOnlyThisOrigin(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range docsPagePaths {
		status, headers, body := getBody(t, ts.URL, path)
		if status != http.StatusOK || !strings.HasPrefix(headers.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: status %d, content-type %q", path, status, headers.Get("Content-Type"))
		}
		refs := referencedURLs.FindAllStringSubmatch(body, -1)
		if len(refs) == 0 {
			t.Errorf("%s references no file at all: %s", path, body)
		}
		for _, ref := range refs {
			if !strings.HasPrefix(ref[1], "/") || strings.HasPrefix(ref[1], "//") {
				t.Errorf("%s references %q, want a same-origin path", path, ref[1])
			}
		}
		if strings.Contains(body, "://") {
			t.Errorf("%s names a scheme or host: %s", path, body)
		}
	}
}

// Each file a page references answers 200 from rx with its content type,
// and the bytes are the embedded asset's.
func TestDocsPages_AssetsAreServedWithTheirContentType(t *testing.T) {
	ts := newTestServer(t)

	seen := 0
	for _, path := range docsPagePaths {
		_, _, body := getBody(t, ts.URL, path)
		for _, ref := range referencedURLs.FindAllStringSubmatch(body, -1) {
			name, ok := docsAssetRoutes[ref[1]]
			if !ok {
				t.Errorf("%s references %q, which no asset route serves", path, ref[1])
				continue
			}
			seen++
			status, headers, got := getBody(t, ts.URL, ref[1])
			file, _ := assets.Lookup(name)
			if status != http.StatusOK || headers.Get("Content-Type") != file.ContentType {
				t.Errorf("%s: status %d, content-type %q, want 200 %q", ref[1], status, headers.Get("Content-Type"), file.ContentType)
			}
			if assets.Digest([]byte(got)) != file.SHA256 {
				t.Errorf("%s: served bytes differ from the embedded %s", ref[1], name)
			}
			if headers.Get("ETag") == "" {
				t.Errorf("%s: no ETag", ref[1])
			}
		}
	}
	if seen != len(docsAssetRoutes) {
		t.Errorf("the pages reference %d assets, %d are embedded", seen, len(docsAssetRoutes))
	}
}

// A browser that holds an asset asks again with If-None-Match and is
// told it is current.
func TestDocsPages_AssetsRevalidateByETag(t *testing.T) {
	ts := newTestServer(t)

	_, headers, _ := getBody(t, ts.URL, "/docs/swagger-ui.css")
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/docs/swagger-ui.css", nil)
	req.Header.Set("If-None-Match", headers.Get("ETag"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation: status %d, want 304", resp.StatusCode)
	}
}

// The pages' policies allow this origin for scripts and styles and the
// one inline bootstrap script of the Swagger UI page by its hash, name
// no external host, and deny framing like every rx response.
func TestDocsPages_PolicyNamesNoExternalHost(t *testing.T) {
	ts := newTestServer(t)

	if got := cspHashOf(swaggerBootstrap); got != swaggerBootstrapHash {
		t.Errorf("the bootstrap script's hash is %s, the policy names %s", got, swaggerBootstrapHash)
	}
	for _, path := range docsPagePaths {
		_, headers, _ := getBody(t, ts.URL, path)
		policy := headers.Get("Content-Security-Policy")
		for _, want := range []string{"script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "default-src 'none'"} {
			if !strings.Contains(policy, want) {
				t.Errorf("%s: policy %q lacks %q", path, policy, want)
			}
		}
		if strings.Contains(policy, "http") {
			t.Errorf("%s: policy names a host: %q", path, policy)
		}
	}
	_, headers, _ := getBody(t, ts.URL, "/docs")
	if !strings.Contains(headers.Get("Content-Security-Policy"), swaggerBootstrapHash) {
		t.Errorf("/docs: policy %q does not allow the bootstrap script", headers.Get("Content-Security-Policy"))
	}
}
