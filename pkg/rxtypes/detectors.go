package rxtypes

// SeverityRange is the min/max severity a single detector can produce.
type SeverityRange struct {
	Min float64 `json:"min" doc:"Lowest severity this detector emits, 0.0 to 1.0"`
	Max float64 `json:"max" doc:"Highest severity this detector emits, 0.0 to 1.0"`
}

// DetectorInfo is the metadata for one registered analyzer, returned by
// GET /v1/detectors.
//
// At v1 rx-go ships with an empty registry (per user instructions). This
// struct is still defined so the endpoint emits the correct envelope
// shape and the rx-viewer frontend doesn't need to branch on a missing
// field.
type DetectorInfo struct {
	Name string `json:"name" doc:"Stable identifier, kebab-case (e.g. \"traceback-python\"). Never display-formatted; use it as a key."`
	//nolint:lll // doc strings are long by nature; they are the spec text.
	Category      string        `json:"category" doc:"The category this detector reports anomalies under. Categories are backend-defined; read them from this response rather than hardcoding a list."`
	Description   string        `json:"description" doc:"One sentence describing what the detector looks for, suitable for a tooltip."`
	SeverityRange SeverityRange `json:"severity_range" doc:"The severity band this detector can emit. Use it to scale a UI indicator without waiting for real anomalies."`
	Examples      []string      `json:"examples" doc:"Short lines that would trigger this detector. May be empty."`
}

// CategoryInfo groups multiple detectors under a named category.
type CategoryInfo struct {
	Name        string   `json:"name" doc:"Stable category identifier, kebab-case. Used as the category field of an anomaly."`
	Description string   `json:"description" doc:"One sentence describing what this category covers."`
	Detectors   []string `json:"detectors" doc:"Names of the detectors that report under this category."`
}

// SeverityScaleLevel is one bucket in the hard-coded severity scale
// (low/medium/high/critical). The full scale is returned by
// GET /v1/detectors regardless of how many detectors are registered.
type SeverityScaleLevel struct {
	Min         float64 `json:"min" doc:"Inclusive lower bound of this level, 0.0 to 1.0"`
	Max         float64 `json:"max" doc:"Inclusive upper bound of this level, 0.0 to 1.0"`
	Label       string  `json:"label" doc:"Display name for the level (e.g. \"high\")"`
	Description string  `json:"description" doc:"One sentence on what this level means for a reader"`
}

// DetectorsResponse is the body for GET /v1/detectors.
//
// At v1 Detectors and Categories are empty slices; SeverityScale is
// always populated with the 4-level scale (defined in internal/analyzer).
type DetectorsResponse struct {
	Detectors     []DetectorInfo       `json:"detectors" doc:"Every detector this backend has registered. The two backends ship different sets, so a client must render whatever it is given."`
	Categories    []CategoryInfo       `json:"categories" doc:"The categories the registered detectors report under."`
	SeverityScale []SeverityScaleLevel `json:"severity_scale" doc:"The severity bands, always populated regardless of how many detectors are registered."`
}
