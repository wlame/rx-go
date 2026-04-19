package webapi

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
)

// The rx Python version is based on FastAPI, which renders Swagger UI
// at /docs and ReDoc at /redoc by default. Both surfaces are available
// from rx-viewer's help panel, so we must keep them alive.
//
// huma v2's DefaultConfig ships with Stoplight Elements. We swap that
// for Swagger UI to more closely match FastAPI — it's the exact same
// Swagger UI asset FastAPI serves, so a user alt-tabbing between
// `rx-python serve` and `rx serve` sees near-identical pages.

// newHumaConfig produces the huma.Config for this service.
//
// Key overrides vs. huma.DefaultConfig:
//   - Title / version pulled from rx-go's own metadata.
//   - Error envelope shape replaced via huma.NewError → {"detail":...}
//     matching FastAPI (see errors.go).
//   - Docs path served by Swagger UI (not Stoplight).
//   - OpenAPI path set to "/openapi" so /openapi.json and /openapi.yaml
//     both work. This matches FastAPI's default.
//   - Description text kept short; rx-viewer doesn't parse it.
func newHumaConfig(appVersion string) huma.Config {
	// Install the FastAPI-compatible error envelope. This must happen
	// BEFORE the first huma.API is built so that the generated OpenAPI
	// error schemas reference the overridden type.
	huma.NewError = humaNewError

	cfg := huma.DefaultConfig("rx-tool API", appVersion)
	// cfg.OpenAPI is embedded, so its Info/Tags are reached directly
	// via the selector without naming the embedded field. staticcheck
	// QF1008 prefers this shorter form.
	cfg.Info.Description = "Regex search + file indexing for large logs. " +
		"Go port of rx-python."
	cfg.Info.Contact = &huma.Contact{
		Name: "rx-tool",
		URL:  "https://github.com/wlame/rx-tool",
	}
	cfg.Info.License = &huma.License{
		Name: "MIT",
		URL:  "https://opensource.org/licenses/MIT",
	}
	// Use Swagger UI so /docs looks like FastAPI's /docs.
	cfg.DocsRenderer = huma.DocsRendererSwaggerUI

	// Tag descriptions mirror the Python tags so the rendered UI has
	// the same group headings users are accustomed to.
	cfg.Tags = []*huma.Tag{
		{Name: "General", Description: "Service health and status"},
		{Name: "Monitoring", Description: "Prometheus metrics"},
		{Name: "Search", Description: "Regex pattern matching"},
		{Name: "Context", Description: "File context and sample extraction"},
		{Name: "Indexing", Description: "File indexing operations"},
		{Name: "Operations", Description: "Background tasks"},
		{Name: "FileTree", Description: "File system navigation"},
		{Name: "Analysis", Description: "Anomaly detection"},
	}

	return cfg
}

// registerRedocRoute installs a minimal ReDoc HTML page at /redoc.
//
// huma v2 doesn't have a built-in /redoc renderer (only Swagger/Stoplight/
// Scalar); Python FastAPI always mounts one. The rx-viewer project links
// to /redoc from its help tooltip, so we provide a tiny HTML shim.
func registerRedocRoute(r chi.Router) {
	const redocHTML = `<!DOCTYPE html>
<html>
  <head>
    <title>rx-tool API docs (ReDoc)</title>
    <meta charset="utf-8"/>
    <meta name="viewport" content="width=device-width, initial-scale=1"/>
    <link href="https://fonts.googleapis.com/css?family=Montserrat:300,400,700|Roboto:300,400,700" rel="stylesheet">
    <style>body{margin:0;padding:0;}</style>
  </head>
  <body>
    <redoc spec-url="/openapi.json"></redoc>
    <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
  </body>
</html>`
	r.Get("/redoc", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(redocHTML))
	})
}
