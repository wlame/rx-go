package webapi

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/wlame/rx-go/internal/frontend"
)

// reservedAPIPrefixes are first path segments that must NEVER fall
// through to the SPA catch-all. Matching rx-python/src/rx/web.py:2164.
//
// Why a defense-in-depth list? chi's route matching already registers
// these under non-catch-all handlers, so in theory they would never
// reach registerStaticHandlers. But if a future handler family is
// mis-registered, this list prevents the SPA from serving /v1/foo as
// an SPA route, which would confuse API clients with a 200-text/html.
var reservedAPIPrefixes = []string{
	"v1/",
	"health",
	"metrics",
	"docs",
	"redoc",
	"openapi.json",
	"openapi.yaml",
	"openapi",
	"schemas/",
}

// isReservedAPIPath reports whether a given cleaned path (relative to
// "/", i.e. the leading slash has been stripped) matches one of the
// reserved API prefixes. Exact matches and "prefix/"-style matches
// both count.
func isReservedAPIPath(p string) bool {
	for _, r := range reservedAPIPrefixes {
		if strings.HasSuffix(r, "/") {
			// Prefix match against a "dir/" style entry.
			if strings.HasPrefix(p, r) || p == strings.TrimSuffix(r, "/") {
				return true
			}
		} else {
			if p == r {
				return true
			}
			// Also reject same-segment subpaths like "openapi.json/foo".
			if strings.HasPrefix(p, r+"/") {
				return true
			}
		}
	}
	return false
}

// registerStaticHandlers mounts:
//
//	GET /           → frontend/index.html or 307 → /docs
//	GET /{rest:*}   → static file from frontend, or SPA index.html fallback
//
// ReDoc is also registered here because it's a static-style HTML page.
//
// This function MUST be called last in the router setup so the catch-all
// does not shadow API routes.
func registerStaticHandlers(r chi.Router, fm *frontend.Manager) {
	registerRedocRoute(r)

	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		// If there's no frontend at all → 307 redirect to /docs.
		if fm == nil || !fm.IsAvailable() {
			http.Redirect(w, req, "/docs", http.StatusTemporaryRedirect)
			return
		}
		serveFrontendIndex(w, req, fm)
	})

	// Catch-all. chi's {rest:*} placeholder captures everything after /.
	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		path := chi.URLParam(req, "*")

		// Reserved API prefix → hard 404. Never fall through.
		if isReservedAPIPath(path) {
			http.NotFound(w, req)
			return
		}

		// No frontend cache → 307 → /docs (same as "/").
		if fm == nil || !fm.IsAvailable() {
			http.Redirect(w, req, "/docs", http.StatusTemporaryRedirect)
			return
		}

		// Try to serve as a static file. ValidateStaticPath rejects
		// directory-traversal, absolute paths, and non-existent files,
		// returning "" in those cases.
		if resolved := fm.ValidateStaticPath(path); resolved != "" {
			serveStaticFile(w, req, resolved)
			return
		}

		// No matching static file → serve SPA index.html so the client
		// router handles the URL. Content-Type MUST be text/html so the
		// browser actually renders it (not image/whatever extension).
		serveFrontendIndex(w, req, fm)
	})
}

// serveFrontendIndex serves frontend/index.html with Content-Type
// text/html. Keeps a short Cache-Control because the asset references
// inside index.html rotate on every deploy.
func serveFrontendIndex(w http.ResponseWriter, req *http.Request, fm *frontend.Manager) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	http.ServeFile(w, req, fm.IndexHTMLPath())
}

// serveStaticFile serves a validated absolute path from the frontend
// cache, with Content-Type derived from extension.
//
// http.ServeFile internally calls http.ServeContent which runs its own
// Content-Type sniff. We override the common web-asset extensions
// BEFORE ServeFile so browsers don't guess wrong on .css / .js / .svg.
// (Go's mime package uses system /etc/mime.types which is fine in
// normal environments but produces "text/plain" for .js in slim Docker
// images without the mime database.)
func serveStaticFile(w http.ResponseWriter, req *http.Request, resolved string) {
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".js", ".mjs":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".map":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	case ".html", ".htm":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	case ".woff":
		w.Header().Set("Content-Type", "font/woff")
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	}
	// Hashed-asset cache hint. index-<hash>.js etc. never change, so a
	// long cache is fine; index.html itself is handled above with no-cache.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, req, resolved)
}
