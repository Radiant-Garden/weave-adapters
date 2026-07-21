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

	// end >= start only when both ends were provided and parsed; a one-sided
	// resize is compared against the side left unchanged in UpdateScope.
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

// rangeFieldsOutsideSubnet names the range fields whose effective value would
// leave the existing subnet, and therefore change the scope's identity.
//
// Effective (provided-or-existing) endpoints, so a one-sided resize is judged
// against the side left unchanged. Returns no fields when no range change was
// requested. The second return is a backend fault reserved for an existing mask
// that will not parse — which decode should have caught, but which cannot be a
// client error if it ever reaches here.
func (in ScopeUpdate) rangeFieldsOutsideSubnet(existing Scope) ([]string, error) {
	if in.StartRange == nil && in.EndRange == nil {
		return nil, nil
	}

	mask, ok := parseIPv4(existing.SubnetMask)
	if !ok {
		return nil, fmt.Errorf("%w: existing scope %s has an unparseable subnet mask %q",
			ErrBackendMalformed, existing.WadaptID, existing.SubnetMask)
	}

	var offending []string

	for _, f := range []struct{ name, value string }{
		{name: "startRange", value: effective(in.StartRange, existing.StartRange)},
		{name: "endRange", value: effective(in.EndRange, existing.EndRange)},
	} {
		addr, ok := parseIPv4(f.value)
		if !ok || networkOf(addr, mask) != existing.ScopeID {
			offending = append(offending, f.name)
		}
	}

	return offending, nil
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
