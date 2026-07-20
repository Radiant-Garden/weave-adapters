//go:build e2e

/*
Testing: the whole read path against a real DHCP backend

Pending:

	A multi-scope host. Several assertions below (ordering, the pagination walk,
	"no duplicates") are only load-bearing with two or more scopes; on a
	single-scope server they pass vacuously. TestE2E_ShouldServeScopes says so in
	its log output rather than pretending otherwise.

Tested:

	the green path, which no other gate can observe
	  - TestE2E_ShouldReportTheBackendHealthy: dhcp-server is healthy, not merely present.
	  - TestE2E_ShouldServeScopes: every scope carries a unique wadaptID, in wadaptID order.
	  - TestE2E_ShouldWalkEveryScopeExactlyOnceViaNextPageUrl: the link form weave follows.
	  - TestE2E_ShouldAnswer304OnAnUnchangedRepresentation: the ETag saves a body.
	  - TestE2E_ShouldRejectAnonymousAndMalformedRequests: 401 and 400, in problem+json.
	  - TestE2E_ShouldFilterWithoutDisturbingTheCollection: ?scopeId= narrows, and does not corrupt.
	  - TestE2E_ShouldDeriveStableIdentitiesAcrossARestart: the same host, read twice, yields the same IDs.
	  - TestE2E_ShouldServeTheOpenAPIContract: the embedded spec reaches the wire.

Tested elsewhere:

	That the artifact starts, serves and shuts down cleanly: smoke_test.go, which
	runs everywhere and needs no backend. This file is the opposite trade — it
	needs a real DHCP server and therefore only runs on the WS2022 gate.

	Handler logic in isolation, including the failure paths a healthy host cannot
	produce: internal/adapters/dhcpwindows/scopes_test.go.

Declined:

	Asserting a scope count, or any particular scope. Both are properties of the
	host, and the gate would then fail whenever someone edited DHCP rather than
	when the adapter broke.

	Provoking 502/504 by breaking the backend. Stopping the DHCP service on a
	shared host to satisfy a test is not a trade worth making; the mapping is
	covered against fakes where it can be exercised precisely.

Additional Remarks:

	THIS GATE REQUIRES A REACHABLE DHCP SERVER AND READ RIGHTS. If the backend is
	unavailable it FAILS rather than skipping, which is deliberate: the whole
	reason this file exists is that CI had never once observed a green adapter
	end to end, and a gate that quietly skips when the thing it verifies is
	absent proves nothing while looking like it passed. The failure message names
	the likely causes.

	Set WEAVE_ADAPTER_DHCP_POWERSHELL_PATH to a stub that replays a captured
	fixture to develop these tests off-host — that is an existing config knob,
	not a fake compiled into production code.

	The identity inputs come from harness_test.go's startAdapter and are FIXED
	constants. TestE2E_ShouldDeriveStableIdentitiesAcrossARestart compares
	wadaptIDs across two processes, so randomising them per run would make that
	assertion pass vacuously.
*/
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wadaptIDShape is the published contract: 8 bytes of HMAC output encoded in the
// RFC 4648 §7 base32hex alphabet, which is always 13 characters. weave's own
// keyShape regexp pins the same thing, so a change here is a breaking change
// there.
var wadaptIDShape = regexp.MustCompile(`^[0-9a-v]{13}$`)

// scope mirrors the served representation. Declared here rather than imported
// from the adapter package on purpose: this gate is a client, and a client that
// shares the server's struct cannot notice the server renaming a field.
type scope struct {
	WadaptID             string `json:"wadaptId"`
	ScopeID              string `json:"scopeId"`
	SubnetMask           string `json:"subnetMask"`
	StartRange           string `json:"startRange"`
	EndRange             string `json:"endRange"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	State                string `json:"state"`
	Type                 string `json:"type"`
	SuperscopeName       string `json:"superscopeName"`
	LeaseDurationSeconds int    `json:"leaseDurationSeconds"`
	AddressFamily        string `json:"addressFamily"`
}

type scopePage struct {
	Items         []scope `json:"items"`
	NextPageToken string  `json:"nextPageToken"`
	NextPageURL   string  `json:"nextPageUrl"`
}

// adapter is a started binary plus what a client needs to talk to it.
type adapter struct {
	base  string
	token string
}

// startE2E builds the binary, mints a token through the real CLI, and starts the
// process against whatever backend the host provides.
func startE2E(t *testing.T) *adapter {
	t.Helper()

	binary := buildAdapter(t)
	store := filepath.Join(t.TempDir(), "tokens.toml")
	token := mintToken(t, binary, store)
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	startAdapter(t, binary, port, store)
	waitReady(t, base+"/api/v1/health")

	return &adapter{base: base, token: token}
}

// requireHealthyBackend fails with a diagnosis rather than letting every later
// test fail obscurely.
//
// It reads the health endpoint, which is the cheapest place to learn whether the
// backend is reachable at all, and reports the component's own detail — that
// carries the shell's error and the PowerShell version, which is usually enough
// to end the investigation without opening a console on the host.
func (a *adapter) requireHealthyBackend(t *testing.T) {
	t.Helper()

	status, body := get(t, a.base+"/api/v1/health", "")

	var health struct {
		Status     string `json:"status"`
		Components []struct {
			Name   string            `json:"name"`
			Status string            `json:"status"`
			Detail string            `json:"detail"`
			Fields map[string]string `json:"fields"`
		} `json:"components"`
	}

	require.NoError(t, json.Unmarshal(body, &health), "health payload: %s", body)

	for _, c := range health.Components {
		if c.Name != "dhcp-server" {
			continue
		}

		if c.Status == "healthy" {
			t.Logf("backend healthy: %s %v", c.Detail, c.Fields)

			return
		}

		t.Fatalf(`the dhcp-server component is %q, so this gate cannot run.

detail: %s
fields: %v

This gate exists to observe the adapter serving REAL scopes, so it fails rather
than skipping. Likely causes, in the order worth checking:
  - the RSAT-DHCP feature is not installed, so the DhcpServer module is missing
    (Install-WindowsFeature RSAT-DHCP)
  - the account this runner executes as lacks DHCP read rights (add it to the
    DHCP Users group) -- note the runner is NOT the interactive administrator
  - this host does not run the DHCP Server role at all, in which case this gate
    belongs on a different runner
Health said %d.`, c.Status, c.Detail, c.Fields, status)
	}

	t.Fatalf("no dhcp-server component in the health response — the probe is not wired into this build: %s", body)
}

// listScopes fetches one page and decodes it, failing on any non-200.
func (a *adapter) listScopes(t *testing.T, query string) scopePage {
	t.Helper()

	target := a.base + "/api/v1/scopes"
	if query != "" {
		target += "?" + query
	}

	status, body := get(t, target, a.token)
	require.Equal(t, http.StatusOK, status, "GET %s: %s", target, body)

	var page scopePage
	require.NoError(t, json.Unmarshal(body, &page), "scopes payload: %s", body)

	return page
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldReportTheBackendHealthy(t *testing.T) {
	// ARRANGE / ACT
	a := startE2E(t)

	// ASSERT — the green path, which is the one thing no other gate in this repo
	// has ever observed: unit tests run against a fake runner, and the smoke gate
	// deliberately accepts either verdict because it runs where there is no DHCP.
	a.requireHealthyBackend(t)
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldServeScopes(t *testing.T) {
	// ARRANGE
	a := startE2E(t)
	a.requireHealthyBackend(t)

	// ACT
	page := a.listScopes(t, "")

	// ASSERT — the milestone's central invariant, on real data: every scope the
	// API serves carries a wadaptID, and no two share one.
	require.NotEmpty(t, page.Items, "the host has no scopes, so this gate proves nothing about identity")

	seen := map[string]string{}

	for _, s := range page.Items {
		assert.Regexp(t, wadaptIDShape, s.WadaptID, "scope %s has no usable wadaptId", s.ScopeID)
		assert.Equal(t, "ipv4", s.AddressFamily)
		assert.NotEmpty(t, s.ScopeID)

		if other, dup := seen[s.WadaptID]; dup {
			t.Errorf("scopes %s and %s share wadaptId %s — identity is not unique", other, s.ScopeID, s.WadaptID)
		}

		seen[s.WadaptID] = s.ScopeID
	}

	// Served in wadaptID order, which is the order the cursor resumes in. Sorting
	// by one key and resuming on another skips and repeats pages in silence.
	ids := make([]string, 0, len(page.Items))
	for _, s := range page.Items {
		ids = append(ids, s.WadaptID)
	}

	assert.True(t, slices.IsSorted(ids), "collection is not in wadaptId order: %v", ids)

	if len(page.Items) < 2 {
		t.Logf("NOTE: this host has %d scope, so uniqueness and ordering pass vacuously. "+
			"Add a second scope to make this gate meaningful.", len(page.Items))
	}
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldWalkEveryScopeExactlyOnceViaNextPageUrl(t *testing.T) {
	// ARRANGE — nextPageUrl is the load-bearing cursor form: weave's list walker
	// follows links and cannot echo a token, so a collection it cannot walk this
	// way cannot be paged by weave at all.
	a := startE2E(t)
	a.requireHealthyBackend(t)

	all := a.listScopes(t, "")
	require.NotEmpty(t, all.Items)

	// ACT — one scope per page, so even a two-scope host exercises a real walk.
	var (
		seen  []string
		query = "pageSize=1"
		pages int
	)

	for {
		page := a.listScopes(t, query)
		pages++

		for _, s := range page.Items {
			seen = append(seen, s.WadaptID)
		}

		if page.NextPageURL == "" {
			assert.Empty(t, page.NextPageToken, "cursor forms must be absent together")

			break
		}

		assert.NotEmpty(t, page.NextPageToken, "cursor forms must be present together")

		next, err := url.Parse(page.NextPageURL)
		require.NoError(t, err, "nextPageUrl is not a URL: %q", page.NextPageURL)
		assert.False(t, next.IsAbs(), "nextPageUrl must be relative: %q", page.NextPageURL)
		assert.Equal(t, "/api/v1/scopes", next.Path)
		assert.Equal(t, "1", next.Query().Get("pageSize"), "nextPageUrl dropped a query parameter")

		query = next.RawQuery

		require.LessOrEqual(t, pages, len(all.Items)+1, "the walk did not terminate")
	}

	// ASSERT — every scope exactly once, in the same order the full listing gave.
	// No omissions and no repeats is the property a resume key buys over an
	// offset, and it is an exit criterion in its own right.
	want := make([]string, 0, len(all.Items))
	for _, s := range all.Items {
		want = append(want, s.WadaptID)
	}

	assert.Equal(t, want, seen)
	assert.Equal(t, len(all.Items), pages, "one page per scope at pageSize=1")
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldAnswer304OnAnUnchangedRepresentation(t *testing.T) {
	// ARRANGE
	a := startE2E(t)
	a.requireHealthyBackend(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.base+"/api/v1/scopes", nil)
	require.NoError(t, err)

	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := httpClient().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	tag := resp.Header.Get("ETag")
	require.NotEmpty(t, tag, "a JSON collection must be tagged or polling it costs a full body every time")

	// ACT
	conditional, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.base+"/api/v1/scopes", nil)
	require.NoError(t, err)

	conditional.Header.Set("Authorization", "Bearer "+a.token)
	conditional.Header.Set("If-None-Match", tag)

	second, err := httpClient().Do(conditional)
	require.NoError(t, err)

	defer func() { _ = second.Body.Close() }()

	// ASSERT
	assert.Equal(t, http.StatusNotModified, second.StatusCode)
	assert.Equal(t, tag, second.Header.Get("ETag"))

	// Authentication runs OUTSIDE the conditional wrapper, so a valid
	// If-None-Match presented without credentials is a rejection, not a cache
	// hit. Getting this backwards would let an anonymous caller confirm whether
	// a representation had changed.
	anon, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.base+"/api/v1/scopes", nil)
	require.NoError(t, err)

	anon.Header.Set("If-None-Match", tag)

	anonResp, err := httpClient().Do(anon)
	require.NoError(t, err)

	defer func() { _ = anonResp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, anonResp.StatusCode, "401 must beat 304")
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldRejectAnonymousAndMalformedRequests(t *testing.T) {
	// ARRANGE
	a := startE2E(t)

	// ACT / ASSERT — anonymous first. Every route that is not health or the spec
	// authenticates, including paths matching no route, so an anonymous caller
	// cannot enumerate what exists.
	status, body := get(t, a.base+"/api/v1/scopes", "")
	assert.Equal(t, http.StatusUnauthorized, status, "body: %s", body)

	var problem struct {
		Type      string `json:"type"`
		Status    int    `json:"status"`
		RequestID string `json:"requestId"`
		Errors    []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}

	require.NoError(t, json.Unmarshal(body, &problem), "401 body: %s", body)
	assert.Equal(t, "weave-adapters:unauthorized", problem.Type)
	assert.Equal(t, http.StatusUnauthorized, problem.Status, "RFC 9457: the body mirrors the wire status")
	assert.NotEmpty(t, problem.RequestID, "a rejection must still be correlatable with the log")

	// A malformed filter is a 400 naming the field, not a cheerful empty 200 —
	// answering 200 would tell a client its filter worked and the server holds no
	// such scope, which is a different and wrong claim.
	status, body = get(t, a.base+"/api/v1/scopes?scopeId=not-an-address", a.token)
	require.Equal(t, http.StatusBadRequest, status, "body: %s", body)
	require.NoError(t, json.Unmarshal(body, &problem))
	assert.Equal(t, "weave-adapters:validation-failed", problem.Type)
	require.NotEmpty(t, problem.Errors)
	assert.Equal(t, "scopeId", problem.Errors[0].Field)

	// A leading-zero address is also a 400: it reads as octal in some resolvers
	// and decimal in others, an ambiguity behind real SSRF bypasses.
	status, _ = get(t, a.base+"/api/v1/scopes?scopeId=010.000.000.000", a.token)
	assert.Equal(t, http.StatusBadRequest, status, "an ambiguous address must not be guessed at")
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldFilterWithoutDisturbingTheCollection(t *testing.T) {
	// ARRANGE
	a := startE2E(t)
	a.requireHealthyBackend(t)

	before := a.listScopes(t, "")
	require.NotEmpty(t, before.Items)

	target := before.Items[0].ScopeID

	// ACT
	filtered := a.listScopes(t, "scopeId="+url.QueryEscape(target))

	// ASSERT — exact equality, and at most one match because Windows permits one
	// scope per subnet.
	require.Len(t, filtered.Items, 1)
	assert.Equal(t, target, filtered.Items[0].ScopeID)
	assert.Equal(t, before.Items[0].WadaptID, filtered.Items[0].WadaptID)

	// A filter matching nothing is an empty page, not a 404: the collection
	// exists, it just has no member on that subnet.
	empty := a.listScopes(t, "scopeId=203.0.113.0")
	assert.Empty(t, empty.Items, "TEST-NET-3 should not be a scope on this host")

	// And filtering must not have disturbed the collection. An earlier build
	// filtered in place and wrote through to the listing the backend returned,
	// after which later requests served scopes with an empty scopeId and an
	// empty wadaptId.
	after := a.listScopes(t, "")
	assert.Equal(t, before.Items, after.Items, "filtering changed what an unfiltered read returns")
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldDeriveStableIdentitiesAcrossARestart(t *testing.T) {
	// ARRANGE — the same host, read twice, across two processes. Derivation is
	// stable or it is not an identity: a wadaptID that changed on restart would
	// make weave see every scope as gone and propose a recreate for each.
	first := startE2E(t)
	first.requireHealthyBackend(t)

	before := first.listScopes(t, "")
	require.NotEmpty(t, before.Items)

	// ACT — a second process, same provisioned identity inputs, fresh port and
	// token store. Nothing persists between them but the configuration.
	second := startE2E(t)
	second.requireHealthyBackend(t)

	after := second.listScopes(t, "")

	// ASSERT
	require.Len(t, after.Items, len(before.Items))

	for i := range before.Items {
		assert.Equal(t, before.Items[i].WadaptID, after.Items[i].WadaptID,
			"scope %s derived a different identity after a restart", before.Items[i].ScopeID)
	}
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldServeTheOpenAPIContract(t *testing.T) {
	// ARRANGE
	a := startE2E(t)

	// ACT — unauthenticated, like health: the contract describes the API rather
	// than exposing its data.
	status, body := get(t, a.base+"/openapi.yaml", "")

	// ASSERT
	require.Equal(t, http.StatusOK, status)

	spec := string(body)
	assert.True(t, strings.HasPrefix(spec, "openapi: 3.0.3"), "not an OpenAPI document: %.60s", spec)
	assert.Contains(t, spec, "/api/v1/scopes", "the spec must document the route it serves")
}

// httpClient returns the client the conditional-request assertions use. The
// shared get() helper cannot serve them: they need to set If-None-Match and read
// response headers, neither of which it exposes.
func httpClient() *http.Client {
	return &http.Client{
		// Never follow a redirect: this API issues none, and a silent follow
		// would turn a misrouted response into a passing assertion.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
