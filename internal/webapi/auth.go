package webapi

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// SECURITY: the opt-in API token.
//
// rx serves a trusted internal network and has no identity system; the
// operator builds the perimeter. Some deployments sit on a network that
// is trusted enough not to need a proxy but shared enough that a shared
// secret is worth having. RX_API_TOKEN is that secret: when it is set,
// every /v1 request must carry `Authorization: Bearer <token>`. It is one
// value for every caller — no users, no sessions, no expiry.
//
// /health, /metrics, /docs, /openapi.json and the viewer's static files
// stay open: probes and scrapers carry no token, the documentation and
// the bundle hold no data, and /health reports secrets as redacted.
//
// The token crosses the wire in clear text over plain HTTP. It is a
// guard against a neighbor on a shared network, not against someone
// who can read the traffic; that still takes a VPN, an SSH tunnel or a
// TLS proxy.

// bearerSecurityScheme names the token in the OpenAPI document.
const bearerSecurityScheme = "bearerAuth"

// unauthorizedDetail is the 401 body's detail. It says what to send, so
// a person who meets it in a terminal can fix the request.
const unauthorizedDetail = "an API token is required: send the header " +
	"Authorization: Bearer <token>, with the token this server's RX_API_TOKEN holds"

// isTokenProtectedPath reports whether a request path is part of the /v1
// API the token guards.
func isTokenProtectedPath(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

// apiTokenMiddleware requires the bearer token on every /v1 request when
// token is non-empty, and passes everything through when it is empty.
//
// The comparison is constant-time, so the time a wrong guess takes says
// nothing about how much of it was right.
func apiTokenMiddleware(token string) func(http.Handler) http.Handler {
	if token == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	want := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isTokenProtectedPath(r.URL.Path) || bearerTokenMatches(r, want) {
				next.ServeHTTP(w, r)
				return
			}
			writeUnauthorized(w)
		})
	}
}

// bearerTokenMatches reports whether the request carries
// `Authorization: Bearer <want>`. The scheme name is case-insensitive,
// as RFC 7235 says; the token is compared exactly.
func bearerTokenMatches(r *http.Request, want []byte) bool {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), want) == 1
}

// writeUnauthorized answers 401 with the challenge and the error
// envelope every other rx error uses, framed the same way: no HTML
// escaping, so `<token>` reads as typed in a terminal, and no trailing
// newline.
func writeUnauthorized(w http.ResponseWriter) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(apiError{Detail: unauthorizedDetail})
	w.Header().Set("WWW-Authenticate", `Bearer realm="rx"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(bytes.TrimSuffix(body.Bytes(), []byte("\n")))
}

// declareOptionalBearerToken publishes the token in the OpenAPI
// document: the bearer scheme, and on every /v1 operation a security
// requirement of "the token, or nothing" plus a 401 response. "Or
// nothing" is what makes it optional — a server without RX_API_TOKEN
// ignores the header — so a generated client neither demands a token
// nor drops one it was given.
//
// It runs after every operation is registered, and reaches the
// operations through the document rather than through each
// registration, so a new /v1 route is covered without remembering to.
func declareOptionalBearerToken(api huma.API) {
	doc := api.OpenAPI()
	if doc.Components.SecuritySchemes == nil {
		doc.Components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	doc.Components.SecuritySchemes[bearerSecurityScheme] = &huma.SecurityScheme{
		Type:        "http",
		Scheme:      "bearer",
		Description: "Required on /v1 only when the server sets RX_API_TOKEN.",
	}
	envelope := doc.Components.Schemas.Schema(reflect.TypeOf(apiError{}), true, "ApiError")
	unauthorized := &huma.Response{
		Description: "The server requires an API token and the request did not carry it",
		Content:     map[string]*huma.MediaType{"application/json": {Schema: envelope}},
	}
	for path, item := range doc.Paths {
		if !isTokenProtectedPath(path) {
			continue
		}
		for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if op == nil {
				continue
			}
			op.Security = []map[string][]string{{bearerSecurityScheme: {}}, {}}
			if op.Responses == nil {
				op.Responses = map[string]*huma.Response{}
			}
			op.Responses["401"] = unauthorized
		}
	}
}
