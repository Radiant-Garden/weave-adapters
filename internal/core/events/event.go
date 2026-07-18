// Package events is the cataloged event system: every noteworthy log line is a
// registered, documented, stably-identified event rather than an ad-hoc slog
// call. This gives a machine-readable operator log, event-derived API errors
// (later milestone), a live event stream, and an auto-generated catalog — all
// from one place. Ported from weave's internal/daemon/events.
package events

import "log/slog"

// EventID is a stable, unique event identifier (e.g. "SYS-001", "API-030").
type EventID string

// Impact describes a static, machine-readable consequence of an event that a
// consumer can rely on. Used by event-derived API errors in a later milestone.
type Impact int

const (
	// ImpactRequestRejected — the request was not processed.
	ImpactRequestRejected Impact = iota
	// ImpactStateChanged — runtime state transitioned.
	ImpactStateChanged
	// ImpactServiceDegraded — a dependency is impaired.
	ImpactServiceDegraded
	// ImpactConfigReloaded — configuration was reloaded.
	ImpactConfigReloaded
	// ImpactResourceCreated — a new resource was persisted.
	ImpactResourceCreated
	// ImpactResourceUpdated — an existing resource was modified.
	ImpactResourceUpdated
	// ImpactResourceDeleted — a resource was removed.
	ImpactResourceDeleted
)

// String returns the snake_case representation of an Impact.
func (i Impact) String() string {
	switch i {
	case ImpactRequestRejected:
		return "request_rejected"
	case ImpactStateChanged:
		return "state_changed"
	case ImpactServiceDegraded:
		return "service_degraded"
	case ImpactConfigReloaded:
		return "config_reloaded"
	case ImpactResourceCreated:
		return "resource_created"
	case ImpactResourceUpdated:
		return "resource_updated"
	case ImpactResourceDeleted:
		return "resource_deleted"
	default:
		return "unknown"
	}
}

// ResponseCode names a client-facing error class. It lives here rather than in
// apierror because Event carries it: the catalog declares a code, and apierror
// maps it to an HTTP status, title, and problem+json type. Putting the constants
// in apierror would make the catalog import it, and apierror already imports the
// catalog for event IDs.
type ResponseCode string

// The core error taxonomy. Adapters map their backend failures onto these codes;
// they may add their own, but never a second error shape. Codes gain HTTP
// meaning in internal/core/apierror.
//
// Only codes with a live emitter exist. The rest of the taxonomy documented in
// 02-shared-core.md (validation-failed, conflict, precondition-failed,
// backend-*) lands with the code that returns it — the backend codes with the
// first backend client. A registered code nobody emits is a ghost event, which
// .claude/guidelines/event-logging.md rules out.
const (
	// CodeUnauthorized — authentication was missing or invalid (401).
	CodeUnauthorized ResponseCode = "unauthorized"
	// CodeNotFound — the addressed resource does not exist (404).
	CodeNotFound ResponseCode = "not-found"
	// CodeMethodNotAllowed — the route exists but not for this method (405).
	CodeMethodNotAllowed ResponseCode = "method-not-allowed"
	// CodeInternal — an unexpected adapter fault (500).
	CodeInternal ResponseCode = "internal"
)

// FieldDef documents an expected field carried by an event.
type FieldDef struct {
	Name        string // Field name
	Type        string // Field type (string, int, bool, time, ...)
	Required    bool   // Whether the field is required
	Description string // Field description
}

// Event is a cataloged event with its metadata. Events are declared in the
// catalog packages and Register()ed from init().
type Event struct {
	ID              EventID    // Unique identifier (e.g. "SYS-001")
	Level           slog.Level // Log level
	MessageTemplate string     // Message with {{key}} placeholders
	Description     string     // Human-readable description
	Fields          []FieldDef // Expected fields
	Example         string     // JSON example
	Troubleshooting string     // Operator guidance
	Category        string     // Category prefix (SYS, API, HLT, ...)
	Topic           string     // Topic within the category

	// ExternalSource marks events triggered by an inbound HTTP request. When
	// true, Emit extracts caller identity (subject, role, remoteAddr) and
	// request metadata (requestId, method, path) from the context and attaches
	// them as "caller" and "request" groups. Emit panics if it is true and the
	// context carries no remoteAddr — a guard against passing a background
	// context for a request-scoped event.
	ExternalSource bool

	// Event-derived API error fields. If ResponseDetail is set, ResponseCode
	// must be too (enforced at Register). apierror renders the problem+json
	// body from these, so the operator log line and the client error agree.
	ResponseDetail string       // Curated consumer message with {{key}} placeholders
	ResponseCode   ResponseCode // Maps to the problem+json error taxonomy
	Impacts        []Impact     // Static consequences
}

// EventCategory is a category prefix within the catalog.
type EventCategory string

// Core (adapter-agnostic) categories. Adapters register their own categories in
// their own packages; the registry just accumulates them.
const (
	CategorySystem  EventCategory = "SYS" // Lifecycle / shutdown
	CategoryAPI     EventCategory = "API" // HTTP request/response, auth outcomes
	CategoryHealth  EventCategory = "HLT" // Health transitions
	CategoryConfig  EventCategory = "CFG" // Config load/validate
	CategoryCache   EventCategory = "CACHE"
	CategoryJob     EventCategory = "JOB"
	CategoryBackend EventCategory = "BACKEND"
)

// String returns the category prefix as a string.
func (c EventCategory) String() string {
	return string(c)
}
