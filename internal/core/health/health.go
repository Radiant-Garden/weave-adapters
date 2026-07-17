// Package health implements the weave-shaped GET /api/v1/health endpoint.
//
// The response mirrors weave's DaemonHealthResponse so weave's generic health
// client works against adapters unchanged: an overall status, the adapter
// version, uptime, and a list of component entries. In M1 the only component is
// the adapter core itself — a backend readiness probe is added in M3.
package health

import (
	"encoding/json"
	"net/http"
	"time"
)

// Status is the health status vocabulary shared with weave, ordered
// healthy < unhealthy < unavailable (worst-of wins for the overall status).
type Status string

const (
	// StatusHealthy means the component is fully operational.
	StatusHealthy Status = "healthy"
	// StatusUnhealthy means the component is degraded but reachable.
	StatusUnhealthy Status = "unhealthy"
	// StatusUnavailable means the component is not ready to serve.
	StatusUnavailable Status = "unavailable"
)

// Component is a single entry in the health response.
type Component struct {
	Name   string            `json:"name"`
	Status Status            `json:"status"`
	Detail string            `json:"detail,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Response mirrors weave's DaemonHealthResponse shape.
type Response struct {
	Status        Status      `json:"status"`
	Version       string      `json:"version"`
	UptimeSeconds int64       `json:"uptimeSeconds"`
	Components    []Component `json:"components"`
}

// Handler serves the health endpoint. It is safe for concurrent use.
type Handler struct {
	version string
	started time.Time
}

// NewHandler returns a health handler that reports the given version and
// computes uptime from started.
func NewHandler(version string, started time.Time) *Handler {
	return &Handler{version: version, started: started}
}

// ServeHTTP writes the health response. The status code is 200 for
// healthy/unhealthy and 503 for unavailable, so an orchestrator readiness probe
// can key on the code alone.
func (h *Handler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	components := []Component{
		{Name: "core", Status: StatusHealthy, Detail: "adapter core running"},
	}
	overall := overallStatus(components)

	resp := Response{
		Status:        overall,
		Version:       h.version,
		UptimeSeconds: int64(time.Since(h.started).Seconds()),
		Components:    components,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(httpStatus(overall))

	// The response is already committed; a write failure is not actionable here
	// and will surface via the (future) request-logging middleware.
	_ = json.NewEncoder(w).Encode(resp)
}

// overallStatus returns the worst status across all components.
func overallStatus(components []Component) Status {
	worst := StatusHealthy
	for _, c := range components {
		if rank(c.Status) > rank(worst) {
			worst = c.Status
		}
	}

	return worst
}

// rank orders the status vocabulary; higher is worse.
func rank(s Status) int {
	switch s {
	case StatusHealthy:
		return 0
	case StatusUnhealthy:
		return 1
	case StatusUnavailable:
		return 2
	default:
		return 3
	}
}

// httpStatus maps an overall status to its HTTP response code.
func httpStatus(s Status) int {
	if s == StatusUnavailable {
		return http.StatusServiceUnavailable
	}

	return http.StatusOK
}
