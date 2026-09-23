package dhcpwindows

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
)

// ErrScopeNotFound reports that no scope on the server carries the requested
// wadaptID.
//
// Distinct from a backend failure: the backend answered, and the answer was
// that the identity names nothing here. The handler renders it as a 404 — which
// for a delete is the outcome weave counts as already-done, and for an update
// the outcome weave treats as "target missing, re-create next cycle".
var ErrScopeNotFound = errors.New("no scope with that wadaptID")

// ErrRangeUnusable reports that the range a requested change would produce is
// not one the DHCP server will set.
//
// Three ways in, all of them the client's input to fix, which is why the
// handler renders this as a field validation failure rather than a backend
// error: an end outside the existing subnet, which would move the derived
// identity; an end resting on the subnet's network or broadcast address, which
// stays inside the subnet and is still not leasable; and an effective range
// whose end comes before its start.
//
// It was ErrRangeOutsideSubnet until the inversion joined it. The name had
// stopped describing two of the three.
var ErrRangeUnusable = errors.New("range is not one the DHCP server will set")

// effectiveRangeError carries the field errors describing what is wrong with
// the effective range, so the handler renders the fields the caller actually
// got wrong with the reason each one is wrong. It wraps ErrRangeUnusable, so
// errors.Is still recognises it.
//
// Field ERRORS rather than names: the three rules behind it do not share a
// message, and a handler that supplied one hardcoded sentence would have to
// pick the wrong one for two of them.
type effectiveRangeError struct {
	scopeID string
	fields  []apierror.FieldError
}

func (e *effectiveRangeError) Error() string {
	names := make([]string, 0, len(e.fields))
	for _, f := range e.fields {
		names = append(names, f.Field)
	}

	return fmt.Sprintf("%s: %s against subnet %s",
		ErrRangeUnusable, strings.Join(names, ", "), e.scopeID)
}

func (e *effectiveRangeError) Unwrap() error { return ErrRangeUnusable }

// resolveScope lists the server's scopes and returns the one matching wadaptID.
//
// A wadaptID is an HMAC of the server name and the subnet, so it cannot be
// reversed to the scopeId a targeted query would need — the mutation methods
// discover their target the same way the item GET does, with one ListScopes
// spawn and a scan. That spawn also runs reportDrift, which is correct: a
// listing is a listing however it was obtained.
//
// A backend failure during the list is returned unchanged, already carrying its
// BACKEND-101 from ListScopes. Only the not-found case is minted here.
func (c *Client) resolveScope(ctx context.Context, wadaptID string) (Scope, error) {
	scopes, err := c.ListScopes(ctx)
	if err != nil {
		return Scope{}, err
	}

	for i := range scopes {
		if scopes[i].WadaptID == wadaptID {
			return scopes[i], nil
		}
	}

	return Scope{}, fmt.Errorf("%w: %s", ErrScopeNotFound, wadaptID)
}

// DeleteScope removes the scope identified by wadaptID.
//
// Two spawns: one ListScopes to resolve the identity to a scopeId, then
// Remove-DhcpServerv4Scope on that subnet. A wadaptID the server does not hold
// is ErrScopeNotFound, which the handler renders as a 404 — the answer weave
// counts as already-deleted.
//
// The drift ledger entry for the removed scope is deliberately NOT evicted:
// retaining it is what lets a later create on the same reused subnet trip
// DHCP-002, which is the reason the ledger keeps deleted entries at all.
//
// The resolve is not atomic with the remove. A concurrent delete in the window
// makes Remove-DhcpServerv4Scope throw under $ErrorActionPreference = 'Stop',
// which surfaces as a backend error rather than a 404; weave retries the 5xx and
// the next delete resolves as a 404. The scope is never removed twice — Windows
// guarantees that — which is the property that matters.
func (c *Client) DeleteScope(ctx context.Context, wadaptID string) error {
	scope, err := c.resolveScope(ctx, wadaptID)
	if err != nil {
		return err
	}

	ctx, cancel := c.bounded(ctx)
	defer cancel()

	_, stderr, err := c.runner.run(ctx, deleteScopeScript, map[string]string{envScopeID: scope.ScopeID})
	if err != nil {
		return c.backendError(ctx, opDeleteScope, runError(err, stderr))
	}

	return nil
}

// UpdateScope applies a merge update to the scope identified by wadaptID and
// returns it as the API serves it.
//
// Two spawns: ListScopes to resolve the target (and read the existing subnet the
// range check needs), then Set-DhcpServerv4Scope -PassThru. A wadaptID the
// server does not hold is ErrScopeNotFound → 404.
//
// The identity is protected on both sides of the write. Before: a range change
// is rejected unless the effective endpoints still fall in the existing subnet,
// so the derived scopeId cannot move. After: the -PassThru scope is re-derived
// and its wadaptID asserted equal to the requested one — the same guard
// CreateScope makes against a backend that did something other than asked.
//
// The same pre-flight is what keeps a client mistake from reading as an outage.
// An effective range the server would throw on reaches runError as a non-zero
// exit and is classified ErrBackendUnavailable, so a 400 the caller can fix
// would arrive as a 502 and a BACKEND-101 at ERROR naming a healthy server.
func (c *Client) UpdateScope(ctx context.Context, wadaptID string, in ScopeUpdate) (Scope, error) {
	existing, err := c.resolveScope(ctx, wadaptID)
	if err != nil {
		return Scope{}, err
	}

	offending, err := in.validateEffectiveRange(existing)
	if err != nil {
		// An unparseable existing mask: a backend fault, not the client's.
		return Scope{}, c.backendError(ctx, opUpdateScope, err)
	}

	if len(offending) > 0 {
		return Scope{}, &effectiveRangeError{scopeID: existing.ScopeID, fields: offending}
	}

	ctx, cancel := c.bounded(ctx)
	defer cancel()

	stdout, stderr, err := c.runner.run(ctx, updateScopeScript, in.env(existing))
	if err != nil {
		return Scope{}, c.backendError(ctx, opUpdateScope, runError(err, stderr))
	}

	scopes, err := decodeScopes(stdout, stderr)
	if err != nil {
		return Scope{}, c.backendError(ctx, opUpdateScope, err)
	}

	// -PassThru returns exactly the scope it changed. Anything else means the
	// script did something other than what it was written to do, and serving a
	// scope from an unexpected payload would report an update that may not have
	// happened as described.
	if len(scopes) != 1 {
		return Scope{}, c.backendError(ctx, opUpdateScope,
			fmt.Errorf("%w: update returned %d scopes, expected 1", ErrBackendMalformed, len(scopes)))
	}

	if err := c.identify(scopes); err != nil {
		return Scope{}, c.backendError(ctx, opUpdateScope, err)
	}

	updated := scopes[0]

	// The scope Windows returned must still be the one asked for. If the identity
	// moved, an update reached a different resource than the caller named, and
	// serving it would report a change to the wrong scope.
	if updated.WadaptID != wadaptID {
		return Scope{}, c.backendError(ctx, opUpdateScope,
			fmt.Errorf("%w: update of %s produced identity %s", ErrBackendMalformed, wadaptID, updated.WadaptID))
	}

	// Re-baseline drift silently. resolveScope above recorded the pre-update
	// fingerprint through ListScopes, so without this the next listing would see
	// the intentional change as drift and emit a spurious DHCP-002. observe
	// updates the ledger to the new fingerprint; its report is discarded, so a
	// *later external* change is still caught.
	c.drift.observe([]Scope{updated})

	return updated, nil
}
