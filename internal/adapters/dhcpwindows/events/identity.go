package events

import (
	"log/slog"

	coreevents "github.com/radiantgarden/weave-adapters/internal/core/events"
)

// DHCP event IDs — this adapter's own domain category. Range 001–009 is
// reserved for adapter lifecycle.
const (
	// DHCP001 is emitted at startup with the resolved identity inputs.
	DHCP001 coreevents.EventID = "DHCP-001"
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
		Category: coreevents.CategoryDHCP.String(),
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
}
