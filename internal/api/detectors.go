package api

import (
	"net/http"

	"github.com/wlame/rx/internal/models"
)

// handleDetectors handles GET /v1/detectors — list available anomaly detectors.
//
// Returns a DetectorsResponse with an empty detector list. The severity scale
// metadata is populated with the standard four levels. Actual detector
// implementations are deferred to Phase 7.
func (s *Server) handleDetectors(w http.ResponseWriter, r *http.Request) {
	resp := models.DetectorsResponse{
		Detectors:  []models.DetectorInfo{},
		Categories: []models.CategoryInfo{},
		SeverityScale: []models.SeverityScaleLevel{
			{Min: 0.0, Max: 0.4, Label: "low", Description: "Minor deviations, informational"},
			{Min: 0.4, Max: 0.6, Label: "medium", Description: "Warnings, format issues"},
			{Min: 0.6, Max: 0.8, Label: "high", Description: "Errors, crashes"},
			{Min: 0.8, Max: 1.0, Label: "critical", Description: "Fatal errors, exposed secrets"},
		},
	}

	writeJSON(w, http.StatusOK, resp)
}
