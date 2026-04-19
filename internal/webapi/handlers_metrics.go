package webapi

import (
	"github.com/go-chi/chi/v5"

	"github.com/wlame/rx-go/internal/prometheus"
)

// registerMetricsHandler wires GET /metrics to the promhttp Handler
// built from our private registry (internal/prometheus.Registry).
//
// This is registered outside huma's typed pipeline because the
// Prometheus exposition format isn't JSON and has its own Content-Type.
// huma has no affordance for raw text/plain endpoints whose body isn't
// a struct.
func registerMetricsHandler(r chi.Router) {
	r.Handle("/metrics", prometheus.Handler())
}
