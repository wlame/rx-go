package models

// AnomalyRange represents a range of lines with detected anomalies
type AnomalyRange struct {
	StartLine   int      `json:"start_line"`
	EndLine     int      `json:"end_line"`
	Detectors   []string `json:"detectors"`   // List of detector IDs that flagged this range
	Severity    string   `json:"severity"`    // "low", "medium", "high"
	Description string   `json:"description"` // Human-readable description
}

// DetectorInfo represents information about an anomaly detector
type DetectorInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Categories  []string `json:"categories"` // e.g., ["error", "traceback"]
	Enabled     bool     `json:"enabled"`
}

// DetectorsResponse represents the response from listing detectors
type DetectorsResponse struct {
	Detectors []DetectorInfo `json:"detectors"`
}
