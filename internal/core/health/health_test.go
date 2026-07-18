/*
Testing: health.go

Pending:

Tested:
  NewHandler / Handler.runProbes
    - TestHandler_ShouldPrependCoreProbe: core leads the adapter probes, and each
      probe's Detail/Fields survive into its component.
  Handler.ServeHTTP / evaluate / runProbes
    - TestHandler_ShouldReturnWeaveShape: 200, no-cache, weave-shaped body with the core component.
    - TestHandler_ShouldReportUptime: uptime is whole seconds since started, truncated.
    - TestHandler_Should503WhenUnavailable: an unavailable probe yields 503.
    - TestHandler_ShouldCacheProbeResults: probes run at most once per TTL.
    - TestHandler_ShouldReRunProbesAfterTTL: probes re-run once the TTL elapses.
    - TestHandler_ShouldServeConcurrently: concurrent ServeHTTP calls are race-free.
    - TestHandler_ShouldEmitHLTOnTransitionOnly: HLT-001 fires only on a status
      change, with correct from/to across successive transitions.

Tested elsewhere:

Declined:
  Within-TTL status flip producing no HLT-001: the composition of two mechanics
    that are each already pinned — TestHandler_ShouldCacheProbeResults owns "no
    re-run inside the TTL", and the transition test owns "emission follows a
    change in the evaluated components". Asserting the composition would give
    the transition test a second concept without covering a blind path.
  Slow-probe serialization under the refresh lock: refresh holds the mutex
    across every Check, so concurrent requests queue behind a re-run. This is a
    deliberate M1 tradeoff (see refresh's doc comment) and unobservable while
    coreProbe is the only probe. Deferred to M3, when the first backend probe
    lands — revisit alongside running probes outside the lock.

Additional Remarks:
  The transition test installs the global emitter hook (recorder) and drives an
  injected clock, so it runs sequentially. The other tests use their own Handler
  and never transition, so they stay parallel-safe.

  stubProbe.calls is atomic so the concurrency test can read it without racing;
  its other fields are only written during ARRANGE, or between sequential serves
  in the transition test, so they need no synchronization. The concurrency test
  uses the real clock — testClock is deliberately not concurrency-safe, and
  ServeHTTP reads h.now() outside the handler mutex.
*/

package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubProbe is a controllable probe that counts its checks. calls is atomic so
// the concurrency test can read it while probes run.
type stubProbe struct {
	name   string
	status Status
	detail string
	fields map[string]string
	calls  atomic.Int64
}

func (p *stubProbe) Name() string { return p.name }

func (p *stubProbe) Check(context.Context) Result {
	p.calls.Add(1)

	return Result{Status: p.status, Detail: p.detail, Fields: p.fields}
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

func TestHandler_ShouldReportUptime(t *testing.T) {
	t.Parallel()

	// ARRANGE
	clk := &testClock{t: time.Now()}
	h := NewHandler("1.0.0", clk.now())
	h.now = clk.now

	// ACT — a fractional second must truncate down, not round.
	clk.advance(90*time.Second + 500*time.Millisecond)

	rw := serve(t, h)

	// ASSERT
	var resp Response

	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &resp))
	assert.Equal(t, int64(90), resp.UptimeSeconds)
}

func TestHandler_ShouldPrependCoreProbe(t *testing.T) {
	t.Parallel()

	// ARRANGE — an adapter probe carrying the detail/fields weave shows operators.
	backend := &stubProbe{
		name:   "backend",
		status: StatusUnhealthy,
		detail: "lease pool nearly exhausted",
		fields: map[string]string{"scope": "10.0.0.0/24", "free": "3"},
	}

	// ACT
	rw := serve(t, NewHandler("1.0.0", time.Now(), backend))

	// ASSERT — core is always present, and leads the adapter probes.
	var resp Response

	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &resp))
	require.Len(t, resp.Components, 2)
	assert.Equal(t, "core", resp.Components[0].Name)
	assert.Equal(t, "backend", resp.Components[1].Name)

	// Detail and Fields survive the probe -> component -> JSON round trip.
	assert.Equal(t, "adapter core running", resp.Components[0].Detail)
	assert.Equal(t, "lease pool nearly exhausted", resp.Components[1].Detail)
	assert.Equal(t, map[string]string{"scope": "10.0.0.0/24", "free": "3"}, resp.Components[1].Fields)
}

func TestHandler_ShouldServeConcurrently(t *testing.T) {
	t.Parallel()

	// ARRANGE — the real clock: testClock is not safe for concurrent reads, and
	// ServeHTTP calls h.now() outside the handler mutex.
	const callers = 32

	probe := &stubProbe{name: "backend", status: StatusHealthy}
	h := NewHandler("1.0.0", time.Now(), probe)

	codes := make([]int, callers)

	var wg sync.WaitGroup

	// ACT — hammer one handler from many goroutines (the suite runs with -race).
	for i := range callers {
		wg.Go(func() {
			codes[i] = serve(t, h).Code
		})
	}

	wg.Wait()

	// ASSERT — every caller got a well-formed response and the probe ran.
	for _, code := range codes {
		assert.Equal(t, http.StatusOK, code)
	}

	assert.Positive(t, probe.calls.Load())
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
	assert.Equal(t, int64(1), probe.calls.Load())
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
	assert.Equal(t, int64(2), probe.calls.Load())
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

	// Recovery is a second transition, and reports the prior status as from —
	// so the emission updated the remembered status, not just read it.
	probe.status = StatusHealthy

	clk.advance(probeCacheTTL + time.Second)
	serve(t, h)

	emitted := rec.FindByID(catalog.HLT001)
	require.Len(t, emitted, 2)
	assert.Equal(t, "healthy", emitted[0].Data("from"))
	assert.Equal(t, "unavailable", emitted[0].Data("to"))
	assert.Equal(t, "unavailable", emitted[1].Data("from"))
	assert.Equal(t, "healthy", emitted[1].Data("to"))
	rec.AssertMatchesCatalog(t)
}
