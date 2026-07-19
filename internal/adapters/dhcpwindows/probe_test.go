/*
Testing: probe.go

Pending:

	Running the probe against a real WS2022 host, which is what actually settles
	  that a read-only service account turns the probe green — the M3a exit
	  criterion. A fake runner can only prove the wiring.

Tested:

	Probe.Name
	  - TestProbe_ShouldReportUnderTheDocumentedComponentName: the name the health
	    exit criterion names.

	Probe.Check
	  - TestProbeCheck_ShouldReportHealthyWithOperatorFields: a good read yields
	    healthy plus the fields that end an investigation in one request.
	  - TestProbeCheck_ShouldReportUnavailableWhenTheBackendFails: every backend
	    failure mode makes the component unavailable, carrying the reason.
	  - TestProbeCheck_ShouldNotServe200WhenScopesCannotBeRead: the status maps to
	    503, so weave stops routing rather than being told a broken adapter is fine.
	  - TestProbeCheck_ShouldBoundItsOwnRuntime: the probe applies its own shorter
	    timeout rather than inheriting the caller's.
	  - TestProbeCheck_ShouldLabelAnUnsetServer: an empty identity reads as
	    "(unset)" rather than as a blank field.
	  - TestProbeCheck_ShouldEmitBackendEventOnFailure: the failure is cataloged
	    once, by the client that classified it.

	Client.probe
	  - TestClientProbe_ShouldRunTheProbeScript: the cheap query, not a full list.
	  - TestClientProbe_ShouldRejectEmptyOutput: same strictness as the list path.

Tested elsewhere:

	The script's own PS 5.1 guards: scope_test.go.
	Error classification into the typed backend errors: client_test.go.
	Overall-status aggregation and HLT-001: internal/core/health's tests.

Declined:

	A new HLT event for the component going bad. HLT-001 already covers
	  overall-status transitions and the component detail carries the rest, so a
	  second event would fire on the same transition — a ghost by duplication.
	Asserting the probe never blocks health.refresh's mutex: that is health's
	  contract, tested there. What this package owes it is a bounded Check, which
	  TestProbeCheck_ShouldBoundItsOwnRuntime covers.

Additional Remarks:

	The status choice is the load-bearing decision here, not the plumbing. A
	  backend that cannot be read makes every scopes request fail, so the
	  component reports unavailable (503) rather than unhealthy (200) — see the
	  comment on Check. TestProbeCheck_ShouldNotServe200WhenScopesCannotBeRead
	  asserts that through health's own status mapping rather than restating the
	  constant, so a change to either side has to agree.
*/
package dhcpwindows

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adapterevents "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows/events"
)

// healthyProbeOutput is what probeScript emits against a working host with
// three scopes.
const healthyProbeOutput = `{
    "scopes":  [
        { "scopeId": "192.168.178.0" },
        { "scopeId": "192.168.2.0" },
        { "scopeId": "10.0.5.0" }
    ],
    "psVersion":  "5.1.20348.558",
    "psEdition":  "Desktop"
}`

// emptyProbeOutput is a working host that simply has no scopes yet.
const emptyProbeOutput = `{
    "scopes":  [],
    "psVersion":  "5.1.20348.558",
    "psEdition":  "Desktop"
}`

// probeWith returns a probe backed by the given fake runner.
func probeWith(f *fakeRunner) *Probe {
	return &Probe{
		client:  clientWith(f),
		timeout: time.Second,
		server:  "dhcp01.example.test",
	}
}

func TestProbe_ShouldReportUnderTheDocumentedComponentName(t *testing.T) {
	t.Parallel()

	// ASSERT — the exit criterion reads "/api/v1/health shows a dhcp-server
	// component", so the name is contract, not decoration.
	assert.Equal(t, "dhcp-server", probeWith(&fakeRunner{}).Name())
}

func TestProbeCheck_ShouldReportHealthyWithOperatorFields(t *testing.T) {
	t.Parallel()

	// ARRANGE
	probe := probeWith(&fakeRunner{stdout: []byte(healthyProbeOutput)})

	// ACT
	result := probe.Check(context.Background())

	// ASSERT — green means the module is present, permissions are right, and
	// the server answered a real query.
	assert.Equal(t, health.StatusHealthy, result.Status)
	assert.Equal(t, "dhcp01.example.test", result.Fields["server"])
	assert.Equal(t, "3", result.Fields["scopeCount"])

	// The shell version is what ends a version-dependent investigation in one
	// request instead of a screen-share.
	assert.Equal(t, "5.1.20348.558", result.Fields["psVersion"])
	assert.Equal(t, "Desktop", result.Fields["psEdition"])
}

func TestProbeCheck_ShouldReportZeroScopesForAFreshlyProvisionedServer(t *testing.T) {
	t.Parallel()

	// ARRANGE — the role is installed and readable, but nothing is configured
	// yet. A working server, not a broken one.
	probe := probeWith(&fakeRunner{stdout: []byte(emptyProbeOutput)})

	// ACT
	result := probe.Check(context.Background())

	// ASSERT — healthy, and honestly zero. Counting in PowerShell risked
	// reporting 1 here, depending on whether an empty pipeline leaves
	// AutomationNull or $null behind — so the count is taken in Go, where both
	// "null" and "[]" are unambiguously length zero. An operator verifying
	// provisioning would otherwise be shown a scope that does not exist.
	assert.Equal(t, health.StatusHealthy, result.Status)
	assert.Equal(t, "0", result.Fields["scopeCount"])
}

func TestProbeCheck_ShouldSurviveACancelledCaller(t *testing.T) {
	t.Parallel()

	// ARRANGE — a caller who has already gone away, which is what an operator
	// pressing ^C on a health poll produces.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	probe := probeWith(&fakeRunner{stdout: []byte(healthyProbeOutput)})

	// ACT
	result := probe.Check(ctx)

	// ASSERT — the probe must not inherit that cancellation. health.refresh
	// caches whatever the probe returns for its whole TTL, so treating a
	// disconnected client as a backend failure would serve 503 to the next
	// unrelated poll, stop weave routing, and log a health transition caused by
	// nothing but a dropped connection.
	assert.Equal(t, health.StatusHealthy, result.Status,
		"a caller disconnecting must not mark the backend unavailable")
}

func TestProbeCheck_ShouldReportUnavailableWhenTheBackendFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fake       *fakeRunner
		wantDetail string
	}{
		{
			// The RSAT-DHCP-absent and permissions cases both land here: the
			// service is happily Running while the query fails.
			name: "the query is refused",
			fake: &fakeRunner{
				err:    exec.ErrNotFound,
				stderr: []byte("Get-DhcpServerv4Scope : Access is denied."),
			},
			wantDetail: "Access is denied",
		},
		{
			name:       "the host is wedged",
			fake:       &fakeRunner{err: ErrBackendTimeout},
			wantDetail: "timed out",
		},
		{
			name:       "the shell says nothing",
			fake:       &fakeRunner{},
			wantDetail: "no output",
		},
		{
			name:       "the shell speaks nonsense",
			fake:       &fakeRunner{stdout: []byte("not json at all")},
			wantDetail: "malformed",
		},
		{
			// Decodes cleanly into a zero probeResult. Reporting healthy off
			// this would make green mean "something answered" rather than "we
			// can read scopes".
			name:       "an empty object",
			fake:       &fakeRunner{stdout: []byte(`{}`)},
			wantDetail: "no psVersion",
		},
		{
			name:       "a bare null",
			fake:       &fakeRunner{stdout: []byte(`null`)},
			wantDetail: "no psVersion",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			result := probeWith(tc.fake).Check(context.Background())

			// ASSERT — surfaced as a health component rather than as a 500 on
			// every scopes request, and the detail carries the shell's own
			// words where it produced any.
			assert.Equal(t, health.StatusUnavailable, result.Status)
			assert.Contains(t, result.Detail, tc.wantDetail)
			assert.Equal(t, "dhcp01.example.test", result.Fields["server"])
		})
	}
}

func TestProbeCheck_ShouldNotServe200WhenScopesCannotBeRead(t *testing.T) {
	t.Parallel()

	// ARRANGE — the probe behind the real health handler, so the assertion runs
	// through health's own status mapping rather than restating a constant.
	handler := health.NewHandler("test", time.Now(), probeWith(&fakeRunner{err: exec.ErrNotFound}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	// ACT
	handler.ServeHTTP(rec, req)

	// ASSERT — 503 is what stops weave routing here and fails an orchestrator
	// readiness check. Answering 200 would have the health endpoint assert that
	// a component works while every request through it fails, which is the lie
	// this probe exists to prevent.
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body health.Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	var found bool

	for _, c := range body.Components {
		if c.Name == ComponentName {
			found = true

			assert.Equal(t, health.StatusUnavailable, c.Status)
		}
	}

	assert.True(t, found, "the health response must carry a %s component", ComponentName)
}

func TestProbeCheck_ShouldBoundItsOwnRuntime(t *testing.T) {
	t.Parallel()

	// ARRANGE — a runner that reports the deadline it was handed.
	var seen time.Duration

	fake := &fakeRunner{
		stdout: []byte(healthyProbeOutput),
		onRun: func(ctx context.Context) {
			if dl, ok := ctx.Deadline(); ok {
				seen = time.Until(dl)
			}
		},
	}

	probe := &Probe{client: clientWith(fake), timeout: 250 * time.Millisecond}

	// ACT — a caller with a much longer deadline, as a health poll would have.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	probe.Check(ctx)

	// ASSERT — the probe applies its own shorter bound rather than inheriting.
	// health.refresh holds its mutex across the check, so without this one slow
	// backend serializes every health poll behind it.
	assert.Positive(t, seen)
	assert.LessOrEqual(t, seen, 250*time.Millisecond)
}

func TestProbeCheck_ShouldLabelAnUnsetServer(t *testing.T) {
	t.Parallel()

	// ARRANGE — identity.serverName is required, so this is a defensive label
	// rather than an expected state; a blank field would read as a bug.
	probe := &Probe{
		client:  clientWith(&fakeRunner{stdout: []byte(healthyProbeOutput)}),
		timeout: time.Second,
	}

	// ACT / ASSERT
	assert.Equal(t, "(unset)", probe.Check(context.Background()).Fields["server"])
}

//nolint:paralleltest // installs the process-global emitter hook
func TestProbeCheck_ShouldEmitBackendEventOnFailure(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	defer rec.Install()()

	// ACT
	probeWith(&fakeRunner{err: exec.ErrNotFound}).Check(context.Background())

	// ASSERT — emitted by the client, which is the layer that classified the
	// failure, and labelled so a failing poll is distinguishable from a failing
	// request in the log.
	rec.AssertEmitted(t, adapterevents.BACKEND101)
	rec.AssertData(t, adapterevents.BACKEND101, "operation", opProbe)
	rec.AssertMatchesCatalog(t)
}

func TestClientProbe_ShouldRunTheProbeScript(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fake := &fakeRunner{stdout: []byte(healthyProbeOutput)}

	// ACT
	_, err := clientWith(fake).probe(context.Background())

	// ASSERT — the cheap counting query, not a full list: the probe runs on a
	// timer and has no use for the bodies.
	require.NoError(t, err)
	require.Len(t, fake.scripts, 1)
	assert.Equal(t, probeScript, fake.scripts[0])
	assert.NotEqual(t, listScopesScript, fake.scripts[0])
}

func TestClientProbe_ShouldRejectEmptyOutput(t *testing.T) {
	t.Parallel()

	// ACT
	_, err := clientWith(&fakeRunner{}).probe(context.Background())

	// ASSERT — same strictness as the list path: zero bytes is a failure, not a
	// server with zero scopes.
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBackendMalformed)
}
