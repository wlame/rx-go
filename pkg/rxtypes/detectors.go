package rxtypes

// SeverityRange is the min/max severity a single detector can produce.
type SeverityRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// DetectorInfo is the metadata for one registered analyzer, returned by
// GET /v1/detectors.
//
// At v1 rx-go ships with an empty registry (per user instructions). This
// struct is still defined so the endpoint emits the correct envelope
// shape and the rx-viewer frontend doesn't need to branch on a missing
// field.
type DetectorInfo struct {
	Name          string        `json:"name"`
	Category      string        `json:"category"`
	Description   string        `json:"description"`
	SeverityRange SeverityRange `json:"severity_range"`
	Examples      []string      `json:"examples"`
}

// CategoryInfo groups multiple detectors under a named category.
type CategoryInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Detectors   []string `json:"detectors"`
}

// SeverityScaleLevel is one bucket in the hard-coded severity scale
// (low/medium/high/critical). The full scale is returned by
// GET /v1/detectors regardless of how many detectors are registered.
type SeverityScaleLevel struct {
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
}

// DetectorsResponse is the body for GET /v1/detectors.
//
// At v1 Detectors and Categories are empty slices; SeverityScale is
// always populated with the 4-level scale (defined in internal/analyzer).
type DetectorsResponse struct {
	Detectors     []DetectorInfo       `json:"detectors"`
	Categories    []CategoryInfo       `json:"categories"`
	SeverityScale []SeverityScaleLevel `json:"severity_scale"`
}
