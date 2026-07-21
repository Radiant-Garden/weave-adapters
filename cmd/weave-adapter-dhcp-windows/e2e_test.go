//go:build e2e

/*
Testing: the read and create paths against a real DHCP backend

Pending:

	A multi-scope host. Several assertions below (ordering, the pagination walk,
	"no duplicates") are only load-bearing with two or more scopes; on a
	single-scope server they pass vacuously. TestE2E_ShouldServeScopes says so in
	its log output rather than pretending otherwise.

	Drift detection (DHCP-002) observing a real delete-and-recreate. The ledger
	is per-process and the event goes to the adapter's log, so asserting it needs
	the harness to capture and parse that process's stderr — machinery no test
	here has yet. Until then the drift path is proven against constructed scopes
	only, and the thing it is meant to catch has never been staged for real.

	The create fixture in internal/adapters/dhcpwindows/create_test.go is still
	hand-written rather than captured. These tests now exercise the real
	projection end to end, which is the coverage that mattered, but the unit
	fixture remains someone's idea of what -PassThru emits. Capture it at
	sign-off.

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

	the write path, which until these existed had never met a real shell
	  - TestE2E_ShouldCreateAScopeAndResolveItsLocation: 201, the derived identity
	    agrees with Windows, the optional values survive exec.Cmd.Env, and the
	    Location a client is handed actually resolves.
	  - TestE2E_ShouldRejectADuplicateSubnetWithAConflict: 409 rather than a
	    backend error, which means the conflict marker survived a real round trip.
	  - TestE2E_ShouldRejectABadCreateBeforeReachingTheBackend: four rejections,
	    and nothing reached the DHCP server.
	  - TestE2E_ShouldUpdateAScopeWithoutMovingItsIdentity: PATCH changes the
	    mutable attributes, the wadaptID holds, and a merge leaves omitted fields.
	  - TestE2E_ShouldResizeAScopeWithinItsSubnet: a range change binds both
	    -StartRange and -EndRange on a real Set, and the identity holds.
	  - TestE2E_ShouldRejectAResizeThatLeavesTheSubnet: an out-of-subnet range is a
	    400 the adapter enforces, and the scope is not half-changed.
	  - TestE2E_ShouldDeleteAScope: DELETE is 204, the scope is gone from both the
	    item route and the collection, and a second delete is the 404 weave counts
	    as already-deleted.
	  - TestE2E_ShouldAnswer404ForMutationsOnAnUnknownScope: PATCH and DELETE on an
	    identity the host does not hold are both 404.

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

	Exercising 413 and 415 here. Both are core request-body rejections that never
	reach the backend, so a real DHCP server proves nothing a fake does not.
	internal/core/requestbody owns them.

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

	THE WRITE TESTS MUTATE THE HOST. They create on 198.51.100.0/24 (RFC 5737
	TEST-NET-2) and clean up through PowerShell directly, deliberately NOT through
	the DELETE endpoint even though M3b added one: cleanup must not depend on the
	thing under test, or a broken DELETE would leave scopes behind while its own
	test failed. reserveTestSubnet pre-cleans as well as post-cleans, so a run
	that died mid-test does not poison every run after it. A cleanup that fails
	prints the exact command to run by hand rather than failing quietly, since a
	leftover scope surfaces later as a 409 on create, which reads like a conflict
	bug and is not one.

	The identity inputs come from harness_test.go's startAdapter and are FIXED
	constants. TestE2E_ShouldDeriveStableIdentitiesAcrossARestart compares
	wadaptIDs across two processes, so randomising them per run would make that
	assertion pass vacuously.
*/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

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

// testSubnet is the subnet the write tests create on.
//
// RFC 5737 TEST-NET-2, chosen because it is reserved for documentation and will
// not be a real scope on any host this gate runs against. Deliberately NOT
// 203.0.113.0 (TEST-NET-3): TestE2E_ShouldFilterWithoutDisturbingTheCollection
// uses that one as its "no such scope" probe, and creating a scope there would
// make that test fail for a reason having nothing to do with filtering.
const (
	testSubnet     = "198.51.100.0"
	testSubnetMask = "255.255.255.0"
	testStartRange = "198.51.100.10"
	testEndRange   = "198.51.100.200"
)

// powershell returns the shell the cleanup path drives.
//
// It honours the same WEAVE_ADAPTER_DHCP_POWERSHELL_PATH knob the adapter does,
// so a stub that replays fixtures keeps working off-host. That is the existing
// config knob, not a second mechanism invented for tests.
func powershell() string {
	if path := os.Getenv("WEAVE_ADAPTER_DHCP_POWERSHELL_PATH"); path != "" {
		return path
	}

	return "powershell.exe"
}

// removeScope deletes a scope directly, bypassing the adapter.
//
// Test code reaching around the thing under test is normally a smell, and here
// it is deliberate: cleanup must not depend on the DELETE endpoint M3b added,
// because a broken DELETE would then leave scopes behind on a shared host while
// its own test failed. A gate that created scopes without removing them would
// leave the host dirtier after every CI run, and the *next* run would meet its
// own leftover as a 409 and fail for the wrong reason.
//
// -Force because a scope holding leases will not delete without it. Failures
// are reported rather than swallowed: a scope this gate could not clean up is a
// real operational fact somebody has to act on, and silently continuing would
// hide it until the next run failed mysteriously.
func removeScope(t *testing.T, subnet string, mustSucceed bool) {
	t.Helper()

	script := fmt.Sprintf(
		`$ErrorActionPreference = 'Stop'; Remove-DhcpServerv4Scope -ScopeId %s -Force`, subnet)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
	defer cancel()

	// The shell path comes from an operator-set environment variable and the
	// script interpolates a package constant, never a value from the network.
	// This is a test helper on a gate that already runs arbitrary built code.
	//nolint:gosec // G204: constant script, shell path from the documented config knob.
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", script)

	if out, err := cmd.CombinedOutput(); err != nil && mustSucceed {
		t.Errorf(`could not remove scope %s from the DHCP server.

MANUAL CLEANUP REQUIRED on this host:
  Remove-DhcpServerv4Scope -ScopeId %s -Force

Leaving it behind will make the next run of this gate fail with a 409 on
create, which looks like a conflict bug and is not one.

error: %v
output: %s`, subnet, subnet, err, out)
	}
}

// reserveTestSubnet clears any leftover from a previous run and schedules
// removal of whatever this test creates.
//
// Pre-cleaning is what makes the gate self-healing: a run that died between
// create and cleanup would otherwise poison every run after it. That first
// removal is best-effort — on a clean host there is nothing to remove and the
// cmdlet failing is the expected case.
func reserveTestSubnet(t *testing.T) {
	t.Helper()

	removeScope(t, testSubnet, false)
	t.Cleanup(func() { removeScope(t, testSubnet, true) })
}

// createBody is a valid create payload for the reserved test subnet.
func createBody(name string) string {
	return fmt.Sprintf(
		`{"name":%q,"startRange":%q,"endRange":%q,"subnetMask":%q,`+
			`"description":"created by the e2e gate","leaseDurationSeconds":691200}`,
		name, testStartRange, testEndRange, testSubnetMask)
}

// createScope posts a body and returns status, decoded scope and headers.
func (a *adapter) createScope(t *testing.T, body string) (int, scope, http.Header) {
	t.Helper()

	status, payload, header := post(t, a.base+"/api/v1/scopes", a.token, body)

	var created scope
	if status == http.StatusCreated {
		require.NoError(t, json.Unmarshal(payload, &created), "create payload: %s", payload)
	}

	return status, created, header
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and mutates the one real
func TestE2E_ShouldCreateAScopeAndResolveItsLocation(t *testing.T) {
	// ARRANGE — this is the test the whole write path rests on. Until it ran,
	// every assertion about create was made against a hand-written fixture
	// nobody had captured from a host: the -PassThru projection, the conflict
	// marker, and the eight values that reach the script through the child
	// environment had never met a real shell.
	a := startE2E(t)
	a.requireHealthyBackend(t)
	reserveTestSubnet(t)

	// ACT
	status, created, header := a.createScope(t, createBody("e2e-create"))

	// ASSERT — 201 with the scope as it now exists.
	require.Equal(t, http.StatusCreated, status)

	assert.Equal(t, testSubnet, created.ScopeID, "the adapter and Windows must agree on the derived subnet")
	assert.Regexp(t, wadaptIDShape, created.WadaptID)
	assert.Equal(t, "e2e-create", created.Name)
	assert.Equal(t, "ipv4", created.AddressFamily)

	// The optional values travel through exec.Cmd.Env and are splatted onto the
	// cmdlet. The read path only ever passed one value that way; this is the
	// first exercise of that mechanism at width, and a silently dropped optional
	// would show up here as a default rather than as an error.
	assert.Equal(t, "created by the e2e gate", created.Description)
	assert.Equal(t, 691200, created.LeaseDurationSeconds)

	// Location must resolve. A 201 pointing at a 404 tells a client the create
	// did not happen, which is worse than returning no header at all.
	location := header.Get("Location")
	require.NotEmpty(t, location, "201 carried no Location")
	assert.Equal(t, "/api/v1/scopes/"+created.WadaptID, location)

	status, body := get(t, a.base+location, a.token)
	require.Equal(t, http.StatusOK, status, "Location %q did not resolve: %s", location, body)

	var fetched scope
	require.NoError(t, json.Unmarshal(body, &fetched))
	assert.Equal(t, created, fetched, "the item route must serve what create returned")

	// And it is in the collection, which is the read path agreeing with the
	// write path on a host neither of them faked.
	page := a.listScopes(t, "scopeId="+testSubnet)
	require.Len(t, page.Items, 1)
	assert.Equal(t, created.WadaptID, page.Items[0].WadaptID)
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and mutates the one real
func TestE2E_ShouldRejectADuplicateSubnetWithAConflict(t *testing.T) {
	// ARRANGE
	a := startE2E(t)
	a.requireHealthyBackend(t)
	reserveTestSubnet(t)

	status, _, _ := a.createScope(t, createBody("e2e-first"))
	require.Equal(t, http.StatusCreated, status)

	// ACT — the same subnet again. Windows permits exactly one scope per subnet.
	status, _, _ = a.createScope(t, createBody("e2e-second"))

	// ASSERT — 409, which means the pre-create check saw the existing scope and
	// the conflict marker survived a round trip through a real shell. That
	// marker match is exact rather than a substring search, so a projection
	// change on the host would surface here as a 502 instead.
	require.Equal(t, http.StatusConflict, status,
		"a duplicate subnet must be a conflict, not a backend error")

	// The subnet is still the one scope it was, so the failed create changed
	// nothing on the server.
	page := a.listScopes(t, "scopeId="+testSubnet)
	assert.Len(t, page.Items, 1)
	assert.Equal(t, "e2e-first", page.Items[0].Name)
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldRejectABadCreateBeforeReachingTheBackend(t *testing.T) {
	// ARRANGE — no reserveTestSubnet: none of these should reach the server, and
	// scheduling a cleanup would hide a failure to honour that.
	a := startE2E(t)
	a.requireHealthyBackend(t)

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"invalid input", `{"name":"","startRange":"nope","endRange":"nope","subnetMask":"nope"}`, http.StatusBadRequest},
		{"malformed json", `{"name":`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
		// A client cannot assert the identity the server derives.
		{"derived field", createBody("x")[:len(createBody("x"))-1] + `,"wadaptId":"chosen"}`, http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// ACT
			status, payload, _ := post(t, a.base+"/api/v1/scopes", a.token, tc.body)

			// ASSERT
			assert.Equal(t, tc.wantStatus, status, "body: %s", payload)
			assert.Contains(t, string(payload), "weave-adapters:", "rejection must be problem+json")
		})
	}

	// Nothing was created on the reserved subnet, which is the assertion that
	// makes the rest of this test worth running against a real host rather than
	// a fake: validation happens before the backend is touched.
	page := a.listScopes(t, "scopeId="+testSubnet)
	assert.Empty(t, page.Items, "a rejected create reached the DHCP server")
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and mutates the one real
func TestE2E_ShouldUpdateAScopeWithoutMovingItsIdentity(t *testing.T) {
	// ARRANGE
	a := startE2E(t)
	a.requireHealthyBackend(t)
	reserveTestSubnet(t)

	status, created, _ := a.createScope(t, createBody("e2e-update"))
	require.Equal(t, http.StatusCreated, status)

	item := a.base + "/api/v1/scopes/" + created.WadaptID

	// ACT — change the mutable attributes, leaving the range alone.
	status, body := patch(t, item, a.token, `{"name":"e2e-renamed","leaseDurationSeconds":3600,"state":"Inactive"}`)

	// ASSERT — 200 with the changed attributes and, crucially, the SAME identity:
	// a merge must never move the wadaptID, or weave would bind to the wrong object.
	require.Equal(t, http.StatusOK, status, "body: %s", body)

	var updated scope
	require.NoError(t, json.Unmarshal(body, &updated))
	assert.Equal(t, created.WadaptID, updated.WadaptID, "an update must not move the identity")
	assert.Equal(t, testSubnet, updated.ScopeID)
	assert.Equal(t, "e2e-renamed", updated.Name)
	assert.Equal(t, 3600, updated.LeaseDurationSeconds)
	assert.Equal(t, "Inactive", updated.State)

	// The omitted fields are unchanged, which is what "merge" means: the range and
	// the description the create set are still there because the update did not
	// mention them.
	assert.Equal(t, testStartRange, updated.StartRange)
	assert.Equal(t, "created by the e2e gate", updated.Description)

	// A fresh GET reflects it, so the change reached the server rather than only
	// the response body.
	getStatus, getBody := get(t, item, a.token)
	require.Equal(t, http.StatusOK, getStatus)

	var fetched scope
	require.NoError(t, json.Unmarshal(getBody, &fetched))
	assert.Equal(t, updated, fetched)
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and mutates the one real
func TestE2E_ShouldResizeAScopeWithinItsSubnet(t *testing.T) {
	// ARRANGE — the one host exercise of the range write and of the
	// both-or-neither splat: Set-DhcpServerv4Scope's -StartRange/-EndRange are
	// mandatory together, so a resize binding only one would fail on a real shell.
	a := startE2E(t)
	a.requireHealthyBackend(t)
	reserveTestSubnet(t)

	status, created, _ := a.createScope(t, createBody("e2e-resize"))
	require.Equal(t, http.StatusCreated, status)

	item := a.base + "/api/v1/scopes/" + created.WadaptID

	// ACT — a tighter pool, still inside 198.51.100.0/24.
	const newStart, newEnd = "198.51.100.20", "198.51.100.190"

	status, body := patch(t, item, a.token, fmt.Sprintf(`{"startRange":%q,"endRange":%q}`, newStart, newEnd))

	// ASSERT — 200, the range changed, and the identity held because the subnet
	// did not.
	require.Equal(t, http.StatusOK, status, "body: %s", body)

	var updated scope
	require.NoError(t, json.Unmarshal(body, &updated))
	assert.Equal(t, created.WadaptID, updated.WadaptID, "a resize inside the subnet must not move the identity")
	assert.Equal(t, newStart, updated.StartRange)
	assert.Equal(t, newEnd, updated.EndRange)
	assert.Equal(t, testSubnet, updated.ScopeID)
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and mutates the one real
func TestE2E_ShouldRejectAResizeThatLeavesTheSubnet(t *testing.T) {
	// ARRANGE
	a := startE2E(t)
	a.requireHealthyBackend(t)
	reserveTestSubnet(t)

	status, created, _ := a.createScope(t, createBody("e2e-badresize"))
	require.Equal(t, http.StatusCreated, status)

	item := a.base + "/api/v1/scopes/" + created.WadaptID

	// ACT — an end one subnet over, which would change the derived identity.
	status, body := patch(t, item, a.token, `{"endRange":"198.51.101.10"}`)

	// ASSERT — a 400 naming the range field, and the adapter enforces it rather
	// than leaving it to Windows: the identity invariant is the adapter's to keep.
	require.Equal(t, http.StatusBadRequest, status, "body: %s", body)
	assert.Contains(t, string(body), "weave-adapters:validation-failed")
	assert.Contains(t, string(body), "endRange")

	// The scope is unchanged — a rejected resize must not have half-applied.
	getStatus, getBody := get(t, item, a.token)
	require.Equal(t, http.StatusOK, getStatus)

	var fetched scope
	require.NoError(t, json.Unmarshal(getBody, &fetched))
	assert.Equal(t, testEndRange, fetched.EndRange, "a rejected resize must leave the scope as it was")
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and mutates the one real
func TestE2E_ShouldDeleteAScope(t *testing.T) {
	// ARRANGE — this test removes the scope itself, so cleanup is best-effort: a
	// mustSucceed cleanup would fail on the scope the test already deleted. The
	// pre-clean still runs, so a run that died mid-test does not poison the next.
	a := startE2E(t)
	a.requireHealthyBackend(t)
	removeScope(t, testSubnet, false)
	t.Cleanup(func() { removeScope(t, testSubnet, false) })

	status, created, _ := a.createScope(t, createBody("e2e-delete"))
	require.Equal(t, http.StatusCreated, status)

	item := a.base + "/api/v1/scopes/" + created.WadaptID

	// ACT — remove it through the adapter.
	delStatus, delBody := del(t, item, a.token)

	// ASSERT — 204 with no body.
	require.Equal(t, http.StatusNoContent, delStatus, "body: %s", delBody)
	assert.Empty(t, delBody, "a 204 carries no body")

	// It is gone: the item route 404s and the collection no longer lists it.
	getStatus, _ := get(t, item, a.token)
	assert.Equal(t, http.StatusNotFound, getStatus, "the deleted scope must not resolve")

	page := a.listScopes(t, "scopeId="+testSubnet)
	assert.Empty(t, page.Items, "the deleted scope must not appear in the collection")

	// A second delete is a 404, which weave counts as already-deleted — the
	// idempotency a reconciler relies on when it retries a delete it already made.
	againStatus, _ := del(t, item, a.token)
	assert.Equal(t, http.StatusNotFound, againStatus, "deleting an already-gone scope is a 404, not a 204")
}

// DHCP server; running them concurrently would put N powershell.exe spawns on a shared
// host and make a slow query look like a flaky test.
//
//nolint:paralleltest // each case starts its own adapter process and queries the one real
func TestE2E_ShouldAnswer404ForMutationsOnAnUnknownScope(t *testing.T) {
	// ARRANGE — a well-formed identity the host does not hold, so nothing this
	// test does touches a real scope and it needs no cleanup.
	a := startE2E(t)
	a.requireHealthyBackend(t)

	item := a.base + "/api/v1/scopes/0000000000000"

	// ACT / ASSERT — PATCH and DELETE on an absent identity are both 404, the
	// answer weave treats as "target gone, re-create next cycle".
	patchStatus, patchBody := patch(t, item, a.token, `{"name":"x"}`)
	assert.Equal(t, http.StatusNotFound, patchStatus, "body: %s", patchBody)

	delStatus, _ := del(t, item, a.token)
	assert.Equal(t, http.StatusNotFound, delStatus)
}
