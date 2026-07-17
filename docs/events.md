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
