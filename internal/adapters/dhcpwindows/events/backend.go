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
const (
	// BACKEND101 is emitted when a call to the DHCP backend fails.
	BACKEND101 coreevents.EventID = "BACKEND-101"
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
				Description: "Which backend call failed (listScopes, probe).",
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
}
