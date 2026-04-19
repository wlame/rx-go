package webapi

import (
	_ "embed"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// faviconSVG is embedded at compile time so the binary is fully static
// and does not require the favicon.svg to exist on the filesystem at
// runtime. Bytes are copied verbatim from rx-python/src/rx/favicon.svg.
//
//go:embed favicon.svg
var faviconSVG []byte

// registerFaviconHandler wires /favicon.ico to the embedded SVG.
//
// Python's rx-python serves the same SVG bytes under /favicon.ico (see
// web.py:222) with Content-Type image/svg+xml regardless of the ".ico"
// suffix. Browsers accept SVGs at .ico for <link rel="icon"> so this is
// compatible with every modern UA.
//
// Important: this route is registered separately from the SPA catch-all
// so that even when the frontend cache is empty (no rx-viewer installed)
// the browser tab still has an icon.
func registerFaviconHandler(r chi.Router) {
	r.Get("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(faviconSVG)
	})
}
