# Event Catalog

## API-010 — request completed

- **Level:** INFO
- **Category / Topic:** API / Request
- **External source:** yes
- **Description:** Emitted once per HTTP request after the handler returns; the audit line for every request.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| requestId | string | true | Correlation ID (X-Request-Id). |
| method | string | true | HTTP method. |
| path | string | true | Request path. |
| status | int | true | Response status code. |
| durationMs | int | true | Handler duration in milliseconds. |
| bytesWritten | int | true | Response body bytes written. |

**Example:** `{"eventId":"API-010","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…","method":"GET","path":"/api/v1/health"},"data":{"status":200,"durationMs":1,"bytesWritten":147}}`

**Troubleshooting:** Informational request audit line. For error spikes, filter status>=500 and correlate related events by requestId.

## API-011 — request panic recovered

- **Level:** ERROR
- **Category / Topic:** API / Request
- **Description:** A handler panicked; the recovery middleware logged it and returned 500.

| Field | Type | Required | Description |
|---|---|---|---|
| method | string | true | HTTP method. |
| path | string | true | Request path. |
| remoteAddr | string | true | Client address. |
| requestId | string | false | Correlation ID, if the request-ID middleware already ran. |
| panic | string | true | The recovered panic value. |
| stack | string | false | Stack trace captured at the panic. |

**Example:** `{"eventId":"API-011","data":{"method":"GET","path":"/x","remoteAddr":"192.0.2.1:1234","requestId":"…","panic":"runtime error: invalid memory address"}}`

**Troubleshooting:** A handler bug caused a panic. Read the stack field, reproduce via method+path, and fix the root cause (often a nil dereference or out-of-range index). Correlate other events by requestId.

## API-020 — request rejected: no credential

- **Level:** WARN
- **Category / Topic:** API / Auth
- **External source:** yes
- **Description:** A request reached an authenticated route with no Authorization header.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |

**Client response**

- **Problem type:** `weave-adapters:unauthorized`
- **Detail:** Authentication is required. Send 'Authorization: Bearer <token>'.
- **Impacts:** `request_rejected`

**Example:** `{"eventId":"API-020","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{}}`

**Troubleshooting:** Expected from an unconfigured client or a probe. If weave is the caller, link a credential set to the service; see docs/token-management.md.

## API-021 — request rejected: malformed credential

- **Level:** WARN
- **Category / Topic:** API / Auth
- **External source:** yes
- **Description:** A request carried an Authorization header that is not 'Bearer <token>'.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| scheme | string | false | The scheme the caller presented, truncated; "(none)" when the header had no scheme. Never the credential. |

**Client response**

- **Problem type:** `weave-adapters:unauthorized`
- **Detail:** Authorization must use the Bearer scheme, e.g. 'Authorization: Bearer <token>'.
- **Impacts:** `request_rejected`

**Example:** `{"eventId":"API-021","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"scheme":"(none)"}}`

**Troubleshooting:** Most often weave's apiToken holds a bare token: its credential store sends the field verbatim and does not prepend a scheme, so the stored value must read 'Bearer <token>'. See docs/token-management.md.

## API-022 — request rejected: unknown credential

- **Level:** WARN
- **Category / Topic:** API / Auth
- **External source:** yes
- **Description:** A bearer token was presented that matches no configured token.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |

**Client response**

- **Problem type:** `weave-adapters:unauthorized`
- **Detail:** The bearer token is not valid.
- **Impacts:** `request_rejected`

**Example:** `{"eventId":"API-022","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{}}`

**Troubleshooting:** Check the token is listed by `weave-adapter-dhcp-windows token list` and that the adapter was restarted after it was added — tokens are read only at startup. Repeated hits from one address are credential probing.

## API-023 — request rejected: expired credential

- **Level:** WARN
- **Category / Topic:** API / Auth
- **External source:** yes
- **Description:** A recognized bearer token was rejected because its expiry has passed.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| label | string | true | Label of the expired token. |
| expiredAt | string | true | When the token expired (RFC 3339). |

**Client response**

- **Problem type:** `weave-adapters:unauthorized`
- **Detail:** The bearer token is not valid.
- **Impacts:** `request_rejected`

**Example:** `{"eventId":"API-023","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"label":"weave-prod","expiredAt":"2026-10-16T09:02:36Z"}}`

**Troubleshooting:** Mint a replacement with `token gen --label <name> --expires-in-days N`, give it to weave, then restart. The response is identical to an unknown token by design, so this event is the only signal.

## API-900 — request rejected: not found

- **Level:** DEBUG
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** A request addressed a resource that does not exist.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| resource | string | true | The resource that was not found. |

**Client response**

- **Problem type:** `weave-adapters:not-found`
- **Detail:** The requested {{resource}} was not found.
- **Impacts:** `request_rejected`

**Example:** `{"eventId":"API-900","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/openapi.yaml"},"data":{"resource":"openapi document"}}`

**Troubleshooting:** Usually a stale client cache or a deleted resource. Confirm the identifier against a list call.

## API-901 — internal error

- **Level:** ERROR
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** An error reached the HTTP boundary without a taxonomy entry. The client gets a generic 500; the cause is recorded here only.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| error | string | true | The internal error. Never sent to the client. |

**Client response**

- **Problem type:** `weave-adapters:internal`
- **Detail:** An unexpected error occurred.
- **Impacts:** `request_rejected`

**Example:** `{"eventId":"API-901","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"error":"dial tcp 10.0.0.9:445: connect: connection refused"}}`

**Troubleshooting:** An adapter bug: some path returns an error that is not an apierror. Read the error field, then map that failure onto a taxonomy entry at its source.

## API-902 — request rejected: method not allowed

- **Level:** DEBUG
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** A request used a method the route does not accept. The response carries an Allow header.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| method | string | true | The method the caller used. |
| allow | string | false | The methods the route does accept. |

**Client response**

- **Problem type:** `weave-adapters:method-not-allowed`
- **Detail:** The {{method}} method is not allowed on this resource.
- **Impacts:** `request_rejected`

**Example:** `{"eventId":"API-902","caller":{"subject":"weave-prod","role":"service","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"POST","path":"/api/v1/health"},"data":{"method":"POST","allow":"GET, HEAD"}}`

**Troubleshooting:** Client-side fault. The Allow header and the allow field list the accepted methods for that path.

## HLT-001 — health status changed

- **Level:** WARN
- **Category / Topic:** HLT / Status
- **Description:** Emitted when the overall health status transitions between healthy, unhealthy, and unavailable. Emitted only on a change, never on an unchanged poll.

| Field | Type | Required | Description |
|---|---|---|---|
| from | string | true | Previous overall status. |
| to | string | true | New overall status. |

**Example:** `{"eventId":"HLT-001","data":{"from":"healthy","to":"unavailable"}}`

**Troubleshooting:** The adapter's overall health changed. If 'to' is unavailable, the adapter returns 503 and weave stops routing to it — inspect the degraded component via GET /api/v1/health. If 'to' is healthy, a prior problem recovered.

## SYS-001 — adapter starting

- **Level:** INFO
- **Category / Topic:** SYS / Lifecycle
- **Description:** The adapter process has started and is initializing.

| Field | Type | Required | Description |
|---|---|---|---|
| version | string | true | Adapter build version. |

**Example:** `{"eventId":"SYS-001","data":{"version":"1.2.3"}}`

**Troubleshooting:** Informational. Marks the beginning of a process lifecycle.

## SYS-002 — listening

- **Level:** INFO
- **Category / Topic:** SYS / Lifecycle
- **Description:** The HTTP server is listening and ready to serve requests.

| Field | Type | Required | Description |
|---|---|---|---|
| addr | string | true | Listen address (host:port). |

**Example:** `{"eventId":"SYS-002","data":{"addr":":8444"}}`

**Troubleshooting:** If this never appears, the server failed to bind; check the port and permissions.

## SYS-003 — shutdown initiated

- **Level:** INFO
- **Category / Topic:** SYS / Lifecycle
- **Description:** A termination signal was received; the server is draining.

| Field | Type | Required | Description |
|---|---|---|---|
| signal | string | false | The signal that triggered shutdown. |

**Example:** `{"eventId":"SYS-003","data":{"signal":"terminated"}}`

**Troubleshooting:** Informational. Follows SIGINT/SIGTERM or a cancelled run context.

## SYS-004 — shutdown complete

- **Level:** INFO
- **Category / Topic:** SYS / Lifecycle
- **Description:** The server drained and shut down cleanly.

**Example:** `{"eventId":"SYS-004","data":{}}`

**Troubleshooting:** Informational. Marks a clean end to a process lifecycle.

## SYS-005 — startup failed

- **Level:** ERROR
- **Category / Topic:** SYS / Lifecycle
- **Description:** The process failed to start and is exiting non-zero.

| Field | Type | Required | Description |
|---|---|---|---|
| error | string | true | The startup error. |

**Example:** `{"eventId":"SYS-005","data":{"error":"loading config: port must be between 1 and 65535, got 0"}}`

**Troubleshooting:** The process did not start. Read the error field. Most often it is a config problem: check the config file, WEAVE_ADAPTER_* env vars, and flags. Validate the port range and logSeverity value, then re-run.

## SYS-006 — authentication disabled

- **Level:** WARN
- **Category / Topic:** SYS / Lifecycle
- **Description:** The adapter started with disableAuth set: every route except health is open to anyone who can reach the port.

**Example:** `{"eventId":"SYS-006","data":{}}`

**Troubleshooting:** Development-only setting. Unset disableAuth and configure a token (`token gen --label <name>`) before this host is reachable by anything but you.
