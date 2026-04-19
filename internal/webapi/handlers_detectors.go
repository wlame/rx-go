package webapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// detectorsOutput wraps the DetectorsResponse body for huma's return-type
// convention.
type detectorsOutput struct {
	Body rxtypes.DetectorsResponse
}

// severityScaleFixed is the hard-coded 4-level severity reference. The
// scale never changes based on registered detectors — rx-viewer uses
// it as a legend regardless. Matches rx-python/src/rx/analyze/severity.py.
var severityScaleFixed = []rxtypes.SeverityScaleLevel{
	{Min: 0.0, Max: 0.4, Label: "low", Description: "Minor deviations, informational"},
	{Min: 0.4, Max: 0.6, Label: "medium", Description: "Warnings, format issues"},
	{Min: 0.6, Max: 0.8, Label: "high", Description: "Errors, crashes"},
	{Min: 0.8, Max: 1.0, Label: "critical", Description: "Fatal errors, exposed secrets"},
}

// registerDetectorsHandlers mounts GET /v1/detectors.
//
// At v1 the Go port ships with an empty analyzer registry (per user
// instructions — "pluggable_analyzer_architecture.zero_or_placeholder_
// analyzers_at_v1"). The endpoint still returns the full envelope with
// empty Detectors/Categories slices and the fixed severity scale so
// rx-viewer's UI doesn't branch on a missing field.
//
// Matches rx-python/src/rx/web.py:673-752.
func registerDetectorsHandlers(_ *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-detectors",
		Method:      http.MethodGet,
		Path:        "/v1/detectors",
		Summary:     "List all available anomaly detectors",
		Description: "Returns metadata about every registered detector, plus the severity-scale legend.",
		Tags:        []string{"Analysis"},
	}, func(_ context.Context, _ *struct{}) (*detectorsOutput, error) {
		return &detectorsOutput{Body: rxtypes.DetectorsResponse{
			Detectors:     []rxtypes.DetectorInfo{},
			Categories:    []rxtypes.CategoryInfo{},
			SeverityScale: severityScaleFixed,
		}}, nil
	})
}
