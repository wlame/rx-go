package webapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

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

	// Frame response bodies the way FastAPI does: no trailing newline.
	cfg.Formats = jsonFormatsWithoutTrailingNewline(cfg.Formats)

	// ...and with no `$schema` field either. huma.DefaultConfig installs
	// a schema-link transformer that adds one to every body; rx-python
	// emits no such key, it is declared nowhere in pkg/rxtypes, and a
	// strict decoder rejects the whole document over it. Dropping the
	// transformer here means it never reaches the OpenAPI schemas or the
	// wire.
	//
	// CreateHooks run when the huma.API is built, so clearing the slice
	// now is what stops the hook from appending the transformer to
	// cfg.Transformers and cfg.OnAddOperation.
	cfg.CreateHooks = nil

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
		{Name: "Logs", Description: "Log chains: the files of a rotated log read as one"},
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

// jsonFormatsWithoutTrailingNewline copies `formats`, replacing every
// JSON entry with one that does not terminate the body with a newline.
//
// Go's json.Encoder.Encode appends "\n" to each value it writes, and
// huma's DefaultJSONFormat uses an Encoder, so every rx-go body carried
// a byte that rx-python's FastAPI body did not. Nothing parsing the
// response can tell the difference, but a cross-backend check comparing
// raw bytes always could, so the two backends now agree byte for byte.
//
// The map is copied rather than mutated: huma.DefaultConfig assigns the
// package-level huma.DefaultFormats map by reference, so writing to
// cfg.Formats would change the default for every API in the process,
// including ones built by tests that expect huma's own framing.
//
// Only entries whose Marshal is JSON-shaped are replaced. The key set
// is huma's ("application/json" and "json" today, plus "application/cbor"
// when the CBOR package is imported), and a binary format must keep its
// own marshaller.
func jsonFormatsWithoutTrailingNewline(formats map[string]huma.Format) map[string]huma.Format {
	out := make(map[string]huma.Format, len(formats))
	for mediaType, format := range formats {
		if strings.Contains(mediaType, "json") {
			out[mediaType] = jsonFormatNoNewline
			continue
		}
		out[mediaType] = format
	}
	return out
}

// jsonFormatNoNewline is huma.DefaultJSONFormat with the terminating
// newline removed.
//
// Encode is still what does the marshaling — SetEscapeHTML(false) has
// to match FastAPI, which passes ensure_ascii=False and does not escape
// <, > or & — so the buffer holds exactly huma's bytes and the newline
// is trimmed off the end. json.Encoder.Encode writes the whole value in
// a single Write, so this buffers nothing the encoder was not already
// holding.
var jsonFormatNoNewline = huma.Format{
	Marshal: func(w io.Writer, v any) error {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return err
		}
		_, err := w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
		return err
	},
	Unmarshal: json.Unmarshal,
}
