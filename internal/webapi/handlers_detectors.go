package webapi

import (
	"context"
	"net/http"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/analyzer"
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

// categoryDescriptions maps each analyzer category to a short human-
// readable sentence for the /v1/detectors response. Unknown categories
// fall back to the category name itself so the field is never empty.
//
//nolint:gosec // G101: human-readable description text, not credential values
var categoryDescriptions = map[string]string{
	"log-traceback": "Stack traces from Python, Java, Go, and JavaScript runtimes.",
	"log-crash":     "Operating-system crash and core-dump signatures.",
	"secrets":       "Credential-shaped strings that may indicate exposed sensitive values.",
	"format":        "Structural format anomalies such as long lines and multi-line JSON blobs.",
	"repetition":    "Runs of identical consecutive lines that suggest spam or stuck loops.",
}

// categoryDescription returns a short description for the given
// category name. Falls back to the raw category name if the map has no
// entry — ensures the Description field is never empty even when a new
// category is added without updating this map.
func categoryDescription(cat string) string {
	if desc, ok := categoryDescriptions[cat]; ok {
		return desc
	}
	return cat
}

// buildDetectorsResponse walks the frozen analyzer registry and produces
// the DetectorsResponse payload. One DetectorInfo per registered detector,
// and a CategoryInfo bucket per distinct Category() value.
//
// Severity ranges are currently hard-coded to the full [0.0, 1.0] interval
// per detector — the registry doesn't expose a per-detector range today,
// and the 4-level scale in SeverityScale is the stable UI contract.
// When a detector starts reporting its own range we can widen this.
func buildDetectorsResponse() rxtypes.DetectorsResponse {
	registered := analyzer.Snapshot()
	detectors := make([]rxtypes.DetectorInfo, 0, len(registered))

	// Track categories in encounter order so the Categories slice is
	// deterministic from run to run. A map holds the member list; the
	// order slice records the distinct category names in registration
	// order (matches the Detectors slice ordering).
	categoryMembers := make(map[string][]string)
	var categoryOrder []string

	for _, a := range registered {
		name := a.Name()
		category := a.Category()
		detectors = append(detectors, rxtypes.DetectorInfo{
			Name:        name,
			Category:    category,
			Description: a.Description(),
			SeverityRange: rxtypes.SeverityRange{
				Min: 0.0,
				Max: 1.0,
			},
			Examples: []string{},
		})
		if _, seen := categoryMembers[category]; !seen {
			categoryOrder = append(categoryOrder, category)
		}
		categoryMembers[category] = append(categoryMembers[category], name)
	}

	categories := make([]rxtypes.CategoryInfo, 0, len(categoryOrder))
	for _, cat := range categoryOrder {
		// Sort members within a category so the response is stable even
		// if registration order shifts between builds (the outer order
		// comes from registration; the inner order is alphabetical).
		members := append([]string(nil), categoryMembers[cat]...)
		sort.Strings(members)
		categories = append(categories, rxtypes.CategoryInfo{
			Name:        cat,
			Description: categoryDescription(cat),
			Detectors:   members,
		})
	}

	return rxtypes.DetectorsResponse{
		Detectors:     detectors,
		Categories:    categories,
		SeverityScale: severityScaleFixed,
	}
}

// registerDetectorsHandlers mounts GET /v1/detectors.
//
// The response is built from the frozen analyzer registry: one entry per
// registered detector plus a category roll-up. Matches
// rx-python/src/rx/web.py:673-752 in shape.
func registerDetectorsHandlers(_ *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-detectors",
		Method:      http.MethodGet,
		Path:        "/v1/detectors",
		Summary:     "List all available anomaly detectors",
		Description: "Returns metadata about every registered detector, plus the severity-scale legend.",
		Tags:        []string{"Analysis"},
	}, func(_ context.Context, _ *struct{}) (*detectorsOutput, error) {
		return &detectorsOutput{Body: buildDetectorsResponse()}, nil
	})
}
