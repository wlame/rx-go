package models

// RegexIssueDetail describes a single vulnerability issue found in a regex pattern.
type RegexIssueDetail struct {
	Type              string   `json:"type"`
	Severity          string   `json:"severity"`
	ComplexityClass   string   `json:"complexity_class"`
	ComplexityNotation string  `json:"complexity_notation"`
	Segment           string   `json:"segment"`
	Explanation       string   `json:"explanation"`
	FixSuggestions    []string `json:"fix_suggestions"`
}

// PerformanceEstimate holds estimated operations for different input sizes.
type PerformanceEstimate struct {
	OpsAt100          int  `json:"ops_at_100"`
	OpsAt1000         int  `json:"ops_at_1000"`
	OpsAt10000        int  `json:"ops_at_10000"`
	SafeForLargeFiles bool `json:"safe_for_large_files"`
}

// ComplexityDetails holds legacy pattern analysis details.
type ComplexityDetails struct {
	StarHeight     int  `json:"star_height"`
	QuantifierCount int  `json:"quantifier_count"`
	HasStartAnchor bool `json:"has_start_anchor"`
	HasEndAnchor   bool `json:"has_end_anchor"`
	IssueCount     int  `json:"issue_count"`
}

// ComplexityResponse is the response from the complexity analysis endpoint / CLI.
// In the Go rewrite this is a STUB — all fields are populated with zero/null/"not_implemented" values.
type ComplexityResponse struct {
	Regex               string              `json:"regex"`
	Score               float64             `json:"score"`
	RiskLevel           string              `json:"risk_level"`
	ComplexityClass     string              `json:"complexity_class"`
	ComplexityNotation  string              `json:"complexity_notation"`
	Issues              []RegexIssueDetail  `json:"issues"`
	Recommendations     []string            `json:"recommendations"`
	Performance         PerformanceEstimate `json:"performance"`
	StarHeight          int                 `json:"star_height"`
	PatternLength       int                 `json:"pattern_length"`
	HasAnchors          [2]bool             `json:"has_anchors"`
	// Legacy fields for backwards compatibility.
	Level    string            `json:"level"`
	Risk     string            `json:"risk"`
	Warnings []string          `json:"warnings"`
	Details  ComplexityDetails `json:"details"`
	// CLI equivalent command.
	CLICommand *string `json:"cli_command"`
}

// NewStubComplexityResponse returns a ComplexityResponse pre-filled with "not implemented"
// values. Used by the stub endpoint and CLI command.
func NewStubComplexityResponse(regex string) ComplexityResponse {
	return ComplexityResponse{
		Regex:              regex,
		Score:              0,
		RiskLevel:          "not_implemented",
		ComplexityClass:    "not_implemented",
		ComplexityNotation: "not_implemented",
		Issues:             []RegexIssueDetail{},
		Recommendations:    []string{},
		Performance: PerformanceEstimate{
			SafeForLargeFiles: false,
		},
		PatternLength: len(regex),
		HasAnchors:    [2]bool{false, false},
		Level:         "unknown",
		Risk:          "not_implemented",
		Warnings:      []string{},
		Details:       ComplexityDetails{},
	}
}
