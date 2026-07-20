package catalog

import (
	"log/slog"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// API event IDs. Range 010–019 is reserved for the request lifecycle, 020–029
// for auth outcomes, and 900–999 for general client-facing errors — each of the
// latter two backing one problem+json response.
//
// Only errors with a live emitter are registered. The rest of the taxonomy in
// 02-shared-core.md arrives with the code that returns it: unauthorized with
// the auth middleware, the backend codes with the first backend client. See
// .claude/guidelines/event-logging.md on ghost events.
const (
	// API010 is emitted once per HTTP request after the handler returns.
	API010 events.EventID = "API-010"
	// API011 is emitted when a handler panics and recovery returns 500.
	API011 events.EventID = "API-011"

	// API012 is emitted when a response was too large to tag conditionally and
	// was streamed through untagged.
	API012 events.EventID = "API-012"

	// API020 is emitted when a request carries no Authorization header.
	API020 events.EventID = "API-020"
	// API021 is emitted when the Authorization header uses a scheme other than
	// Bearer, or is otherwise malformed.
	API021 events.EventID = "API-021"
	// API022 is emitted when a bearer token matches no configured token.
	API022 events.EventID = "API-022"
	// API023 is emitted when a bearer token is recognized but has expired.
	API023 events.EventID = "API-023"

	// API900 backs a 404 not-found response.
	API900 events.EventID = "API-900"
	// API902 backs a 405 method-not-allowed response.
	API902 events.EventID = "API-902"
	// API901 backs a 500 internal response — the catch-all for an error that
	// reached the HTTP boundary without a taxonomy entry.
	API901 events.EventID = "API-901"
	// API903 backs a 400 validation-failed response.
	API903 events.EventID = "API-903"
	// API904 backs a 413 payload-too-large response.
	API904 events.EventID = "API-904"
	// API905 backs a 415 unsupported-media-type response.
	API905 events.EventID = "API-905"
)

// clientError describes one entry in the client-facing error range. Declaring
// them as data keeps the registrations honest: every one carries the same
// shape, and a missing ResponseCode is impossible rather than merely caught.
type clientError struct {
	id       events.EventID
	topic    string
	level    slog.Level
	message  string
	detail   string // ResponseDetail: curated, client-visible, {{key}} placeholders
	code     events.ResponseCode
	fields   []events.FieldDef
	example  string
	describe string
	fix      string
}

// clientErrors covers both the auth-outcome range (020–029) and the general
// client-error range (900–999). A rejected credential is WARN — the guideline's
// severity table puts "auth failure/denied" there, because an operator should
// plan corrective action — while an ordinary 4xx is DEBUG and a 5xx is ERROR.
//
// **The four auth failures are four events but not four bodies.** Each has its
// own troubleshooting, so the guideline's merge test keeps them separate. Their
// responses differ only where the difference tells the caller something it
// already knows: that it sent no header (API-020), or the wrong scheme
// (API-021). Unknown (API-022) and expired (API-023) return byte-identical
// bodies on purpose — distinguishing them would confirm to an attacker that a
// guessed token exists, turning the endpoint into a validity oracle.
var clientErrors = []clientError{
	{
		id: API020, topic: "Auth", level: slog.LevelWarn, message: "request rejected: no credential",
		detail: "Authentication is required. Send 'Authorization: Bearer <token>'.", code: events.CodeUnauthorized,
		example:  `{"eventId":"API-020","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{}}`,
		describe: "A request reached an authenticated route with no Authorization header.",
		fix: "Expected from an unconfigured client or a probe. If weave is the caller, link a credential set to " +
			"the service; see docs/token-management.md.",
	},
	{
		id: API021, topic: "Auth", level: slog.LevelWarn, message: "request rejected: malformed credential",
		detail: "Authorization must use the Bearer scheme, e.g. 'Authorization: Bearer <token>'.", code: events.CodeUnauthorized,
		fields: []events.FieldDef{
			{Name: "scheme", Type: "string", Required: false, Description: "The scheme the caller presented, truncated; \"(none)\" when the header had no scheme. Never the credential."},
		},
		example:  `{"eventId":"API-021","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"scheme":"(none)"}}`,
		describe: "A request carried an Authorization header that is not 'Bearer <token>'.",
		fix: "Most often weave's apiToken holds a bare token: its credential store sends the field verbatim and does " +
			"not prepend a scheme, so the stored value must read 'Bearer <token>'. See docs/token-management.md.",
	},
	{
		id: API022, topic: "Auth", level: slog.LevelWarn, message: "request rejected: unknown credential",
		detail: "The bearer token is not valid.", code: events.CodeUnauthorized,
		example:  `{"eventId":"API-022","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{}}`,
		describe: "A bearer token was presented that matches no configured token.",
		fix: "Check the token is listed by `weave-adapter-dhcp-windows token list` and that the adapter was restarted " +
			"after it was added — tokens are read only at startup. Repeated hits from one address are credential probing.",
	},
	{
		id: API023, topic: "Auth", level: slog.LevelWarn, message: "request rejected: expired credential",
		detail: "The bearer token is not valid.", code: events.CodeUnauthorized,
		fields: []events.FieldDef{
			{Name: "label", Type: "string", Required: true, Description: "Label of the expired token."},
			{Name: "expiredAt", Type: "string", Required: true, Description: "When the token expired (RFC 3339)."},
		},
		example:  `{"eventId":"API-023","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"label":"weave-prod","expiredAt":"2026-10-16T09:02:36Z"}}`,
		describe: "A recognized bearer token was rejected because its expiry has passed.",
		fix: "Mint a replacement with `token gen --label <name> --expires-in-days N`, give it to weave, then restart. " +
			"The response is identical to an unknown token by design, so this event is the only signal.",
	},
	{
		id: API900, topic: "Errors", level: slog.LevelDebug, message: "request rejected: not found",
		detail: "The requested {{resource}} was not found.", code: events.CodeNotFound,
		fields:   []events.FieldDef{{Name: "resource", Type: "string", Required: true, Description: "The resource that was not found."}},
		example:  `{"eventId":"API-900","caller":{"subject":"weave-prod","role":"service","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"resource":"route GET /api/v1/leases"}}`,
		describe: "A request addressed a resource that does not exist.",
		fix:      "Usually a stale client cache or a deleted resource. Confirm the identifier against a list call.",
	},
	{
		id: API902, topic: "Errors", level: slog.LevelDebug, message: "request rejected: method not allowed",
		detail: "The {{method}} method is not allowed on this resource.", code: events.CodeMethodNotAllowed,
		fields: []events.FieldDef{
			{Name: "method", Type: "string", Required: true, Description: "The method the caller used."},
			{Name: "allow", Type: "string", Required: false, Description: "The methods the route does accept."},
		},
		example:  `{"eventId":"API-902","caller":{"subject":"weave-prod","role":"service","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"POST","path":"/api/v1/health"},"data":{"method":"POST","allow":"GET, HEAD"}}`,
		describe: "A request used a method the route does not accept. The response carries an Allow header.",
		fix:      "Client-side fault. The Allow header and the allow field list the accepted methods for that path.",
	},
	{
		id: API903, topic: "Errors", level: slog.LevelDebug, message: "request rejected: validation failed",
		detail: "The request has invalid parameters.", code: events.CodeValidationFailed,
		fields: []events.FieldDef{
			{Name: "fields", Type: "string", Required: true, Description: "Comma-separated names of the fields that failed. The per-field messages are in the response body's errors[]."},
		},
		example:  `{"eventId":"API-903","caller":{"subject":"weave-prod","role":"service","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"fields":"pageSize, pageToken"}}`,
		describe: "A request carried parameters or body fields the endpoint rejected. Every failure is listed in one response, not just the first.",
		fix: "Client-side fault; the response body's errors[] names each field and what was expected. A recurring " +
			"pageToken failure usually means the client stored a token across a listing whose scope changed — it " +
			"should drop the token and list from the first page.",
	},
	{
		id: API904, topic: "Errors", level: slog.LevelDebug, message: "request rejected: body too large",
		detail: "The request body exceeds the {{limitBytes}} byte limit.", code: events.CodePayloadTooLarge,
		fields: []events.FieldDef{
			{Name: "limitBytes", Type: "int", Required: true, Description: "The configured maxRequestBodyBytes the body exceeded."},
		},
		example:  `{"eventId":"API-904","caller":{"subject":"weave-prod","role":"service","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"POST","path":"/api/v1/scopes"},"data":{"limitBytes":1048576}}`,
		describe: "A request body exceeded maxRequestBodyBytes and was rejected before the decoder read it.",
		fix: "The limit is three orders of magnitude above any single resource this adapter accepts, so a genuine " +
			"client should never reach it — treat a hit as a malformed or hostile request first, and raise " +
			"maxRequestBodyBytes only after confirming the payload is legitimate. The byte count is not logged " +
			"because the body was never read; only the limit is known.",
	},
	{
		id: API905, topic: "Errors", level: slog.LevelDebug, message: "request rejected: unsupported media type",
		detail: "The request body must be sent as application/json.", code: events.CodeUnsupportedMediaType,
		fields: []events.FieldDef{
			{Name: "contentType", Type: "string", Required: false, Description: "The media type the caller sent, truncated; \"(none)\" when the header was absent."},
		},
		example:  `{"eventId":"API-905","caller":{"subject":"weave-prod","role":"service","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"POST","path":"/api/v1/scopes"},"data":{"contentType":"text/plain"}}`,
		describe: "A request carried a body whose Content-Type is not application/json.",
		fix: "Client-side fault. The adapter speaks one media type on the request side; a caller sending form data " +
			"or no Content-Type at all lands here. Note this fires before decoding, so a body that is valid JSON " +
			"but mislabelled is still rejected — the header is the contract, not the bytes.",
	},
	{
		id: API901, topic: "Errors", level: slog.LevelError, message: "internal error",
		detail: "An unexpected error occurred.", code: events.CodeInternal,
		fields:  []events.FieldDef{{Name: "error", Type: "string", Required: true, Description: "The internal error. Never sent to the client."}},
		example: `{"eventId":"API-901","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"error":"dial tcp 10.0.0.9:445: connect: connection refused"}}`,
		describe: "An error reached the HTTP boundary without a taxonomy entry. The client gets a generic 500; " +
			"the cause is recorded here only.",
		fix: "An adapter bug: some path returns an error that is not an apierror. Read the error field, " +
			"then map that failure onto a taxonomy entry at its source.",
	},
}

func init() {
	events.Register(&events.Event{
		ID:              API010,
		Level:           slog.LevelInfo,
		MessageTemplate: "request completed",
		Description:     "Emitted once per HTTP request after the handler returns; the audit line for every request.",
		Category:        events.CategoryAPI.String(),
		Topic:           "Request",
		ExternalSource:  true,
		Fields: append(events.CallerFields(), []events.FieldDef{
			{Name: "requestId", Type: "string", Required: true, Description: "Correlation ID (X-Request-Id)."},
			{Name: "method", Type: "string", Required: true, Description: "HTTP method."},
			{Name: "path", Type: "string", Required: true, Description: "Request path."},
			{Name: "status", Type: "int", Required: true, Description: "Response status code."},
			{Name: "durationMs", Type: "int", Required: true, Description: "Handler duration in milliseconds."},
			{Name: "bytesWritten", Type: "int", Required: true, Description: "Response body bytes written."},
		}...),
		Example:         `{"eventId":"API-010","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…","method":"GET","path":"/api/v1/health"},"data":{"status":200,"durationMs":1,"bytesWritten":147}}`,
		Troubleshooting: "Informational request audit line. For error spikes, filter status>=500 and correlate related events by requestId.",
	})

	events.Register(&events.Event{
		ID:              API011,
		Level:           slog.LevelError,
		MessageTemplate: "request panic recovered",
		Description:     "A handler panicked; the recovery middleware logged it and returned 500.",
		Category:        events.CategoryAPI.String(),
		Topic:           "Request",
		Fields: []events.FieldDef{
			{Name: "method", Type: "string", Required: true, Description: "HTTP method."},
			{Name: "path", Type: "string", Required: true, Description: "Request path."},
			{Name: "remoteAddr", Type: "string", Required: true, Description: "Client address."},
			{Name: "requestId", Type: "string", Required: false, Description: "Correlation ID, if the request-ID middleware already ran."},
			{Name: "panic", Type: "string", Required: true, Description: "The recovered panic value."},
			{Name: "stack", Type: "string", Required: false, Description: "Stack trace captured at the panic."},
		},
		Example: `{"eventId":"API-011","data":{"method":"GET","path":"/x","remoteAddr":"192.0.2.1:1234","requestId":"…","panic":"runtime error: invalid memory address"}}`,
		Troubleshooting: "A handler bug caused a panic. Read the stack field, reproduce via method+path, and fix the root cause " +
			"(often a nil dereference or out-of-range index). Correlate other events by requestId.",
	})

	events.Register(&events.Event{
		ID:              API012,
		Level:           slog.LevelWarn,
		MessageTemplate: "response too large to tag",
		Description: "A conditionally-read response exceeded the size the ETag wrapper will buffer, so it was " +
			"streamed through without an ETag. Clients cannot cache it and every poll pays for the full body.",
		Category: events.CategoryAPI.String(),
		Topic:    "Request",
		Fields: []events.FieldDef{
			{Name: "path", Type: "string", Required: true, Description: "The route that produced the oversized response."},
			{Name: "limitBytes", Type: "int", Required: true, Description: "The buffering limit that was exceeded."},
		},
		Example: `{"eventId":"API-012","data":{"path":"/api/v1/leases","limitBytes":4194304}}`,
		Troubleshooting: "The route returns an unbounded collection. Add or lower pagination (pageSize) so a page " +
			"fits the limit, or stop wrapping the handler in etag.Conditional if the resource is genuinely a stream.",
	})

	for _, ce := range clientErrors {
		events.Register(&events.Event{
			ID:              ce.id,
			Level:           ce.level,
			MessageTemplate: ce.message,
			Description:     ce.describe,
			Category:        events.CategoryAPI.String(),
			Topic:           ce.topic,
			ExternalSource:  true,
			Fields:          append(events.CallerFields(), ce.fields...),
			Example:         ce.example,
			Troubleshooting: ce.fix,
			ResponseDetail:  ce.detail,
			ResponseCode:    ce.code,
			Impacts:         []events.Impact{events.ImpactRequestRejected},
		})
	}
}
