package events

import (
	"log/slog"

	coreevents "github.com/radiantgarden/weave-adapters/internal/core/events"
)

// CategoryDHCP is this adapter's own domain category, declared here rather than
// in core.
//
// The distinction from CategoryBackend is the whole point of the split: BACKEND
// is shared by every adapter, so its constant lives in core to stop a second
// one being invented. DHCP belongs to this adapter alone, so core has no reason
// to know the prefix exists — putting it there would make the adapter-agnostic
// package name a specific backend, which is the rule CLAUDE.md exists to hold.
const CategoryDHCP coreevents.EventCategory = "DHCP"

// DHCP event IDs — this adapter's own domain category. Range 001–009 is
// reserved for adapter lifecycle.
const (
	// DHCP001 is emitted at startup with the resolved identity inputs.
	DHCP001 coreevents.EventID = "DHCP-001"
	// DHCP002 is emitted when a wadaptID's scope changed materially, which may
	// mean the identity now denotes a different scope.
	DHCP002 coreevents.EventID = "DHCP-002"
)

func init() {
	coreevents.Register(&coreevents.Event{
		ID:              DHCP001,
		Level:           slog.LevelInfo,
		MessageTemplate: "dhcp adapter identity resolved",
		Description: "Emitted once at startup with the inputs every wadaptID is derived from. Registered here " +
			"but emitted by the binary's wiring, which is where startup events are owned — the same split as " +
			"SYS-001. Its purpose is diagnostic: because the read path is stateless, nothing persists a previous " +
			"identity to compare against, so an accidental re-key is otherwise invisible until a wall of sync " +
			"failures appears hours later. This turns it into one log line at startup.",
		Category: CategoryDHCP.String(),
		Topic:    "Identity",

		Fields: []coreevents.FieldDef{
			{
				Name: "serverName", Type: "string", Required: true,
				Description: "The canonicalized identity.serverName hashed into every wadaptID.",
			},
			{
				Name: "namespaceKeyFingerprint", Type: "string", Required: true,
				Description: "A short hash of identity.namespaceKey — never the key itself. Enough to tell " +
					"two deployments apart, and to notice a key that changed.",
			},
		},
		Example: `{"eventId":"DHCP-001","data":{"serverName":"dhcp01.example.test",` +
			`"namespaceKeyFingerprint":"3f9a2c11"}}`,
		Troubleshooting: "Informational at startup, no action. It becomes actionable when either field changed " +
			"unexpectedly between restarts: every wadaptID is then different, so weave sees every scope as gone " +
			"and proposes a recreate for each, which Windows rejects because a subnet already holds a scope — " +
			"sync stalls loudly rather than deleting anything. Compare both values against the previous start. " +
			"A changed serverName usually means a host rename, dhcp.server being set for the first time, or a " +
			"short-name/FQDN switch. A changed fingerprint means identity.namespaceKey was lost or rotated; " +
			"restore it from backup rather than accepting the new one.",
	})

	coreevents.Register(&coreevents.Event{
		ID:              DHCP002,
		Level:           slog.LevelWarn,
		MessageTemplate: "scope attributes changed materially for an existing wadaptID",
		Description: "Emitted when a wadaptID that was seen before now carries materially different attributes " +
			"— name, ranges, subnet mask or lease duration. This is the detection half of the adapter's one " +
			"accepted silent failure: a wadaptID is derived from the server name and the subnet and nothing else, " +
			"because the host exposes no creation time and no GUID, so a scope deleted and recreated on the same " +
			"subnet derives the *same* identity as its predecessor. It cannot be prevented; this makes it " +
			"visible. A legitimate edit to an existing scope fires it too, which is the accepted cost of having " +
			"no way to tell the two apart.",
		Category: CategoryDHCP.String(),
		Topic:    "Identity",

		// Deliberately not ExternalSource, for the reason BACKEND-101 is not:
		// this describes the data, not a request. It is emitted from whatever
		// call observed the listing, which includes paths with no caller in
		// context, and Emit panics on an ExternalSource event without one.
		ExternalSource: false,

		Fields: []coreevents.FieldDef{
			{
				Name: "wadaptId", Type: "string", Required: true,
				Description: "The identity whose scope changed. This is the value weave holds.",
			},
			{
				Name: "scopeId", Type: "string", Required: true,
				Description: "The subnet the identity derives from — unchanged by definition, since it is an input.",
			},
			{
				Name: "changed", Type: "string", Required: true,
				Description: "Comma-separated names of the attributes that differ from the last observation.",
			},
		},
		// A real wadaptID is exactly WadaptIDLength (13) characters of lowercase
		// base32hex (0-9a-v) — the earlier example was 16 characters and contained
		// a 'y', which the alphabet has no room for, so it could never be a value
		// this event actually carries.
		Example: `{"eventId":"DHCP-002","data":{"wadaptId":"c7k3n2q8r4t6v",` +
			`"scopeId":"10.0.30.0","changed":"name, startRange, endRange"}}`,
		Troubleshooting: "Decide which of two things happened, because they need opposite responses. If somebody " +
			"edited an existing scope, nothing is wrong and the event is noise. If a scope was deleted and a new " +
			"one created on the same subnet, weave is now bound to the wrong object: it holds desired state for " +
			"the scope that is gone and will apply it to the scope that replaced it, with no error and no retry " +
			"— the adapter cannot tell, which is why this is a warning rather than a rejection. Confirm against " +
			"the DHCP server's own change history, and if it was a recreate, reconcile the weave-side object " +
			"deliberately rather than letting the next sync do it. Note the ledger is in memory: a restart " +
			"re-baselines, so an absent event after one proves nothing.",
	})
}
