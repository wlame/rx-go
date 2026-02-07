package models

// HealthResponse represents the health status of the service
type HealthResponse struct {
	Status  string            `json:"status"`  // "ok", "degraded", "error"
	Version string            `json:"version"`
	Uptime  float64           `json:"uptime"` // Seconds since start
	Checks  map[string]string `json:"checks"` // Component -> status
}
