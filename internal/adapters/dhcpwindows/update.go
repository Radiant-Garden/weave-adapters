package dhcpwindows

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
)

// ScopeUpdate is a merge update to an existing scope: the fields a caller wants
// to change, and only those.
//
// The fields are pointers so "absent" (nil — leave unchanged) is distinct from
// "provided". This is the one place the package needs that distinction, which
// is why ScopeInput (create) stays value-typed. The immutable identity inputs —
// scopeId, subnetMask, wadaptId — are absent from the struct entirely, so the
// decoder's DisallowUnknownFields rejects a client that tries to assert one
// rather than silently dropping it.
//
// StartRange and EndRange are present because a pool may be resized, but only
// within the existing subnet: UpdateScope rejects a resize that would move the
// scope's identity. The JSON tags carry omitempty so this struct's observable
// wire shape matches the generated ScopeUpdate, which spec_test.go pins; on a
// decoded request body the omitempty is inert.
type ScopeUpdate struct {
	Name                 *string `json:"name,omitempty"`
	Description          *string `json:"description,omitempty"`
	LeaseDurationSeconds *int    `json:"leaseDurationSeconds,omitempty"`
	State                *string `json:"state,omitempty"`
	Type                 *string `json:"type,omitempty"`
	StartRange           *string `json:"startRange,omitempty"`
	EndRange             *string `json:"endRange,omitempty"`
}

// Validate reports every context-free problem with the update at once, as field
// errors ready for a 400.
//
// All of them, not the first: the same one-round-trip contract create keeps.
// Only rules that need no other state live here — whether a range change keeps
// the scope's identity needs the existing scope, so that check is UpdateScope's.
// This one proves each provided value is well-formed on its own.
func (in ScopeUpdate) Validate() []apierror.FieldError {
	var errs []apierror.FieldError

	errs = append(errs, in.validateText()...)
	errs = append(errs, in.validateOptions()...)
	errs = append(errs, in.validateRange()...)

	return errs
}

// validateText checks the free-text fields. A present-but-empty value is a
// rejection rather than a clear: an empty string cannot be told apart from
// absent once it reaches the script's if ($env:...) guard, so clearing a
// free-text field is not expressible in this version — omit it to leave it.
func (in ScopeUpdate) validateText() []apierror.FieldError {
	var errs []apierror.FieldError

	if in.Name != nil {
		switch {
		case *in.Name == "":
			errs = append(errs, fieldError("name", "must not be empty; omit to leave it unchanged"))
		case len(*in.Name) > maxNameLength:
			errs = append(errs, fieldError("name",
				fmt.Sprintf("must be at most %d characters", maxNameLength)))
		case hasControlChars(*in.Name):
			errs = append(errs, fieldError("name", controlCharMessage))
		}
	}

	if in.Description != nil {
		switch {
		case *in.Description == "":
			errs = append(errs, fieldError("description", "must not be empty; omit to leave it unchanged"))
		case len(*in.Description) > maxDescriptionLength:
			errs = append(errs, fieldError("description",
				fmt.Sprintf("must be at most %d characters", maxDescriptionLength)))
		case hasControlChars(*in.Description):
			errs = append(errs, fieldError("description", controlCharMessage))
		}
	}

	return errs
}

// validateOptions checks lease, state and type.
func (in ScopeUpdate) validateOptions() []apierror.FieldError {
	var errs []apierror.FieldError

	// "> 0" rather than create's ">= 0": zero has no "apply the server default"
	// meaning on an update, and "0" is truthy in the script's if ($env:...LEASE)
	// guard, so admitting it would splat a zero lease onto Set. See update_test.go.
	if in.LeaseDurationSeconds != nil {
		switch {
		case *in.LeaseDurationSeconds <= 0:
			errs = append(errs, fieldError("leaseDurationSeconds", "must be a positive number of seconds"))
		case *in.LeaseDurationSeconds > maxLeaseDurationSeconds:
			errs = append(errs, fieldError("leaseDurationSeconds",
				fmt.Sprintf("must be at most %d", maxLeaseDurationSeconds)))
		}
	}

	if in.State != nil && !slices.Contains(validStates, *in.State) {
		errs = append(errs, fieldError("state", "must be one of Active, Inactive"))
	}

	if in.Type != nil && !slices.Contains(validTypes, *in.Type) {
		errs = append(errs, fieldError("type", "must be one of Dhcp, Bootp, Both"))
	}

	return errs
}

// validateRange checks the range fields that can be judged without the existing
// scope: that each provided value parses as IPv4, and that a fully-specified
// range is not inverted. Whether a resize keeps the scope inside its subnet
// needs the existing mask, so that check is UpdateScope's.
func (in ScopeUpdate) validateRange() []apierror.FieldError {
	var errs []apierror.FieldError

	start, startOK := parseProvidedIPv4(in.StartRange)
	if in.StartRange != nil && !startOK {
		errs = append(errs, fieldError("startRange", "must be an IPv4 address"))
	}

	end, endOK := parseProvidedIPv4(in.EndRange)
	if in.EndRange != nil && !endOK {
		errs = append(errs, fieldError("endRange", "must be an IPv4 address"))
	}

	// end >= start only when both ends were provided and parsed. A one-sided
	// resize has nothing here to compare against — the other end lives on the
	// existing scope — so it is judged in validateEffectiveRange, which has it.
	if in.StartRange != nil && in.EndRange != nil && startOK && endOK && end.Less(start) {
		errs = append(errs, fieldError("endRange", "must not be before startRange"))
	}

	return errs
}

// env renders the update as the script's parameters, against the existing scope.
//
// Only the provided scalar fields are splatted, the same emptiness contract
// create uses. The range is the exception: Set-DhcpServerv4Scope's -StartRange
// and -EndRange are a mandatory-together parameter set (the WithRange set), so a
// one-sided change would bind it with a missing parameter and fail. When either
// is provided this emits both, filling the side the caller omitted from the
// existing scope, so the script always splats the pair or neither.
func (in ScopeUpdate) env(existing Scope) map[string]string {
	env := map[string]string{envScopeID: existing.ScopeID}

	if in.Name != nil {
		env[envScopeName] = *in.Name
	}

	if in.Description != nil {
		env[envScopeDescription] = *in.Description
	}

	if in.State != nil {
		env[envScopeState] = *in.State
	}

	if in.Type != nil {
		env[envScopeType] = *in.Type
	}

	if in.LeaseDurationSeconds != nil {
		env[envScopeLease] = strconv.Itoa(*in.LeaseDurationSeconds)
	}

	if in.StartRange != nil || in.EndRange != nil {
		env[envScopeStartRange] = effective(in.StartRange, existing.StartRange)
		env[envScopeEndRange] = effective(in.EndRange, existing.EndRange)
	}

	return env
}

// validateEffectiveRange reports every range field whose EFFECTIVE value — the
// one provided, or the existing one where the caller left it out — describes a
// range the DHCP server would refuse.
//
// Three rules, all of them needing the existing scope, which is why they are
// here rather than in validateRange:
//
//   - both ends must stay inside the existing subnet, or the derived scopeId
//     moves and the update reaches a different resource than the caller named;
//   - neither end may rest on the subnet's network or broadcast address, which
//     is inside the subnet and still not leasable — the same rule create
//     enforces, through the same function;
//   - the end must not come before the start.
//
// The third was missing, and its absence was the bug this function exists to
// prevent. `{"endRange": "10.0.30.5"}` against a scope running .10–.250
// produced no field errors, no offending fields, and an environment carrying
// start=10.0.30.10 end=10.0.30.5. Set-DhcpServerv4Scope throws,
// $ErrorActionPreference = 'Stop' exits non-zero, runError classifies it as
// ErrBackendUnavailable — and the client is told the DHCP server is
// unreachable, with a BACKEND-101 at ERROR pointing an operator at a server
// that is working perfectly. A client mistake must not be able to manufacture
// an outage signal; that is the same class 2db3f9d closed for unleasable
// ranges, and the reason create bounds leases and control characters at all.
//
// Field ERRORS rather than field names, because the three rules do not share a
// message and the caller renders them verbatim. Returns nothing when no range
// change was requested. The second return is a backend fault reserved for an
// existing mask that will not parse — which decode should have caught, but
// which cannot be a client error if it ever reaches here.
func (in ScopeUpdate) validateEffectiveRange(existing Scope) ([]apierror.FieldError, error) {
	if in.StartRange == nil && in.EndRange == nil {
		return nil, nil
	}

	mask, ok := parseIPv4(existing.SubnetMask)
	if !ok {
		return nil, fmt.Errorf("%w: existing scope %s has an unparseable subnet mask %q",
			ErrBackendMalformed, existing.WadaptID, existing.SubnetMask)
	}

	var offending []apierror.FieldError

	ends := make([]netip.Addr, 0, 2)
	parsed := true

	for _, f := range []struct{ name, value string }{
		{name: "startRange", value: effective(in.StartRange, existing.StartRange)},
		{name: "endRange", value: effective(in.EndRange, existing.EndRange)},
	} {
		addr, ok := parseIPv4(f.value)
		if !ok || networkOf(addr, mask) != existing.ScopeID {
			offending = append(offending, fieldError(f.name,
				"must be a leasable address inside the scope's existing subnet "+existing.ScopeID))
		}

		parsed = parsed && ok

		ends = append(ends, addr)
	}

	// An end on the subnet's network or broadcast address is inside the subnet
	// and still not a range Windows will set — the same rule create enforces,
	// reported through the same error so the handler renders one 400 for both.
	// Only judged on parsed ends: an unparseable one is already named above,
	// and the zero Addr has no bytes to mask.
	if parsed {
		offending = appendUnnamed(offending, checkLeasableEnds(ends[0], ends[1], mask)...)
		offending = appendUnnamed(offending, in.invertedRange(ends[0], ends[1])...)
	}

	return offending, nil
}

// invertedRange names the range field to fix when the effective ends are the
// wrong way round.
//
// Whichever end the CALLER provided is the one named, because that is the one
// they can change: a body of {"endRange": …} is told about endRange even though
// the comparison also involves a startRange they never sent. When both were
// provided this agrees with validateRange and names endRange, so one mistake
// reads the same however it arrives.
func (in ScopeUpdate) invertedRange(start, end netip.Addr) []apierror.FieldError {
	if !end.Less(start) {
		return nil
	}

	if in.EndRange != nil {
		return []apierror.FieldError{fieldError("endRange", "must not be before startRange")}
	}

	return []apierror.FieldError{fieldError("startRange", "must not be after endRange")}
}

// appendUnnamed adds each field error for a field not already named.
//
// Windows commonly makes one mistake fail two rules at once — an end outside
// the subnet is often also inverted — and reporting the same field twice reads
// as two problems.
func appendUnnamed(into []apierror.FieldError, more ...apierror.FieldError) []apierror.FieldError {
	for _, e := range more {
		if slices.ContainsFunc(into, func(have apierror.FieldError) bool { return have.Field == e.Field }) {
			continue
		}

		into = append(into, e)
	}

	return into
}

// parseProvidedIPv4 parses an optional address field: nil is (zero, true) so a
// caller can distinguish "not provided" from "provided and invalid".
func parseProvidedIPv4(value *string) (addr netip.Addr, ok bool) {
	if value == nil {
		return netip.Addr{}, true
	}

	return parseIPv4(*value)
}

// effective returns the provided value, or the existing one when the caller left
// the field out.
func effective(provided *string, existing string) string {
	if provided != nil {
		return *provided
	}

	return existing
}
