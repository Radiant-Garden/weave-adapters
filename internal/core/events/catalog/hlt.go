package catalog

import (
	"log/slog"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// HLT event IDs. Range 001–009 is reserved for status.
const (
	// HLT001 is emitted when the overall health status changes.
	HLT001 events.EventID = "HLT-001"
)

func init() {
	events.Register(&events.Event{
		ID:              HLT001,
		Level:           slog.LevelWarn,
		MessageTemplate: "health status changed",
		Description: "Emitted when the overall health status transitions between healthy, unhealthy, and " +
			"unavailable. Emitted only on a change, never on an unchanged poll.",
		Category: events.CategoryHealth.String(),
		Topic:    "Status",
		Fields: []events.FieldDef{
			{Name: "from", Type: "string", Required: true, Description: "Previous overall status."},
			{Name: "to", Type: "string", Required: true, Description: "New overall status."},
		},
		Example: `{"eventId":"HLT-001","data":{"from":"healthy","to":"unavailable"}}`,
		Troubleshooting: "The adapter's overall health changed. If 'to' is unavailable, the adapter returns 503 and " +
			"weave stops routing to it — inspect the degraded component via GET /api/v1/health. If 'to' is healthy, a " +
			"prior problem recovered.",
	})
}
