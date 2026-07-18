package health

import (
	"context"
	"net/http"
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

// Result is the outcome of a single probe check.
type Result struct {
	Status Status
	Detail string
	Fields map[string]string
}

// Probe reports the health of one component. Implementations must be safe for
// concurrent use and should respect ctx cancellation/deadline (backend probes
// talk to slow systems).
type Probe interface {
	Name() string
	Check(ctx context.Context) Result
}

// coreProbe reports the adapter core itself. In M1 it is always healthy — there
// is no backend to probe yet (that arrives with the first adapter in M3).
type coreProbe struct{}

func (coreProbe) Name() string { return "core" }

func (coreProbe) Check(context.Context) Result {
	return Result{Status: StatusHealthy, Detail: "adapter core running"}
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

// httpStatus maps an overall status to its HTTP response code: 503 only when
// unavailable, so an orchestrator readiness probe can key on the code alone.
func httpStatus(s Status) int {
	if s == StatusUnavailable {
		return http.StatusServiceUnavailable
	}

	return http.StatusOK
}
