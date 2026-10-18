package webapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wlame/rx-go/internal/webapi/assets"
)

// The two documentation pages, /docs (Swagger UI) and /redoc (ReDoc),
// are served by rx itself from embedded files (internal/webapi/assets),
// so they render on a network with no route to the internet. huma's
// own /docs renderer is switched off (DocsPath is empty in
// newHumaConfig): its page loaded the Swagger UI files from a CDN and
// named that host in its policy. The pages here reference nothing but
// this origin, and say so in a Content-Security-Policy of their own
// that also denies framing, as every rx response does.

// swaggerBootstrap is the inline script of the Swagger UI page: it
// reads the spec URL from its own data attribute and starts the UI once
// the bundle has loaded. Its text is the one huma ships, byte for byte,
// so the hash the policy names for it (swaggerBootstrapHash) is huma's.
const swaggerBootstrap = `
      const script = document.currentScript;
      const url = script.dataset.url;
      const config = script.dataset.config ? JSON.parse(script.dataset.config) : {};
      window.onload = () => {
        window.ui = SwaggerUIBundle({
          ...config,
          url: url,
          dom_id: '#swagger-ui',
        });
      };
    `

// swaggerBootstrapHash is the CSP source expression that allows
// swaggerBootstrap and no other inline script: 'sha256-<base64 of the
// script text>'. A test recomputes it from swaggerBootstrap.
const swaggerBootstrapHash = "'sha256-gRya58TMnKTH/Tne/zBInjBwFUxL66aMDYvPuAX0lNY='"

// docsPolicy is the Content-Security-Policy of the Swagger UI page:
// huma's for the same page, with its CDN host replaced by this origin.
// Scripts are the bundle from this origin and the one inline bootstrap
// (by hash); styles are the stylesheet from this origin; the UI fetches
// /openapi.json from this origin (connect-src). Nothing else loads.
var docsPolicy = strings.Join([]string{
	"default-src 'none'",
	"base-uri 'none'",
	"connect-src 'self'",
	"form-action 'none'",
	"frame-ancestors 'none'",
	"sandbox allow-same-origin allow-scripts allow-popups allow-popups-to-escape-sandbox allow-forms allow-downloads",
	"script-src 'self' " + swaggerBootstrapHash,
	"style-src 'self'",
	"img-src 'self' data:",
}, "; ")

// redocPolicy is the Content-Security-Policy of the ReDoc page. The
// bundle comes from this origin; ReDoc writes its styles into the
// document as it renders (styled-components), which takes
// 'unsafe-inline' for styles, draws images and logos from data: URLs
// and runs its search in a worker built from a blob: URL. No external
// host is named: the Google Fonts link the page used to carry is gone,
// and ReDoc falls back to the system fonts.
var redocPolicy = strings.Join([]string{
	"default-src 'none'",
	"base-uri 'none'",
	"connect-src 'self'",
	"form-action 'none'",
	"frame-ancestors 'none'",
	"script-src 'self' blob:",
	"style-src 'self' 'unsafe-inline'",
	"img-src 'self' data:",
	"font-src 'self' data:",
	"worker-src blob:",
	"child-src blob:",
}, "; ")

// swaggerPage is the /docs page: huma's Swagger UI page with the
// stylesheet and the bundle served from this origin.
const swaggerPage = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="referrer" content="no-referrer">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>rx-tool API docs (Swagger UI)</title>
    <link rel="stylesheet" href="/docs/swagger-ui.css">
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="/docs/swagger-ui-bundle.js"></script>
    <script data-url="/openapi.json">` + swaggerBootstrap + `</script>
  </body>
</html>
`

// redocPage is the /redoc page: ReDoc's standalone bundle served from
// this origin, reading the spec from this origin.
const redocPage = `<!DOCTYPE html>
<html>
  <head>
    <title>rx-tool API docs (ReDoc)</title>
    <meta charset="utf-8"/>
    <meta name="referrer" content="no-referrer">
    <meta name="viewport" content="width=device-width, initial-scale=1"/>
    <style>body{margin:0;padding:0;}</style>
  </head>
  <body>
    <redoc spec-url="/openapi.json"></redoc>
    <script src="/redoc/redoc.standalone.js"></script>
  </body>
</html>
`

// docsAssetRoutes maps the URL of each asset the pages reference to
// its name in the embedded files: the Swagger UI files under /docs/,
// ReDoc's under /redoc/. Both prefixes are reserved API paths, so the
// SPA catch-all never answers for them, and neither is under /v1, so
// the API token does not guard them, as it does not guard the pages.
var docsAssetRoutes = map[string]string{
	"/docs/swagger-ui.css":       "swagger-ui.css",
	"/docs/swagger-ui-bundle.js": "swagger-ui-bundle.js",
	"/redoc/redoc.standalone.js": "redoc.standalone.js",
}

// registerDocsPages mounts /docs, /redoc and the asset routes of
// docsAssetRoutes on r.
func registerDocsPages(r chi.Router) {
	r.Get("/docs", servePage([]byte(swaggerPage), docsPolicy))
	r.Get("/redoc", servePage([]byte(redocPage), redocPolicy))
	for route, name := range docsAssetRoutes {
		r.Get(route, serveAsset(name))
	}
}

// servePage answers an HTML page with policy as its
// Content-Security-Policy. The policy is set here, after the middleware
// has set the response-wide headers, so this page's fuller policy
// replaces the `frame-ancestors 'none'` every response carries (and
// repeats it).
func servePage(page []byte, policy string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", policy)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}
}

// serveAsset answers the embedded asset named name with its content
// type and its digest as the ETag, so a browser that holds it asks
// again with If-None-Match and gets 304 until a release changes the
// file. http.ServeContent handles the precondition headers; the zero
// time means no Last-Modified, the ETag alone decides.
func serveAsset(name string) http.HandlerFunc {
	file, _ := assets.Lookup(name)
	data, err := assets.Read(name)
	if err != nil {
		// The names come from docsAssetRoutes and are checked by the
		// package's test; a miss here is a build error, not a request's.
		panic(err)
	}
	etag := `"` + file.SHA256[:16] + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", file.ContentType)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	}
}

// cspHashOf returns the CSP source expression for an inline script with
// text: 'sha256-<base64 of its SHA-256>'. The test of the page's
// policy uses it to check swaggerBootstrapHash against swaggerBootstrap.
func cspHashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}
