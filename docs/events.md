# Event Catalog

## API-010 — request completed

- **Level:** INFO
- **Category / Topic:** API / Request
- **External source:** yes
- **Description:** Emitted once per HTTP request after the handler returns; the audit line for every request.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands in M2). |
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

## API-900 — request rejected: validation failed

- **Level:** DEBUG
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** A request was rejected because it failed validation. The response lists every failing field.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| fieldErrors | int | false | Number of failing fields. |

**Example:** `{"eventId":"API-900","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** Client-side fault. Read the errors[] array in the response body; each entry names a field and why it failed.

## API-901 — request rejected: unauthorized

- **Level:** DEBUG
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** A request was rejected because its credential was missing, malformed, or unknown.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| reason | string | true | Client-safe reason the credential was rejected. |

**Example:** `{"eventId":"API-901","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** Check the caller sends 'Authorization: Bearer <token>' with the full scheme, and that the token is listed by `token list` and not expired. See docs/token-management.md.

## API-902 — request rejected: not found

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

**Example:** `{"eventId":"API-902","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** Usually a stale client cache or a deleted resource. Confirm the identifier against a list call.

## API-903 — request rejected: conflict

- **Level:** DEBUG
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** A request conflicted with the current state of the resource.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| reason | string | true | Client-safe description of the conflict. |

**Example:** `{"eventId":"API-903","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** Re-read the resource and retry against its current state.

## API-904 — request rejected: precondition failed

- **Level:** DEBUG
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** A conditional request failed because the resource changed since the client last read it.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| expected | string | false | The If-Match value supplied. |

**Example:** `{"eventId":"API-904","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** Expected under concurrent writes. The client should re-read, re-apply, and retry with the new ETag.

## API-905 — backend unreachable

- **Level:** ERROR
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** The adapter could not contact its backend service at all.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| backend | string | true | The backend that could not be reached. |

**Example:** `{"eventId":"API-905","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** Check the backend is running and reachable from this host: DNS, routing, firewall, and credentials.

## API-906 — backend error

- **Level:** ERROR
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** The backend was reached but answered with a failure.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| backend | string | true | The backend that failed. |

**Example:** `{"eventId":"API-906","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** Read the backendError field in the response and the backend's own logs; the fault is on the backend side.

## API-907 — backend timeout

- **Level:** ERROR
- **Category / Topic:** API / Errors
- **External source:** yes
- **Description:** A backend call exceeded its deadline.

| Field | Type | Required | Description |
|---|---|---|---|
| subject | string | false | Authenticated caller (empty until auth lands). |
| role | string | false | Caller role (empty until auth). |
| remoteAddr | string | true | Client address. |
| backend | string | true | The backend that timed out. |

**Example:** `{"eventId":"API-907","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** Check backend load and health. Persistent timeouts mean the backend is overloaded or the timeout is too tight.

## API-908 — internal error

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

**Example:** `{"eventId":"API-908","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`

**Troubleshooting:** An adapter bug: some path returns an error that is not an apierror. Read the error field, then map that failure onto a taxonomy entry at its source.

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
