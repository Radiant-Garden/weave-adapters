package dhcpwindows

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/health"
)

// ComponentName is the name this adapter's backend reports under in the health
// response.
const ComponentName = "dhcp-server"

// probeResult is what probeScript emits.
//
// Scopes is a slice rather than a count so the counting happens here: `null`
// and `[]` both unmarshal to length zero, which removes the PowerShell
// ambiguity described on probeScript.
type probeResult struct {
	Scopes    []struct{} `json:"scopes"`
	PSVersion string     `json:"psVersion"`
	PSEdition string     `json:"psEdition"`
}

// Probe reports whether the DHCP backend can actually be read. It satisfies
// health.Probe, which has been waiting for a backend since M1.
type Probe struct {
	client  *Client
	timeout time.Duration

	// target is the connection target (dhcp.server), empty for the local host.
	// identity is the provisioned name every wadaptID derives from
	// (identity.serverName).
	//
	// Two fields because they answer two different questions and are separately
	// configurable. Reporting one under a label that reads like the other is not
	// a cosmetic slip: a probe that times out while showing an identity name in
	// a field called "server" sends whoever reads it hunting a network path to a
	// host nothing ever tried to reach.
	target   string
	identity string
}

// NewProbe returns a health probe for the given client.
//
// timeout is deliberately its own setting (dhcp.probeTimeout) and shorter than
// the general command timeout. health.refresh holds its mutex across the whole
// check, so concurrent health requests serialize behind a re-run — a probe
// bounded only by the general timeout would let one slow powershell.exe stall
// every health poll. The runner's WaitDelay is the other half of that promise:
// without it the deadline bounds the process but not the call.
func NewProbe(client *Client, cfg Config) *Probe {
	return &Probe{
		client:   client,
		timeout:  cfg.ProbeTimeout,
		target:   cfg.Server,
		identity: cfg.ServerName,
	}
}

// Name identifies the component in the health response.
func (p *Probe) Name() string { return ComponentName }

// Check runs one real query against the backend.
//
// A failure reports unavailable rather than unhealthy, and the distinction is
// deliberate. health.httpStatus serves 200 for unhealthy and 503 for
// unavailable, and 503 is what tells weave and an orchestrator readiness check
// to stop routing here. If scopes cannot be read, every scopes request will
// fail — answering 200 would make the health endpoint assert that a component
// works while it does not, which is the exact lie this probe exists to prevent.
//
// The plan's risk table words this as "unhealthy rather than a 500"; that
// contrast is with a 500 on the resource endpoint, and the vocabulary's own
// definitions (unhealthy is "degraded but reachable") make unavailable the
// right member for "cannot read at all".
func (p *Probe) Check(ctx context.Context) health.Result {
	// Detached from the caller's cancellation, then given our own deadline.
	//
	// health.refresh passes the *request* context down and caches whatever comes
	// back for probeCacheTTL. Without this, an operator pressing ^C on a health
	// poll cancels the probe, the cancellation is classified as a backend
	// failure, and that verdict is cached — so the next unrelated poll within
	// the TTL is served 503, weave stops routing, and the log shows a health
	// transition caused by nothing but a disconnected client. Values and
	// deadlines are inherited; only the cancellation is dropped.
	ctx = context.WithoutCancel(ctx)

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	result, err := p.client.probe(ctx)
	if err != nil {
		// The error already carries its classification and the shell's own
		// words; the probe adds the identity of what it could not reach.
		return health.Result{
			Status: health.StatusUnavailable,
			Detail: err.Error(),
			Fields: p.operatorFields(),
		}
	}

	return health.Result{
		Status: health.StatusHealthy,
		Detail: "dhcp server reachable and scopes readable",
		Fields: withFields(p.operatorFields(),
			"scopeCount", strconv.Itoa(len(result.Scopes)),
			"psVersion", result.PSVersion,
			"psEdition", result.PSEdition,
		),
	}
}

// operatorFields are the fields every result carries, healthy or not.
//
// Both are here because a failing probe is exactly when the difference matters.
// "server" answers "who did we ask?", which is what an unreachable backend sends
// someone to check. "identity" answers "whose IDs are we deriving?", which is
// what explains a fleet of wadaptIDs that changed without any scope changing —
// and it is otherwise visible only in the DHCP-001 line at startup, long
// scrolled away by the time anyone polls health.
func (p *Probe) operatorFields() map[string]string {
	return map[string]string{
		"server":   p.targetLabel(),
		"identity": p.identity,
	}
}

// targetLabel names the host the query is sent to.
//
// An empty dhcp.server means the local machine, which is the default and the
// common case. Rendering that as "" would read as "unconfigured" in a health
// response, so it says what it means.
func (p *Probe) targetLabel() string {
	if p.target == "" {
		return "(local host)"
	}

	return p.target
}

// withFields returns fields extended with the given key/value pairs. An odd
// trailing key is dropped rather than panicking: a malformed diagnostic must not
// take down the health endpoint it was describing.
func withFields(fields map[string]string, pairs ...string) map[string]string {
	for i := 0; i+1 < len(pairs); i += 2 {
		fields[pairs[i]] = pairs[i+1]
	}

	return fields
}

// probe runs the health query. It lives on Client rather than on Probe so that
// every backend call goes through one place — the same classification, the same
// stderr handling, and the same event on failure.
func (c *Client) probe(ctx context.Context) (probeResult, error) {
	stdout, stderr, err := c.runner.run(ctx, probeScript)
	if err != nil {
		return probeResult{}, c.backendError(ctx, opProbe, runError(err, stderr))
	}

	if strings.TrimSpace(string(stdout)) == "" {
		return probeResult{}, c.backendError(ctx, opProbe,
			fmt.Errorf("%w: no output%s", ErrBackendMalformed, stderrContext(stderr)))
	}

	var result probeResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return probeResult{}, c.backendError(ctx, opProbe,
			fmt.Errorf("%w: %w%s", ErrBackendMalformed, err, stderrContext(stderr)))
	}

	// A payload that decodes but says nothing is not a successful probe.
	//
	// "null", "{}", or any object without the expected keys unmarshals into a
	// zero probeResult with no error — and reporting *healthy* off that is
	// precisely the lie Check exists to prevent: green would mean "something
	// answered" rather than "we can read scopes". psVersion is the guard because
	// probeScript always emits it, on every host, whatever the scope count. This
	// mirrors the same strictness decodeScopes applies on the list path.
	if result.PSVersion == "" {
		return probeResult{}, c.backendError(ctx, opProbe,
			fmt.Errorf("%w: no psVersion in the probe payload%s", ErrBackendMalformed, stderrContext(stderr)))
	}

	return result, nil
}
