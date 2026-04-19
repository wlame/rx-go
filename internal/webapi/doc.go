// Package webapi implements the HTTP surface of rx-go.
//
// This package wires together nine endpoint families (health, metrics,
// trace, samples, index, compress, tasks, tree, detectors) plus a static
// SPA server for the rx-viewer frontend. The router is chi v5; OpenAPI
// 3.1 generation + request/response binding is handled by
// github.com/danielgtaylor/huma/v2 via its humachi adapter.
//
// # Architecture
//
// NewServer returns a configured *Server. The caller calls Start() to
// bind and serve, and Shutdown(ctx) to drain in-flight requests on
// SIGINT/SIGTERM.
//
// Server wires these dependencies:
//   - chi.Router — raw HTTP routing
//   - huma.API   — typed handler registration + OpenAPI
//   - trace.Engine — regex search backend (stateless)
//   - tasks.Manager — background compress/index jobs
//   - requeststore.Store — per-request scan metadata
//   - hooks.Dispatcher — webhook fan-out
//   - frontend.Manager — SPA cache directory
//   - prometheus.Registry — metrics
//
// # Route ordering (critical)
//
// chi matches routes in registration order. This package registers in
// exactly three phases:
//
//  1. Middleware (requestID → logger → recover → promMetrics)
//  2. API routes (via huma): /health, /metrics, /v1/*, /openapi.json,
//     /docs, /redoc. All huma routes get registered before any static
//     routes so that a 404 from a /v1/... URL does not fall through to
//     the SPA catch-all.
//  3. Static routes (handlers_static.go): /favicon.ico, /, /{rest:*}.
//     The catch-all is LAST. It returns a 404 for any URL whose first
//     segment is a reserved API prefix (v1/, health, metrics, docs,
//     redoc, openapi.json) so that a subsequent fall-through can't
//     accidentally serve the SPA to an API client.
//
// # Frontend/SPA semantics
//
// Given a request to "/{path}":
//   - If path is "" → serve frontend/index.html (or 307 → /docs).
//   - If path starts with reserved API prefix → 404 regardless.
//   - If frontend cache has a matching static file → serve it with the
//     correct Content-Type (image/svg+xml, text/css, application/javascript,
//     etc.) via http.ServeFile.
//   - Otherwise (unknown path) → serve frontend/index.html with
//     Content-Type: text/html. The SPA client router handles the route.
//
// This matches rx-python/src/rx/web.py (serve_frontend + serve_spa
// functions at web.py:2144-2189).
package webapi
