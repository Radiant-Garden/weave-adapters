/*
Testing: verify.go

Pending:

Tested:
  verifyStep
    - TestVerifyStep_ShouldAlwaysBePending: verification observes; no host state makes it unnecessary.
    - TestVerifyStep_ShouldReadTheComponentOutOfTheBodyRatherThanTheStatusCode: health answers 200 either way.
    - TestVerifyStep_ShouldReportAnUnhealthyComponentWithoutFailing: the answer on every host with no backend.
    - TestVerifyStep_ShouldSayWhenTheRequiredComponentIsNotRegisteredAtAll
    - TestVerifyStep_ShouldFailWhenTheServiceNeverAnswers
    - TestVerifyStep_ShouldRetryUntilTheServiceComesUp
    - TestVerifyStep_ShouldProveTheTokenStoreIsReadByAnythingButA401
    - TestVerifyStep_ShouldReportA401AsAFailureToProveRatherThanAsProof
    - TestVerifyStep_ShouldSkipTheAuthenticatedCallWhenAuthIsDisabled
    - TestVerifyStep_ShouldSkipTheAuthenticatedCallWhenNoTokenWasMinted

Tested elsewhere:
  That health really answers 200 for an unhealthy adapter, which is the whole
  reason the body is read: internal/core/health's own tests.

  The real HTTP client this step is given: cmd/weave-adapter-dhcp-windows,
  whose httpGet builds the request and bounds it.

Declined:
  Driving a real listener. What this step decides — reachable or not, healthy
  or not, proved or not — is decision logic, and a real socket would make the
  tests slower and flakier without making any of those decisions more true.

Additional Remarks:
  Two predicates here are easy to get wrong in the direction that passes.

  The component comes from the BODY. httpStatus maps healthy and unhealthy
  alike to 200 — the code says the endpoint worked, not that the adapter is
  well — so a check asserting on the status would pass on a host with no DHCP
  backend at all and prove nothing.

  The authenticated call's predicate is "anything but 401". A dev host answers
  502 or 504 on a protected route because there is no backend behind it, so
  demanding 200 would fail every developer machine while establishing nothing
  extra. What is being proved is only that the token store was read through the
  ACL this run applied, and 401 is the single answer that says it was not.
*/

package setup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
)

// verifyPlan returns a plan ready for the verification step, with resolved
// values and a token in hand.
func verifyPlan(t *testing.T, opts Options, deps Deps, body string) *Plan {
	t.Helper()

	p := planFor(opts, deps)
	p.Values = localValues(t, body)
	p.Token = "wadapt_a-token-this-run-minted"

	return p
}

// healthBody renders a health response carrying one component.
func healthBody(t *testing.T, component string, status health.Status) []byte {
	t.Helper()

	body, err := json.Marshal(health.Response{
		Status:     status,
		Components: []health.Component{{Name: component, Status: status, Detail: "the detail"}},
	})
	require.NoError(t, err)

	return body
}

func TestVerifyStep_ShouldAlwaysBePending(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()

	// ACT
	verdict, err := verifyStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// Verification observes rather than converges: there is no state on the
	// host that makes it unnecessary, and a dry run reporting "would verify"
	// is honest, since nothing has been started for it to look at.
	require.NoError(t, err)
	assert.Equal(t, Pending, verdict.Condition)
	assert.Contains(t, verdict.Detail, httpserver.HealthPath)
	assert.Contains(t, verdict.Detail, opts.HealthComponent)
}

func TestVerifyStep_ShouldReadTheComponentOutOfTheBodyRatherThanTheStatusCode(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()
	deps.Get = func(_ context.Context, url, _ string) (Response, error) {
		if strings.Contains(url, httpserver.HealthPath) {
			return Response{Status: http.StatusOK, Body: healthBody(t, opts.HealthComponent, health.StatusHealthy)}, nil
		}

		return Response{Status: http.StatusBadGateway}, nil
	}

	p := verifyPlan(t, opts, deps, "authTokensFile = 'x'\n")

	// ACT
	require.NoError(t, verifyStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.True(t, p.health.Reachable)
	assert.True(t, p.health.ComponentHealthy)
	assert.Equal(t, opts.HealthComponent, p.health.Component)
}

func TestVerifyStep_ShouldReportAnUnhealthyComponentWithoutFailing(t *testing.T) {
	t.Parallel()

	// ARRANGE — 200, and unhealthy. That combination is the point.
	opts := runOptions(t)
	deps, _, _ := okDeps()
	deps.Get = func(_ context.Context, url, _ string) (Response, error) {
		if strings.Contains(url, httpserver.HealthPath) {
			return Response{
				Status: http.StatusOK,
				Body:   healthBody(t, opts.HealthComponent, health.StatusUnhealthy),
			}, nil
		}

		return Response{Status: http.StatusGatewayTimeout}, nil
	}

	p := verifyPlan(t, opts, deps, "authTokensFile = 'x'\n")

	// ACT
	err := verifyStep{}.Apply(context.Background(), p)

	// ASSERT
	// NOT an error. It is what every developer machine and every CI runner
	// answers, because neither has a DHCP backend, and the caller tells it
	// from a failed step by exit code rather than by a failed step.
	require.NoError(t, err)
	assert.True(t, p.health.Reachable)
	assert.False(t, p.health.ComponentHealthy)
	assert.Contains(t, p.health.Detail, string(health.StatusUnhealthy))
}

func TestVerifyStep_ShouldSayWhenTheRequiredComponentIsNotRegisteredAtAll(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()
	deps.Get = func(_ context.Context, url, _ string) (Response, error) {
		if strings.Contains(url, httpserver.HealthPath) {
			return Response{Status: http.StatusOK, Body: healthBody(t, "something-else", health.StatusHealthy)}, nil
		}

		return Response{Status: http.StatusBadGateway}, nil
	}

	p := verifyPlan(t, opts, deps, "authTokensFile = 'x'\n")

	// ACT
	require.NoError(t, verifyStep{}.Apply(context.Background(), p))

	// ASSERT
	// Named but absent is a wiring mistake in the binary, not an unhealthy
	// host, and it would otherwise read as the same thing.
	assert.False(t, p.health.ComponentHealthy)
	assert.Contains(t, p.health.Detail, "no component named")
}

func TestVerifyStep_ShouldFailWhenTheServiceNeverAnswers(t *testing.T) {
	t.Parallel()

	// ARRANGE
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)

	opts := runOptions(t)
	opts.VerifyDeadline = time.Millisecond
	opts.Now = func() time.Time {
		now = now.Add(time.Hour) // past the deadline on the second read.

		return now
	}

	deps, _, _ := okDeps()
	deps.Get = func(context.Context, string, string) (Response, error) {
		return Response{}, errors.New("connection refused")
	}

	p := verifyPlan(t, opts, deps, "authTokensFile = 'x'\n")

	// ACT
	err := verifyStep{}.Apply(context.Background(), p)

	// ASSERT
	// This one IS a failure: the service was started and never answered, which
	// is the single result meaning the run did not do what it said.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "never answered")
	assert.False(t, p.health.Reachable)
}

func TestVerifyStep_ShouldRetryUntilTheServiceComesUp(t *testing.T) {
	t.Parallel()

	// ARRANGE — refused twice, then healthy, as a service coming up does.
	opts := runOptions(t)
	opts.VerifyDeadline = time.Minute

	var attempts int

	deps, _, _ := okDeps()
	deps.Get = func(_ context.Context, url, _ string) (Response, error) {
		if !strings.Contains(url, httpserver.HealthPath) {
			return Response{Status: http.StatusBadGateway}, nil
		}

		attempts++
		if attempts < 3 {
			return Response{}, errors.New("connection refused")
		}

		return Response{Status: http.StatusOK, Body: healthBody(t, opts.HealthComponent, health.StatusHealthy)}, nil
	}

	p := verifyPlan(t, opts, deps, "authTokensFile = 'x'\n")

	// ACT
	require.NoError(t, verifyStep{}.Apply(context.Background(), p))

	// ASSERT
	// A service does not answer the instant the SCM reports Running, so a
	// single attempt would fail every correct installation.
	assert.GreaterOrEqual(t, attempts, 3)
	assert.True(t, p.health.Reachable)
}

func TestVerifyStep_ShouldProveTheTokenStoreIsReadByAnythingButA401(t *testing.T) {
	t.Parallel()

	tests := map[string]int{
		"should accept a backend failure": http.StatusBadGateway,
		"should accept a backend timeout": http.StatusGatewayTimeout,
		"should accept a successful read": http.StatusOK,
		"should accept a not-found route": http.StatusNotFound,
	}

	for name, status := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			opts := runOptions(t)
			deps, _, _ := okDeps()

			var presented string

			deps.Get = func(_ context.Context, url, bearer string) (Response, error) {
				if strings.Contains(url, httpserver.HealthPath) {
					return Response{
						Status: http.StatusOK,
						Body:   healthBody(t, opts.HealthComponent, health.StatusHealthy),
					}, nil
				}

				presented = bearer

				return Response{Status: status}, nil
			}

			p := verifyPlan(t, opts, deps, "authTokensFile = 'x'\n")

			// ACT
			require.NoError(t, verifyStep{}.Apply(context.Background(), p))

			// ASSERT
			// A dev host answers 502 or 504 on a protected route because there
			// is no backend behind it. Demanding 200 would fail every
			// developer machine while establishing nothing extra: what is
			// being proved is that the store was read, and only a 401 says it
			// was not.
			assert.True(t, p.health.AuthProved)
			assert.Equal(t, p.Token, presented, "the token this run minted was not the one presented")
		})
	}
}

func TestVerifyStep_ShouldReportA401AsAFailureToProveRatherThanAsProof(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()
	deps.Get = func(_ context.Context, url, _ string) (Response, error) {
		if strings.Contains(url, httpserver.HealthPath) {
			return Response{Status: http.StatusOK, Body: healthBody(t, opts.HealthComponent, health.StatusHealthy)}, nil
		}

		return Response{Status: http.StatusUnauthorized}, nil
	}

	p := verifyPlan(t, opts, deps, "authTokensFile = 'the-store'\n")

	// ACT
	require.NoError(t, verifyStep{}.Apply(context.Background(), p))

	// ASSERT
	// The service is running and rejecting the credential this run just
	// created, which means it is not reading the store this run wrote — the
	// one failure a green "installed and running" would otherwise hide.
	assert.False(t, p.health.AuthProved)
	assert.Contains(t, p.health.AuthSkipReason, "401")
	assert.Contains(t, p.health.AuthSkipReason, "the-store")
}

func TestVerifyStep_ShouldSkipTheAuthenticatedCallWhenAuthIsDisabled(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()

	p := verifyPlan(t, opts, deps, "disableAuth = true\nauthTokensFile = 'x'\n")

	// ACT
	require.NoError(t, verifyStep{}.Apply(context.Background(), p))

	// ASSERT
	// Nothing is authenticated, so there is nothing to prove — and reporting
	// a pass would be claiming a check that never happened.
	assert.True(t, p.health.AuthSkipped)
	assert.False(t, p.health.AuthProved)
	assert.Contains(t, p.health.AuthSkipReason, config.KeyDisableAuth)
}

func TestVerifyStep_ShouldSkipTheAuthenticatedCallWhenNoTokenWasMinted(t *testing.T) {
	t.Parallel()

	// ARRANGE — a usable token was already in the store, so this run never
	// saw one: the store keeps only hashes.
	opts := runOptions(t)
	deps, _, _ := okDeps()

	p := verifyPlan(t, opts, deps, "authTokensFile = 'x'\n")
	p.Token = ""

	// ACT
	require.NoError(t, verifyStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.True(t, p.health.AuthSkipped)
	assert.Contains(t, p.health.AuthSkipReason, "no token was minted")
}
