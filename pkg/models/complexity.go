package models

// ComplexityRequest represents a request to analyze regex complexity
type ComplexityRequest struct {
	Pattern string `json:"pattern" validate:"required"`
}

// ComplexityResponse represents the response from a complexity check
type ComplexityResponse struct {
	Pattern             string  `json:"pattern"`
	ComplexityScore     int     `json:"complexity_score"`
	RiskLevel           string  `json:"risk_level"` // "low", "medium", "high", "catastrophic"
	EstimatedTimeMs     float64 `json:"estimated_time_ms"`
	Warnings            []string `json:"warnings"`
	RedoSVulnerable     bool    `json:"redos_vulnerable"`
	BacktrackingFactors []string `json:"backtracking_factors,omitempty"`
}
