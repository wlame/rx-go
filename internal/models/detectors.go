package models

// SeverityRange defines a min/max severity score band.
type SeverityRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// DetectorInfo holds metadata about an anomaly detector.
type DetectorInfo struct {
	Name          string        `json:"name"`
	Category      string        `json:"category"`
	Description   string        `json:"description"`
	SeverityRange SeverityRange `json:"severity_range"`
	Examples      []string      `json:"examples"`
}

// CategoryInfo holds metadata about an anomaly category.
type CategoryInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Detectors   []string `json:"detectors"`
}

// SeverityScaleLevel describes one level in the severity scale.
type SeverityScaleLevel struct {
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
}

// DetectorsResponse is the response from GET /v1/detectors.
type DetectorsResponse struct {
	Detectors     []DetectorInfo       `json:"detectors"`
	Categories    []CategoryInfo       `json:"categories"`
	SeverityScale []SeverityScaleLevel `json:"severity_scale"`
}

// NewDetectorsResponse returns a DetectorsResponse with all slice fields initialized
// to non-nil empty values so they serialize as [] rather than null.
func NewDetectorsResponse() DetectorsResponse {
	return DetectorsResponse{
		Detectors:     []DetectorInfo{},
		Categories:    []CategoryInfo{},
		SeverityScale: []SeverityScaleLevel{},
	}
}
