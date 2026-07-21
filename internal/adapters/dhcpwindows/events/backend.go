// Package events registers the DHCP-Windows adapter's own cataloged events.
//
// Category ownership works in two halves, and this package is the second one.
// The BACKEND *category constant* lives in internal/core/events, shared by every
// adapter, but each adapter registers its own BACKEND events here, in a
// partitioned ID range — BACKEND-1xx for dhcpwindows, the next adapter takes
// BACKEND-2xx.
//
// The partition is forced by the single-owner rule: each ID is emitted from
// exactly one package. A BACKEND-001 registered in the core catalog would break
// that the moment a second adapter wanted to emit a backend failure, since both
// would have to emit an ID owned by core. Sharing the category while
// partitioning the IDs satisfies both halves of the guideline instead of
// picking one.
//
// A DHCP-xxx category is reserved for DHCP *domain* events that are not call
// failures. It stays empty until something emits one — an event nobody emits is
// a ghost event, and this package registers only what has a live emitter.
package events

import (
	"log/slog"

	coreevents "github.com/radiantgarden/weave-adapters/internal/core/events"
)

// BACKEND event IDs for this adapter. Range 100–199 is this adapter's
// partition; 100–109 is reserved for call outcomes.
//
// The split between 101 and 102–104 mirrors core's split between API-010 and
// the API-9xx block, and it is not redundancy. BACKEND-101 is the operator's
// diagnostic: it is emitted by the client, which is the only layer that knows
// whether the shell failed to start, exited non-zero, timed out or spoke
// nonsense, and it carries the shell's own stderr. BACKEND-102–104 back the
// *responses* — they exist because apierror renders the problem+json body from
// a catalog entry, so each distinct response needs its own ID with its own
// ResponseCode and client-safe detail.
//
// One failed request therefore logs BACKEND-101 at Error with the cause, and
// its response event at Warn. The response events are deliberately quieter:
// the line worth alerting on is the one carrying the diagnostic, and levelling
// both at Error would double every backend outage in the log without adding a
// fact.
const (
	// BACKEND101 is emitted when a call to the DHCP backend fails.
	BACKEND101 coreevents.EventID = "BACKEND-101"

	// BACKEND102 backs a 502 when the backend could not be reached.
	BACKEND102 coreevents.EventID = "BACKEND-102"
	// BACKEND103 backs a 504 when the backend exceeded its timeout.
	BACKEND103 coreevents.EventID = "BACKEND-103"
	// BACKEND104 backs a 502 when the backend answered unusably.
	BACKEND104 coreevents.EventID = "BACKEND-104"
	// BACKEND105 backs a 409 when the subnet already holds a scope.
	BACKEND105 coreevents.EventID = "BACKEND-105"
)

func init() {
	coreevents.Register(&coreevents.Event{
		ID:              BACKEND101,
		Level:           slog.LevelError,
		MessageTemplate: "dhcp backend call failed",
		Description: "Emitted when a call to the Windows DHCP backend fails: the shell could not be run, " +
			"exited non-zero, exceeded its timeout, or returned output that could not be decoded. Emitted by " +
			"the backend client, which is the layer that knows which of those it was — callers above it " +
			"(the health probe, resource handlers) trust this event and do not re-emit.",
		Category: coreevents.CategoryBackend.String(),
		Topic:    "Calls",

		// Deliberately not ExternalSource. The same call fails from a
		// request-scoped handler and from the health probe's background
		// context, and severity and shape are fixed at registration — marking
		// it ExternalSource would make Emit panic the first time the probe ran.
		// The event describes the backend call, not the inbound request.
		ExternalSource: false,

		Fields: []coreevents.FieldDef{
			{
				Name: "operation", Type: "string", Required: true,
				Description: "Which backend call failed (listScopes, createScope, updateScope, deleteScope, probe).",
			},
			{
				Name: "error", Type: "string", Required: true,
				Description: "The failure, including the shell's own stderr where it produced any.",
			},
		},
		Example: `{"eventId":"BACKEND-101","data":{"operation":"listScopes",` +
			`"error":"dhcp backend unavailable: powershell exited 1: Get-DhcpServerv4Scope : Access is denied."}}`,
		Troubleshooting: "Scopes cannot be read, so /api/v1/scopes fails and the dhcp-server health component " +
			"reports unavailable. Reproduce with: powershell.exe -NoProfile -NonInteractive -Command " +
			"\"Get-DhcpServerv4Scope\". Likely causes: the RSAT-DHCP feature is not installed, so the DhcpServer " +
			"module is missing (Install-WindowsFeature RSAT-DHCP); the service account lacks DHCP read rights " +
			"(add it to the DHCP Users group); or dhcp.server names a host that is unreachable or not running " +
			"the DHCP Server role. A timeout instead suggests a slow or wedged host — raise dhcp.commandTimeout " +
			"only after confirming the query is slow rather than hung. Escalate to the Windows server owner.",
	})

	// BACKEND105 is registered on its own rather than in responseEvents, because
	// it differs in the two fields that table holds constant.
	//
	// Level: the others are Warn because they record that a request could not be
	// served. This one records that it was served correctly with a "no" — an
	// ordinary 4xx, which the guideline's severity table puts at Debug alongside
	// API-900/902/903.
	//
	// Description: the others point at a BACKEND-101 line carrying the cause.
	// There is none here, and saying so matters — an operator who goes looking
	// for the companion ERROR line would find nothing and conclude the log had
	// dropped it. The create path returns a conflict without emitting
	// BACKEND-101, because nothing failed.
	coreevents.Register(&coreevents.Event{
		ID:              BACKEND105,
		Level:           slog.LevelDebug,
		MessageTemplate: "request rejected: scope already exists",
		Description: "Emitted when a create names a subnet that already holds a scope. Windows permits exactly " +
			"one scope per subnet, so this is the backend answering correctly rather than failing — there is no " +
			"BACKEND-101 line for it.",
		Category:       coreevents.CategoryBackend.String(),
		Topic:          "Calls",
		ExternalSource: true,

		ResponseCode:   coreevents.CodeConflict,
		ResponseDetail: "A scope already exists on subnet {{scopeId}}.",
		Impacts:        []coreevents.Impact{coreevents.ImpactRequestRejected},

		Fields: append(coreevents.CallerFields(), coreevents.FieldDef{
			Name: "scopeId", Type: "string", Required: true,
			Description: "The subnet that already holds a scope.",
		}),
		Example: `{"eventId":"BACKEND-105","caller":{"subject":"weave-prod","role":"service",` +
			`"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"POST",` +
			`"path":"/api/v1/scopes"},"data":{"scopeId":"10.0.30.0"}}`,
		Troubleshooting: "Not a fault. The caller asked for a subnet that is already scoped; the answer is to " +
			"update the existing scope rather than create a second one, which Windows would refuse anyway. " +
			"GET /api/v1/scopes?scopeId=<subnet> returns the one that is there. Note the pre-create check is not " +
			"atomic: two creates racing on one subnet can both pass it, and the loser surfaces as a backend error " +
			"rather than as this event.",
	})

	for _, r := range responseEvents {
		coreevents.Register(&coreevents.Event{
			ID:              r.id,
			Level:           slog.LevelWarn,
			MessageTemplate: r.message,
			Description: r.describe + " Backs the client-facing response; the cause is on the BACKEND-101 line " +
				"emitted for the same failure, which carries the shell's own stderr.",
			Category: coreevents.CategoryBackend.String(),
			Topic:    "Calls",

			// ExternalSource: these are emitted only from a request-scoped
			// handler through apierror.WriteError, so a caller is always in
			// context — unlike BACKEND-101, which the health probe also emits
			// from a background context.
			ExternalSource: true,

			ResponseCode:   r.code,
			ResponseDetail: r.detail,
			Impacts:        []coreevents.Impact{coreevents.ImpactRequestRejected},

			Fields: append(coreevents.CallerFields(), coreevents.FieldDef{
				Name: "operation", Type: "string", Required: true,
				Description: "Which backend call failed (listScopes, createScope, updateScope, deleteScope).",
			}),
			Example: `{"eventId":"` + string(r.id) + `","caller":{"subject":"weave-prod","role":"service",` +
				`"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET",` +
				`"path":"/api/v1/scopes"},"data":{"operation":"listScopes"}}`,
			Troubleshooting: r.fix,
		})
	}
}

// responseEvents declares the client-facing backend failures.
//
// A table rather than three Register calls because they differ only in the four
// fields below; spelling out the shared two-thirds three times is how one of
// them ends up with a different category or a missing Impacts entry.
var responseEvents = []struct {
	id       coreevents.EventID
	code     coreevents.ResponseCode
	message  string
	detail   string
	describe string
	fix      string
}{
	{
		id:      BACKEND102,
		code:    coreevents.CodeBackendUnavailable,
		message: "request failed: dhcp backend unavailable",
		detail:  "The DHCP server could not be reached.",
		describe: "Emitted when a request could not be served because the backend was unreachable — the shell " +
			"would not run, or it exited non-zero.",
		fix: "Same causes as BACKEND-101: the RSAT-DHCP feature is missing, the service account lacks DHCP " +
			"read rights, or dhcp.server names an unreachable host. The dhcp-server health component reports " +
			"unavailable for the same reason, so check /api/v1/health first — if it is also failing, this is " +
			"an outage rather than a request-specific fault.",
	},
	{
		id:       BACKEND103,
		code:     coreevents.CodeBackendTimeout,
		message:  "request failed: dhcp backend timed out",
		detail:   "The DHCP server did not respond in time.",
		describe: "Emitted when a request could not be served because the backend exceeded dhcp.commandTimeout.",
		fix: "A slow or wedged host rather than a broken one. Time the query directly before touching config: " +
			"Measure-Command { Get-DhcpServerv4Scope }. Raise dhcp.commandTimeout only if the query is " +
			"genuinely slow — raising it to cover a hung host just makes every request hang longer. Note that " +
			"dhcp.probeTimeout must stay below it.",
	},
	{
		id:      BACKEND104,
		code:    coreevents.CodeBackendError,
		message: "request failed: dhcp backend returned malformed output",
		detail:  "The DHCP server returned a response the adapter could not read.",
		describe: "Emitted when a request could not be served because the backend exited zero but its output " +
			"could not be decoded — including empty output, a bare null, and a scope with no usable scopeId.",
		fix: "This one is an adapter or environment defect rather than a permissions problem, because the shell " +
			"reported success. Reproduce with the projection the client sends and inspect the raw bytes. Likely " +
			"causes: a PowerShell profile writing to stdout despite -NoProfile, a -Depth regression serializing " +
			"nested values as \"System.Object[]\", or an output encoding that mangles non-ASCII scope names. The " +
			"BACKEND-101 line for the same failure carries the decode error itself.",
	},
}
