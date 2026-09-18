package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
)

// verifyPoll is how often the health endpoint is re-tried while the service
// comes up.
const verifyPoll = time.Second

// verifyStep looks at whether the service this run installed actually works.
//
// It is part of the run rather than advice at the end of it. A provisioning
// command that registered a service, started it, and never looked has reported
// success for the half of the job an operator can watch fail — and the SCM
// reporting Running means only that the process did not exit, not that it can
// reach a backend or accept a token.
type verifyStep struct{}

func (verifyStep) Name() string { return "verify" }

// Check is always Pending. Verification observes rather than converges: there
// is no state on the host that makes it unnecessary, and a dry run reporting
// "would verify" is the honest answer, since nothing has been started for it
// to look at.
func (verifyStep) Check(_ context.Context, p *Plan) (Verdict, error) {
	return pending("poll %s for up to %s and require the %q component to be healthy",
		httpserver.HealthPath, p.opts.verifyDeadline(), p.opts.HealthComponent), nil
}

func (s verifyStep) Apply(ctx context.Context, p *Plan) error {
	base := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Values.Int(config.KeyPort)))

	report, err := s.pollHealth(ctx, p, base)
	if err != nil {
		return err
	}

	p.health = report

	if !report.Reachable {
		// A failure, not an outcome: the service was started and never
		// answered, which is the one result that means the run did not do what
		// it said.
		return fmt.Errorf("%s was started but never answered %s within %s",
			p.opts.Definition.Name, httpserver.HealthPath, p.opts.verifyDeadline())
	}

	s.proveAuth(ctx, p, base)

	// An unhealthy component is NOT an error. It is what every developer
	// machine and every CI runner answers, because neither has a DHCP backend,
	// and the caller distinguishes it by exit code rather than by a failed
	// step.
	return nil
}

// pollHealth waits for the adapter to answer, then reads the named component
// out of the body.
//
// Out of the BODY, and not from the status code. Health answers 200 for
// healthy and unhealthy alike — the code says the endpoint worked, not that
// the adapter is well — so a check that asserted on the status would pass on a
// host with no backend at all and prove nothing.
func (s verifyStep) pollHealth(ctx context.Context, p *Plan, base string) (HealthReport, error) {
	report := HealthReport{Checked: true, Component: p.opts.HealthComponent}

	deadline := p.opts.now().Add(p.opts.verifyDeadline())

	for {
		resp, err := p.deps.Get(ctx, base+httpserver.HealthPath, "")
		if err == nil && resp.Status > 0 {
			report.Reachable = true

			s.readComponent(&report, resp.Body)

			return report, nil
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			return report, ctxErr
		}

		if !p.opts.now().Before(deadline) {
			return report, nil
		}

		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case <-time.After(verifyPoll):
		}
	}
}

// readComponent finds the required component in a health body.
func (verifyStep) readComponent(report *HealthReport, body []byte) {
	var parsed health.Response

	if err := json.Unmarshal(body, &parsed); err != nil {
		report.Detail = "the health response could not be parsed: " + err.Error()

		return
	}

	for _, component := range parsed.Components {
		if component.Name != report.Component {
			continue
		}

		report.ComponentHealthy = component.Status == health.StatusHealthy
		report.Detail = string(component.Status)

		if component.Detail != "" {
			report.Detail += ": " + component.Detail
		}

		return
	}

	// Named but absent is worth saying plainly. It means the binary does not
	// register the component the caller asked about, which is a wiring mistake
	// rather than an unhealthy host.
	report.Detail = fmt.Sprintf("no component named %q is registered", report.Component)
}

// proveAuth makes one authenticated call against a protected route.
//
// The predicate is "anything but 401". A host with no DHCP backend answers 502
// or 504 on a protected route, so demanding 200 would fail every developer
// machine while proving nothing extra — what is being established is that the
// token store was read through the ACL this run applied, and a 401 is the only
// answer that says it was not.
func (verifyStep) proveAuth(ctx context.Context, p *Plan, base string) {
	switch {
	case p.Values.Bool(config.KeyDisableAuth):
		p.health.AuthSkipped = true
		p.health.AuthSkipReason = config.KeyDisableAuth + " is true, so no route is authenticated"

		return

	case p.opts.ProtectedPath == "":
		p.health.AuthSkipped = true
		p.health.AuthSkipReason = "no protected route was named to test against"

		return

	case p.Token == "":
		// Nothing was minted this run, which means a usable token was already
		// there — and this run never saw it. The store keeps only hashes, so
		// there is nothing to present.
		p.health.AuthSkipped = true
		p.health.AuthSkipReason = "no token was minted this run, so none is available to present"

		return
	}

	resp, err := p.deps.Get(ctx, base+p.opts.ProtectedPath, p.Token)
	if err != nil {
		p.health.AuthSkipped = true
		p.health.AuthSkipReason = "the protected route could not be reached: " + err.Error()

		return
	}

	p.health.AuthProved = resp.Status != http.StatusUnauthorized

	if !p.health.AuthProved {
		p.health.AuthSkipReason = fmt.Sprintf(
			"%s answered 401 to the token this run minted: the service is not reading %s",
			p.opts.ProtectedPath, p.Values.String(config.KeyAuthTokensFile))
	}
}
