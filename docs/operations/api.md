# HTTP API

The Server exposes one HTTPS API under `/api/v1`, used by the embedded web UI
and available directly. This is the product's only untrusted surface, and it is
written that way: unknown query parameters, repeated query parameters, bodies on
GET requests, and oversized payloads are all refused rather than ignored.

**v1 has no authentication.** Anyone who can reach this API controls every
connected Docker host. See [`../../SECURITY.md`](../../SECURITY.md) before
exposing the port.

## Conventions

Read endpoints return `200` with a JSON body. Mutations are asynchronous: they
return `202 Accepted` with an operation handle, and the caller polls the
operation for its outcome.

Errors are a uniform JSON object:

```json
{ "code": "PROJECT_BUSY", "message": "project \"...\" is locked by operation \"...\"" }
```

| Status | `code` | Meaning |
|---|---|---|
| 400 | `INVALID_REQUEST` | Malformed, unknown parameter, repeated parameter, or body where none is allowed. |
| 404 | `NOT_FOUND` | No such host, project, backup, or route. |
| 405 | `METHOD_NOT_ALLOWED` | Route exists, method does not. |
| 409 | `CONFLICT` | Project lock contention, concurrent edit, name collision, or a refused mutation. |
| 413 | `TOO_LARGE` | Body exceeds the endpoint's limit. |
| 503 | `CAPABILITY_UNAVAILABLE` | Agent offline, or the capability is disabled for this host or root. |
| 503 | `SERVER_BUSY` | The Server database could not take its write lock within the busy timeout. Transient contention, safe to retry. |
| 500 | `INTERNAL` | Server-side invariant failure. The response body carries no detail by design; the Server logs it. |

Refusals carry a specific reason where one exists — `PROJECT_BUSY`,
`PATH_IDENTITY_MISMATCH`, `DENY_PROTECTED_CONTAINER`, `DENY_PROTECTED_PROJECT`,
`SAFE_FILE_CONFLICT` — rather than a generic conflict.

## Fleet and hosts

| Method | Path | Returns |
|---|---|---|
| `GET` | `/api/v1/dashboard` | Fleet overview: hosts, their state, and project summaries. |
| `GET` | `/api/v1/hosts/{agent_id}` | One host's detail and capabilities. |
| `GET` | `/api/v1/hosts/{agent_id}/containers` | Containers, as Docker reports them. |
| `GET` | `/api/v1/hosts/{agent_id}/images` | Images. |
| `GET` | `/api/v1/hosts/{agent_id}/networks` | Networks. |
| `GET` | `/api/v1/hosts/{agent_id}/volumes` | Volumes. |
| `GET` | `/api/v1/hosts/{agent_id}/audit` | Audit page for the host. |

`audit` accepts `limit` (1–500) and `cursor`. Nothing else — an unrecognized
parameter is a `400`, so a typo in a filter name can never silently return
unfiltered data.

The inventory routes accept no query parameters at all. They are a pass-through
of Docker's own answer, not a Dockpilot cache.

## Projects

| Method | Path | Returns |
|---|---|---|
| `GET` | `/api/v1/projects/{project_uid}/environment` | `.env` entries. |
| `GET` | `/api/v1/projects/{project_uid}/activity` | Audit page scoped to the project. Accepts `limit` and `cursor`. |
| `GET` | `/api/v1/projects/{project_uid}/compose/ps` | Compose service state. Accepts repeated `service` and one `all`. |
| `GET` | `/api/v1/projects/{project_uid}/compose/config` | Resolved Compose configuration. Accepts repeated `service`. |
| `GET` | `/api/v1/projects/{project_uid}/compose/logs` | Compose logs as Server-Sent Events. |

`project_uid` is content-derived (`hash(agent_id + canonical_working_dir)`), not
a name — see [concepts](../concepts.md#discovery-and-project-identity).

### Secrets

Values the Agent classifies as secret are returned masked as `********` unless
the request passes `reveal=true`. The masking happens on the Server at
serialization time, so an omitted parameter can never leak — the default is
always the safe one.

Secret values are never written to the Server's storage; they live on the Agent
and are fetched on demand.

## Files

| Method | Path | Body |
|---|---|---|
| `GET` | `/api/v1/projects/{project_uid}/files?path=<relative>` | — |
| `PUT` | `/api/v1/projects/{project_uid}/files` | `{ "operation_id", "relative_path", "expected_sha256", "content" }` |

`GET` accepts `path` (required) and `reveal` (optional). `path` is always
relative — an absolute path is refused at the API boundary, and the editable set
is a whitelist of Compose and `.env` files inside the project's canonical
working directory.

`PUT` is an **operation**, not a synchronous write: it returns `202` with an
operation handle. It requires `operation_id`, and `expected_sha256` is the
concurrent-edit guard — a write whose base hash no longer matches the file on
disk is refused with a conflict rather than overwriting the change someone else
made. Each request writes exactly one file.

## Backups

| Method | Path | Body |
|---|---|---|
| `GET` | `/api/v1/projects/{project_uid}/backups` | — |
| `POST` | `/api/v1/projects/{project_uid}/backups` | `{ "operation_id", "relative_paths": [...] }` |
| `POST` | `/api/v1/projects/{project_uid}/backups/{backup_id}/restore` | `{ "operation_id" }` |

Both mutations return `202`. Restore verifies every file's SHA-256 against the
archive manifest and refuses the whole restore on any mismatch — a tampered or
truncated archive fails closed and leaves the live project untouched.

Restore is the *only* multi-file transaction in Dockpilot.

## Operations

| Method | Path | Body |
|---|---|---|
| `POST` | `/api/v1/operations` | `{ "operation_id", "agent_id", "kind", "project_uid"?, "target"? }` |

`operation_id`, `agent_id`, and `kind` are required. Returns `202` with the
operation's status, phase, revision, and `partial_effects_possible`.

Valid `kind` values:

```
container.start   container.stop    container.restart   container.remove
compose.pull      compose.up        compose.down
compose.start     compose.stop      compose.restart
compose.file.write   env.write      override.write
backup.create     backup.restore    discovery.rescan
```

The ID is caller-supplied and is the idempotency key. Re-sending a completed
operation's ID returns the stored result instead of re-executing — this is how
an outcome survives a reconnect. Re-using an ID for a *different* operation is
refused, so an ID cannot be rebound to new work.

Terminal statuses are `success`, `failed`, `canceled`, `interrupted`, and
`rejected`. A poller that only waits for the first three will hang forever on a
`PROJECT_BUSY` refusal, which arrives as `rejected`.

## Live streams

| Method | Path | Stream |
|---|---|---|
| `GET` | `/api/v1/live/logs` | Container logs, as SSE. |
| `GET` | `/api/v1/live/stats` | Container stats, as SSE. |

Both require `agent_id` and `container_id`. Logs additionally accept `follow`,
`tail`, `stdout`, `stderr`, and `timestamps`; at least one of `stdout` or
`stderr` must be enabled.

Neither stream is resumable. A request carrying `Last-Event-ID` is refused with
a `400` rather than silently restarting from the beginning and presenting the
result as a continuation. There is no log history and no metric history to
resume from — both are live relays by design.

Logs are rate-capped per stream and buffered with a bound; when the bound is hit
the stream reports the dropped byte count explicitly. Stats are latest-wins: a
slow consumer sees fresh samples, never a queue of stale ones.
