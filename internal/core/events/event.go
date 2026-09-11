// Package events is the cataloged event system: every noteworthy log line is a
// registered, documented, stably-identified event rather than an ad-hoc slog
// call. This gives a machine-readable operator log, event-derived API errors,
// and an auto-generated catalog — all from one place. Ported from weave's
// internal/daemon/events; the subscriber fan-out that port carried was dropped
// here as speculative — no milestone consumes it — and returns with the endpoint
// that needs it.
package events

import "log/slog"

// EventID is a stable, unique event identifier (e.g. "SYS-001", "API-030").
type EventID string

// Impact describes a static, machine-readable consequence of an event that a
// consumer can rely on — rendered into docs/events.md and available for a SIEM
// to filter on.
//
// Only the impacts something actually declares live here. The vocabulary once
// carried seven, six of them unused — the same speculative-code smell the rest
// of the repo forbids, sitting in core. A milestone that emits a state
// transition or a resource lifecycle event adds its impact back in the same
// commit that consumes it, next to the emitter, which is the rule everywhere
// else.
type Impact int

const (
	// ImpactRequestRejected — the request was not processed.
	ImpactRequestRejected Impact = iota
)

// String returns the snake_case representation of an Impact.
func (i Impact) String() string {
	switch i {
	case ImpactRequestRejected:
		return "request_rejected"
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
// 02-shared-core.md (conflict, precondition-failed) lands with the code that
// returns it. A registered code nobody emits is a ghost event, which
// .claude/guidelines/event-logging.md rules out.
const (
	// CodeValidationFailed — the request's parameters or body were rejected (400).
	CodeValidationFailed ResponseCode = "validation-failed"
	// CodeUnauthorized — authentication was missing or invalid (401).
	CodeUnauthorized ResponseCode = "unauthorized"
	// CodeNotFound — the addressed resource does not exist (404).
	CodeNotFound ResponseCode = "not-found"
	// CodeMethodNotAllowed — the route exists but not for this method (405).
	CodeMethodNotAllowed ResponseCode = "method-not-allowed"
	// CodeInternal — an unexpected adapter fault (500).
	CodeInternal ResponseCode = "internal"
	// CodePayloadTooLarge — the request body exceeded maxRequestBodyBytes (413).
	CodePayloadTooLarge ResponseCode = "payload-too-large"
	// CodeUnsupportedMediaType — the request body was not application/json (415).
	CodeUnsupportedMediaType ResponseCode = "unsupported-media-type"
	// CodeConflict — the resource cannot be created or changed as asked because
	// it would collide with one that already exists (409).
	CodeConflict ResponseCode = "conflict"
)

// The backend codes. An adapter maps its backend's failures onto these; the
// events that carry them are the adapter's own, in its own category, because
// only the adapter knows which failure it hit.
//
// They are separate codes rather than one generic 502 because the distinction
// is the only part of the failure weave can act on. Its response classifier
// reads the status code and nothing else — it never decodes an error body — so
// anything it must distinguish has to be expressible there. "The backend is
// unreachable" and "the backend answered with nonsense" want different
// operator responses and plausibly different retry behaviour; collapsing them
// into 500 would put both, plus every adapter bug, behind one code.
const (
	// CodeBackendUnavailable — the backend could not be reached or refused the
	// call (502).
	CodeBackendUnavailable ResponseCode = "backend-unavailable"
	// CodeBackendTimeout — the backend did not answer within its timeout (504).
	CodeBackendTimeout ResponseCode = "backend-timeout"
	// CodeBackendError — the backend answered, but with something the adapter
	// could not use (502).
	CodeBackendError ResponseCode = "backend-error"
)

// FieldDef documents an expected field carried by an event.
type FieldDef struct {
	Name        string // Field name
	Type        string // Field type (string, int, bool, time, ...)
	Required    bool   // Whether the field is required
	Description string // Field description
}

// MaxEventLogID is the highest Windows Event Log ID that renders.
//
// Entries are registered against EventCreate.exe as their message file, whose
// table stops short of this. Measured on Windows Server 2022: 1001 rendered,
// while 1500 and 65535 wrote without error and displayed nothing. 1000 is the
// documented figure and sits safely inside what was observed, so it is the
// bound rather than the measured edge.
const MaxEventLogID uint32 = 1000

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

	// EventLogID is this event's numeric ID in the Windows Event Log. Zero
	// means the event is not mirrored there.
	//
	// It is declared per event, by the package that owns the event, for the
	// same reason Level is: the owner knows whether an operator would open
	// Event Viewer for this. Deriving it from the catalog ID is not an option
	// — the digits collide across categories (SYS-001 and HLT-001 both give 1)
	// — and a central table would have to name every adapter's events from
	// core, which cannot see them.
	//
	// The number is an operator-facing contract: it is what an Event Viewer
	// filter matches on, so renumbering breaks whoever wrote that filter.
	// Allocate a new one rather than reusing a retired one.
	//
	// Ranges, by convention rather than by enforcement (only uniqueness and
	// the ceiling are checked):
	//
	//	1-99     SYS        400-499  reserved for core
	//	100-199  API        500-599  dhcpwindows
	//	200-299  HLT        600-699  the next adapter
	//	300-399  CFG
	//
	// The ceiling is not arbitrary: entries render through EventCreate.exe's
	// message table, and IDs above it were measured on Windows Server 2022 to
	// write successfully and display as an empty message — a silent failure
	// with no error to catch at runtime, which is why Register checks it.
	EventLogID uint32

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
	CategorySystem EventCategory = "SYS" // Lifecycle / shutdown
	CategoryAPI    EventCategory = "API" // HTTP request/response, auth outcomes
	CategoryHealth EventCategory = "HLT" // Health transitions
	CategoryConfig EventCategory = "CFG" // Config load/validate
	CategoryCache  EventCategory = "CACHE"
	CategoryJob    EventCategory = "JOB"

	// CategoryBackend is shared by every adapter's backend client. It is the one
	// category core declares without owning any of its events: the constant
	// lives here so nobody defines a second one, while each adapter registers
	// its own BACKEND-xxx in a partitioned ID range. An adapter's *own* domain
	// category is declared in that adapter's package, not here — see
	// .claude/guidelines/event-logging.md.
	CategoryBackend EventCategory = "BACKEND"
)

// String returns the category prefix as a string.
func (c EventCategory) String() string {
	return string(c)
}
