package dhcpwindows

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
)

// ErrScopeExists reports that the subnet already holds a scope.
//
// Distinct from the other backend failures because it is the one a client can
// act on: Windows permits exactly one scope per subnet, so the answer is not
// "retry" but "you meant to update the scope that is already there".
var ErrScopeExists = errors.New("a scope already exists on that subnet")

// ScopeInput is what a caller supplies to create a scope.
//
// Narrower than Scope, and deliberately so: it carries only what
// Add-DhcpServerv4Scope accepts, and nothing the server derives. scopeId is
// computed from StartRange and SubnetMask, and wadaptID from that — a caller
// that could set either would be asserting an identity the server is about to
// compute, and could disagree with it.
//
// The optional fields are zero-valued when omitted, and a zero value means
// "let Windows apply its own default" rather than a value this adapter chose.
// The JSON tags are the request body's contract, and they are hand-written for
// the same reason Scope's are: the generated ScopeCreate is compared against
// this struct by a conformance test rather than imported, which keeps the
// adapter free of a dependency on its own generated code.
type ScopeInput struct {
	Name       string `json:"name"`
	StartRange string `json:"startRange"`
	EndRange   string `json:"endRange"`
	SubnetMask string `json:"subnetMask"`

	Description          string `json:"description,omitempty"`
	LeaseDurationSeconds int    `json:"leaseDurationSeconds,omitempty"`
	State                string `json:"state,omitempty"`
	Type                 string `json:"type,omitempty"`
}

// Scope states and types Windows accepts. Validated here rather than left to
// the cmdlet: a rejected value should be a 400 naming the field, not a backend
// error naming a PowerShell parameter the client never saw.
var (
	validStates = []string{"Active", "Inactive"}
	validTypes  = []string{"Dhcp", "Bootp", "Both"}
)

// maxNameLength and maxDescriptionLength mirror what the DHCP console accepts.
// Bounded here so an over-long value is a field error rather than a backend
// failure halfway through a create.
const (
	maxNameLength        = 255
	maxDescriptionLength = 255
)

// Validate reports every problem with the input at once, as field errors ready
// for a 400.
//
// All of them, not the first: a client fixing one mistake per round trip is the
// thing the errors[] extension exists to prevent.
//
// Split into three because the rules group that way and because one function
// checking eleven things is one nobody re-reads before adding a twelfth. Same
// shape as config.Validate.
func (in ScopeInput) Validate() []apierror.FieldError {
	var errs []apierror.FieldError

	errs = append(errs, in.validateText()...)
	errs = append(errs, in.validateAddressing()...)
	errs = append(errs, in.validateOptions()...)

	return errs
}

// validateText checks the free-text fields, which are bounded so an over-long
// value is a field error rather than a backend failure halfway through a create.
func (in ScopeInput) validateText() []apierror.FieldError {
	var errs []apierror.FieldError

	switch {
	case in.Name == "":
		errs = append(errs, fieldError("name", "is required"))
	case len(in.Name) > maxNameLength:
		errs = append(errs, fieldError("name",
			fmt.Sprintf("must be at most %d characters", maxNameLength)))
	}

	if len(in.Description) > maxDescriptionLength {
		errs = append(errs, fieldError("description",
			fmt.Sprintf("must be at most %d characters", maxDescriptionLength)))
	}

	return errs
}

// validateAddressing checks the three fields that decide which subnet the scope
// lands on — and therefore what its identity will be.
func (in ScopeInput) validateAddressing() []apierror.FieldError {
	var errs []apierror.FieldError

	start, startOK := parseIPv4(in.StartRange)
	if !startOK {
		errs = append(errs, fieldError("startRange", "must be an IPv4 address"))
	}

	end, endOK := parseIPv4(in.EndRange)
	if !endOK {
		errs = append(errs, fieldError("endRange", "must be an IPv4 address"))
	}

	mask, maskOK := parseIPv4(in.SubnetMask)

	switch {
	case !maskOK:
		errs = append(errs, fieldError("subnetMask", "must be an IPv4 address"))
	case !isContiguousMask(mask):
		// 255.255.0.255 parses as an address and is not a subnet mask. Windows
		// would reject it, but with a message about a cmdlet parameter rather
		// than about the field the client sent.
		errs = append(errs, fieldError("subnetMask",
			"must be a contiguous subnet mask, e.g. 255.255.255.0"))
		maskOK = false
	}

	if !startOK || !endOK {
		return errs
	}

	if end.Less(start) {
		errs = append(errs, fieldError("endRange", "must not be before startRange"))
	}

	// Both ends must be in the subnet the mask defines, or the scope Windows
	// creates would not be the scope the client described — and its identity
	// derives from that subnet.
	if maskOK && networkOf(start, mask) != networkOf(end, mask) {
		errs = append(errs, fieldError("endRange",
			"must be in the same subnet as startRange under subnetMask"))
	}

	return errs
}

// validateOptions checks the fields Windows would otherwise reject with a
// message naming a cmdlet parameter the client never sent.
func (in ScopeInput) validateOptions() []apierror.FieldError {
	var errs []apierror.FieldError

	if in.LeaseDurationSeconds < 0 {
		errs = append(errs, fieldError("leaseDurationSeconds", "must be positive"))
	}

	if in.State != "" && !slices.Contains(validStates, in.State) {
		errs = append(errs, fieldError("state", "must be one of Active, Inactive"))
	}

	if in.Type != "" && !slices.Contains(validTypes, in.Type) {
		errs = append(errs, fieldError("type", "must be one of Dhcp, Bootp, Both"))
	}

	return errs
}

// fieldError builds one client-facing validation failure. The message describes
// the expectation and never internal state, because it reaches the client
// verbatim.
func fieldError(field, message string) apierror.FieldError {
	return apierror.FieldError{Field: field, Message: message}
}

// ScopeID returns the subnet address the input describes, which is the identity
// Windows will give the scope.
//
// Computed here rather than in PowerShell because Go needs it anyway to derive
// the wadaptID, and computing the same value twice in two languages is how the
// two come to disagree. Only valid after Validate returns no errors.
func (in ScopeInput) ScopeID() (string, error) {
	start, startOK := parseIPv4(in.StartRange)
	mask, maskOK := parseIPv4(in.SubnetMask)

	if !startOK || !maskOK {
		return "", fmt.Errorf("cannot derive scopeId from %q/%q", in.StartRange, in.SubnetMask)
	}

	return networkOf(start, mask), nil
}

// CreateScope adds a scope and returns it as the API serves it, carrying its
// derived identity.
//
// One PowerShell spawn. Every value travels through the child environment; the
// script is a constant and nothing is interpolated into it.
func (c *Client) CreateScope(ctx context.Context, in ScopeInput) (Scope, error) {
	scopeID, err := in.ScopeID()
	if err != nil {
		return Scope{}, fmt.Errorf("%w: %w", ErrBackendMalformed, err)
	}

	ctx, cancel := c.bounded(ctx)
	defer cancel()

	stdout, stderr, err := c.runner.run(ctx, createScopeScript, in.env(scopeID))
	if err != nil {
		return Scope{}, c.backendError(ctx, opCreateScope, runError(err, stderr))
	}

	// The subnet was already taken. Checked before the create rather than
	// classified from a localized error message; see createScopeScript.
	//
	// Returned without backendError, deliberately: that emits BACKEND-101 at
	// ERROR with "dhcp backend call failed", and nothing failed here. The shell
	// ran, the check ran, and the answer was "taken" — a normal outcome of a
	// client asking for a scope that already exists. Logging it as a backend
	// error would alert an operator to ordinary client behaviour and point them
	// at a DHCP server that is working. BACKEND-105 is the only line this
	// deserves, and the handler emits it with the response.
	if isConflict(stdout) {
		return Scope{}, fmt.Errorf("%w: %s", ErrScopeExists, scopeID)
	}

	scopes, err := decodeScopes(stdout, stderr)
	if err != nil {
		return Scope{}, c.backendError(ctx, opCreateScope, err)
	}

	// -PassThru returns exactly the scope it created. Anything else means the
	// script did something other than what it was written to do, and serving a
	// scope from an unexpected payload would report a create that may not have
	// happened as described.
	if len(scopes) != 1 {
		return Scope{}, c.backendError(ctx, opCreateScope,
			fmt.Errorf("%w: create returned %d scopes, expected 1", ErrBackendMalformed, len(scopes)))
	}

	if err := c.identify(scopes); err != nil {
		return Scope{}, c.backendError(ctx, opCreateScope, err)
	}

	// A create is the operation most likely to reuse a subnet, so this is the
	// path the drift ledger exists for rather than an afterthought on it: a
	// scope created on a subnet that held one before derives the same wadaptID
	// as its predecessor, and this is where that becomes observable.
	c.reportDrift(ctx, scopes)

	// The scope Windows created must be the subnet the client described. If it
	// is not, the identity we are about to hand back in a Location header would
	// name a different resource.
	if scopes[0].ScopeID != scopeID {
		return Scope{}, c.backendError(ctx, opCreateScope,
			fmt.Errorf("%w: asked for scope %s, backend created %s",
				ErrBackendMalformed, scopeID, scopes[0].ScopeID))
	}

	return scopes[0], nil
}

// env renders the input as the script's parameters.
//
// Optional values are omitted when empty rather than passed as an empty string,
// because the script tests each variable for emptiness to decide whether to
// splat it — and an empty Description passed explicitly would set a description
// of "" rather than leaving it unset.
func (in ScopeInput) env(scopeID string) map[string]string {
	env := map[string]string{
		envScopeID:         scopeID,
		envScopeName:       in.Name,
		envScopeStartRange: in.StartRange,
		envScopeEndRange:   in.EndRange,
		envScopeSubnetMask: in.SubnetMask,
	}

	if in.Description != "" {
		env[envScopeDescription] = in.Description
	}

	if in.State != "" {
		env[envScopeState] = in.State
	}

	if in.Type != "" {
		env[envScopeType] = in.Type
	}

	if in.LeaseDurationSeconds > 0 {
		env[envScopeLease] = strconv.Itoa(in.LeaseDurationSeconds)
	}

	return env
}

// isConflict reports whether the script signalled the subnet was taken.
//
// An exact match on the trimmed output, not a substring search: a scope
// description containing the marker text would otherwise turn a successful
// create into a reported conflict.
func isConflict(stdout []byte) bool {
	return strings.TrimSpace(string(stdout)) == conflictMarker
}

// parseIPv4 parses an IPv4 address, rejecting anything else — including an
// IPv6 address and an IPv4-mapped IPv6 one, neither of which this version
// serves.
func parseIPv4(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil || !addr.Is4() {
		return netip.Addr{}, false
	}

	return addr, true
}

// networkOf returns the network address of addr under mask, as a string.
func networkOf(addr, mask netip.Addr) string {
	a, m := addr.As4(), mask.As4()

	var network [4]byte
	for i := range network {
		network[i] = a[i] & m[i]
	}

	return netip.AddrFrom4(network).String()
}

// isContiguousMask reports whether mask is a run of ones followed by a run of
// zeros. 255.255.255.0 is; 255.0.255.0 is not, and is not a subnet mask however
// well it parses as an address.
func isContiguousMask(mask netip.Addr) bool {
	b := mask.As4()
	value := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])

	// Inverting a contiguous mask gives a value whose bits are all low, so
	// adding one carries into a single high bit — i.e. ^value+1 is a power of
	// two. A zero mask is contiguous by this test and is rejected separately
	// below, since 0.0.0.0 describes no subnet.
	inverted := ^value

	return value != 0 && inverted&(inverted+1) == 0
}
