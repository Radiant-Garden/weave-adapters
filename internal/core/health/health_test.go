/*
Testing: health.go

Pending:

Tested:
  Handler.ServeHTTP / evaluate / runProbes
    - TestHandler_ShouldReturnWeaveShape: 200, no-cache, weave-shaped body with the core component.
    - TestHandler_Should503WhenUnavailable: an unavailable probe yields 503.
    - TestHandler_ShouldCacheProbeResults: probes run at most once per TTL.
    - TestHandler_ShouldReRunProbesAfterTTL: probes re-run once the TTL elapses.
    - TestHandler_ShouldEmitHLTOnTransitionOnly: HLT-001 fires only on a status change.

Tested elsewhere:

Declined:

Additional Remarks:
  The transition test installs the global emitter hook (recorder) and drives an
  injected clock, so it runs sequentially. The other tests use their own Handler
  and never transition, so they stay parallel-safe.
*/

package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubProbe is a controllable probe that counts its checks.
type stubProbe struct {
	name   string
	status Status
	calls  int
}

func (p *stubProbe) Name() string { return p.name }

func (p *stubProbe) Check(context.Context) Result {
	p.calls++

	return Result{Status: p.status}
}

// testClock is a manually-advanced clock.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func serve(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil))

	return rw
}

func TestHandler_ShouldReturnWeaveShape(t *testing.T) {
	t.Parallel()

	// ACT
	rw := serve(t, NewHandler("1.2.3", time.Now()))

	// ASSERT
	assert.Equal(t, http.StatusOK, rw.Code)
	assert.Equal(t, "no-cache", rw.Header().Get("Cache-Control"))
	assert.Equal(t, "application/json", rw.Header().Get("Content-Type"))

	var resp Response

	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &resp))
	assert.Equal(t, StatusHealthy, resp.Status)
	assert.Equal(t, "1.2.3", resp.Version)
	require.Len(t, resp.Components, 1)
	assert.Equal(t, "core", resp.Components[0].Name)
	assert.Equal(t, StatusHealthy, resp.Components[0].Status)
}

func TestHandler_Should503WhenUnavailable(t *testing.T) {
	t.Parallel()

	// ACT
	rw := serve(t, NewHandler("1.0.0", time.Now(), &stubProbe{name: "backend", status: StatusUnavailable}))

	// ASSERT
	assert.Equal(t, http.StatusServiceUnavailable, rw.Code)

	var resp Response

	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &resp))
	assert.Equal(t, StatusUnavailable, resp.Status)
}

func TestHandler_ShouldCacheProbeResults(t *testing.T) {
	t.Parallel()

	// ARRANGE
	probe := &stubProbe{name: "backend", status: StatusHealthy}
	h := NewHandler("1.0.0", time.Now(), probe)

	// ACT — two quick polls within the TTL.
	serve(t, h)
	serve(t, h)

	// ASSERT — the probe ran only once.
	assert.Equal(t, 1, probe.calls)
}

func TestHandler_ShouldReRunProbesAfterTTL(t *testing.T) {
	t.Parallel()

	// ARRANGE
	clk := &testClock{t: time.Now()}
	probe := &stubProbe{name: "backend", status: StatusHealthy}
	h := NewHandler("1.0.0", clk.now(), probe)
	h.now = clk.now

	// ACT
	serve(t, h)
	clk.advance(probeCacheTTL + time.Second)
	serve(t, h)

	// ASSERT — the TTL elapsed, so the probe ran again.
	assert.Equal(t, 2, probe.calls)
}

func TestHandler_ShouldEmitHLTOnTransitionOnly(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	clk := &testClock{t: time.Now()}
	probe := &stubProbe{name: "backend", status: StatusHealthy}
	h := NewHandler("1.0.0", clk.now(), probe)
	h.now = clk.now

	// ACT / ASSERT — first evaluation never emits.
	serve(t, h)
	rec.AssertNotEmitted(t, catalog.HLT001)

	// Unchanged after a re-run: still no emission.
	clk.advance(probeCacheTTL + time.Second)
	serve(t, h)
	rec.AssertNotEmitted(t, catalog.HLT001)

	// A real change emits exactly once.
	probe.status = StatusUnavailable

	clk.advance(probeCacheTTL + time.Second)
	serve(t, h)

	rec.AssertEmittedN(t, catalog.HLT001, 1)
	rec.AssertData(t, catalog.HLT001, "from", "healthy")
	rec.AssertData(t, catalog.HLT001, "to", "unavailable")
	rec.AssertMatchesCatalog(t)
}
