// Package health implements the weave-shaped GET /api/v1/health endpoint.
//
// The response mirrors weave's DaemonHealthResponse so weave's generic health
// client works against adapters unchanged: an overall status, the adapter
// version, uptime, and per-component detail. The adapter core registers a
// self-component; each adapter adds a probe that cheaply pings its backend (M3).
// Probe results are cached briefly so aggressive polling can't hammer backends,
// and overall-status transitions emit HLT-001 (only on change).
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// probeCacheTTL bounds how often probes actually run, regardless of poll rate.
const probeCacheTTL = 5 * time.Second

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
	probes  []Probe
	ttl     time.Duration
	now     func() time.Time // injectable clock for tests

	mu         sync.Mutex
	components []Component
	checkedAt  time.Time
	last       Status // last overall status, for transition detection
}

// NewHandler returns a health handler reporting the given version, computing
// uptime from started. The core self-component is always present; adapters pass
// additional backend probes.
func NewHandler(version string, started time.Time, probes ...Probe) *Handler {
	all := make([]Probe, 0, len(probes)+1)
	all = append(all, coreProbe{})
	all = append(all, probes...)

	return &Handler{
		version: version,
		started: started,
		probes:  all,
		ttl:     probeCacheTTL,
		now:     time.Now,
	}
}

// ServeHTTP writes the health response. The status code is 200 for
// healthy/unhealthy and 503 for unavailable.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	components, overall := h.evaluate(r.Context())

	resp := Response{
		Status:        overall,
		Version:       h.version,
		UptimeSeconds: int64(h.now().Sub(h.started).Seconds()),
		Components:    components,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(httpStatus(overall))

	// The response is already committed; a write failure is not actionable here.
	_ = json.NewEncoder(w).Encode(resp)
}

// evaluate returns the component results and overall status, re-running the
// probes at most once per ttl. It emits HLT-001 when the overall status changes
// — never on the first evaluation, never on an unchanged poll.
func (h *Handler) evaluate(ctx context.Context) ([]Component, Status) {
	components, overall, from, changed := h.refresh(ctx)

	// Emit outside the lock: a transition is a system event, and slog/fan-out
	// must not run while the health mutex is held.
	if changed {
		events.Emit(ctx, catalog.HLT001, "from", string(from), "to", string(overall))
	}

	return components, overall
}

// refresh re-runs the probes at most once per ttl, records the overall status,
// and reports whether it changed. It holds the mutex for the whole check, so
// backend probes (M3) must keep Check fast (or the ttl short) — concurrent
// health requests serialize behind a re-run.
func (h *Handler) refresh(ctx context.Context) (components []Component, overall, from Status, changed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.components == nil || h.now().Sub(h.checkedAt) >= h.ttl {
		h.components = h.runProbes(ctx)
		h.checkedAt = h.now()
	}

	overall = overallStatus(h.components)
	from = h.last
	changed = h.last != "" && overall != h.last
	h.last = overall

	return h.components, overall, from, changed
}

// runProbes checks every probe and maps the results to response components.
func (h *Handler) runProbes(ctx context.Context) []Component {
	out := make([]Component, 0, len(h.probes))

	for _, p := range h.probes {
		res := p.Check(ctx)
		out = append(out, Component{
			Name:   p.Name(),
			Status: res.Status,
			Detail: res.Detail,
			Fields: res.Fields,
		})
	}

	return out
}
