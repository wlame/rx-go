package webapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// signaturesOutput wraps the catalogue for huma, which takes the body
// from the Body field.
type signaturesOutput struct {
	Body rxtypes.SignaturesResponse
}

// registerSignaturesHandlers mounts GET /v1/signatures: the catalogue the
// server loaded when it started, as `rx signatures --json` prints it.
// It reads nothing: the catalogue is never reloaded while the server
// runs, so a change to a signature file takes a restart.
func registerSignaturesHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "signatures",
		Method:      http.MethodGet,
		Path:        "/v1/signatures",
		Summary:     "List the signatures the server loaded",
		Description: "The signature catalogue loaded when the server started (built-in signatures and the user's " +
			"files from RX_SIGNATURES_PATH or the configuration directory), as rx signatures --json prints it: " +
			"every signature with every field of its definition, its kind, source, keyword clauses and line pass; " +
			"the groups with their order and count; the levels; the load errors; the search path and the files read.",
		Tags: []string{"Signatures"},
	}, func(_ context.Context, _ *struct{}) (*signaturesOutput, error) {
		cat := s.cfg.Signatures
		return &signaturesOutput{Body: SignaturesView(cat, ListedSignatures(cat, "", ""), "")}, nil
	})
}
