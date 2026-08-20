# v1 Core Interface Freeze

Status: FROZEN at `ac6d84e` on branch `hardening-fixture-safety-and-write-transactions`.

This document freezes the semantics external consumers depend on. It does not
freeze internal implementation: package layout, function names, SQL, and
storage details stay free to change. Where this document and the code disagree,
the code is right and this document is wrong - it was written by reading the
code, and it should be corrected the same way.

The immediate consumer is the Web Console. A console built on this contract
should never need Core semantics changed for its convenience.

## 1. Frozen scope

Frozen: the Agent↔Server domain contract, the Server↔API application contract,
stable identifiers, the operation state machine, machine-readable error codes,
capability semantics, audit coverage and recovery semantics, the file editing
concurrency contract, log and metric stream lifecycle, backup/restore state,
registration and credential lifecycle, and host/project status.

Not frozen: internal function names, package boundaries, SQL schema beyond what
the contract implies, transport implementation, log message wording.

## 2. Stable identifiers

| Identifier | Created by | Persistence | Survives restart | Backup/restore effect | Safe as an external key |
|---|---|---|---|---|---|
| `agent_id` | Agent, at enrollment | Agent state directory | yes | Server DB restore does not change it; losing the Agent state directory means a new Agent | yes |
| `incarnation` | Agent, per process lifetime | Agent state directory | increments each start | unaffected | only with `agent_id`; not stable on its own |
| `project_uid` | derived: `sha256(agent_id ‖ NUL ‖ canonical working dir)` | derived, not stored as authority | yes, while the path and Agent are unchanged | unaffected | yes |
| `operation_id` | caller, supplied on dispatch | Agent journal, Server mirror | yes | a Server DB restore can forget operations the Agent still knows | yes |
| audit cursor | Agent: `(incarnation, seq)` | Agent WAL, Server archive | yes | a Server DB restore moves the Server's view backwards; see §11 | yes, as a position |
| `archive_generation` | Server Identity State | Identity State, separate from the Audit DB | yes | never regresses; a restored DB does not lower it | yes |
| `audit_archive_id` | Server, per archive | Audit DB + Identity State | yes | a new archive gets a new id | yes |
| `server_identity_id` | Server, once | Identity State | yes | losing it is a new trust domain, not a restore | yes |
| `credential_id` | Server, per issued credential | Agent state + Server | yes | revocation lives in Identity State and does not come back with a DB rebuild | yes |

Never used as identity: Compose project name, container name, Compose service
name, display name. They are mutable presentation. Index-based selection
(`projects[0]`, `containers[0]`) is not a way to name anything.

## 3. Host and Agent state

`GET /api/v1/dashboard` and `GET /api/v1/hosts/{agent_id}` return, per host:

- `state`: `ACTIVE` | `OFFLINE` | `CLOSED`
- `capabilities`: see §4
- `project_scan`: last discovery scan, with truncation

There is no separate "Docker unreachable" or "degraded storage" host state.
Those are capability facts, not host states, and are read from
`capabilities.docker.enabled` and the capability `reason`. A console must not
invent a state machine on top; the pair (state, capabilities) is the contract.

## 4. Capabilities

The frozen set is exactly:

`connection`, `docker`, `compose`, `discovery`, `metrics`,
`operation_recovery`, `fs_read`, `fs_write`

`metrics` was added with the host metrics matrix as a new optional field under
§19. It is additive: a console that does not know it is unaffected, and no
existing capability changed meaning.

Each is `{ "enabled": bool, "reason": string }`. `reason` is human-readable and
is **not** frozen as a machine contract; it carries prefixed markers such as
`DEGRADED_STORAGE: …` and `PATH_IDENTITY_MISMATCH` for operators, and callers
must not branch on its text.

How capability falls:

| Condition | Effect |
|---|---|
| Agent self-identification incomplete | mutation fails closed |
| Docker unreachable | `docker.enabled=false`, everything below it false |
| no verified Compose discovery root | `compose.enabled=false` |
| discovery root path identity mismatch (host path ≠ Agent path) | `fs_write=false`, Compose execution refused for that root |
| storage reclaim failed | `compose.enabled=false` |
| degraded storage | capabilities stay enabled, `reason` says so |

A console must not offer an action whose capability is false.

## 5. Operations

Status: `requested`, `dispatched`, `running`, `success`, `failed`, `canceled`,
`interrupted`, `unknown`, `rejected`.

Terminal: `success`, `failed`, `canceled`, `interrupted`, `rejected`.
`unknown` is not terminal - it means the Server cannot currently say.

Phase: `PREPARING`, `EXECUTING`, `COMMITTING`, `FINALIZING`.

Operation view fields: `operation_id`, `status`, `phase`, `revision`,
`partial_effects_possible`, `error`, `output_tail`, `output_truncated`.

`error` is human-readable and is **not** a machine contract. See §13 for the
gap this leaves and what is reserved for it.

Rules:

- The Agent's operation state is authority. The Server is a mirror and a
  recovery view; a Server DB restore can lose operations the Agent still knows,
  and those come back as `CONFLICT` or `NOT_FOUND` rather than as an invented
  status.
- Project-level mutation takes the Agent's exclusive project lock. Reads do not.
- There is no queue for an offline Agent. The request is rejected and the caller
  retries.
- `operation_id` is caller-supplied and the Agent is idempotent on it.
- Browser or transport disconnect does not cancel a mutation.

## 6. Cancellation

Modes: `NONE`, `SAFE`, `BEST_EFFORT_PARTIAL`, `BEFORE_COMMIT`.
Reasons: `USER`, `TIMEOUT`, `AGENT_SHUTDOWN`.
Outcomes: `ACCEPTED`, `TOO_LATE`, `NOT_CANCELABLE`, `ALREADY_TERMINAL`,
`NOT_FOUND`.

**Cancel is not rollback.** A cancelled operation may have partial effects, and
`partial_effects_possible` says whether it might. Cancellation in `COMMITTING`
is refused. An operation the Agent was running when it restarted comes back
`interrupted`, with `partial_effects_possible` true.

## 7. Files

Addressed by `project_uid` + `relative_path`. The Server never sends an absolute
path to an Agent.

- Read returns `content`, `sha256`, `mtime`.
- Write requires `expected_sha256`. A stale digest is `409 CONFLICT` and the
  refusal names the current digest so a console can show a diff.
- Writes are atomic: staged in the same directory, fsynced, renamed, directory
  fsynced. Compose files are validated while staged.
- Limit: 1 MiB per file. UTF-8. Path traversal, absolute paths, NUL, and symlink
  targets are refused.
- **Save is not apply.** Writing a Compose file does not start anything.
- A multi-file save is not a transaction. Only restore has a recovery journal.

## 8. Logs

Live relay. Not storage, not search, not retention.

- A browser disconnect may end the stream; that is the client's decision and is
  answered `499 CLIENT_CLOSED_REQUEST` with nothing recorded as a Server failure.
- A slow consumer gets bounded buffering and exact drop accounting
  (`dropped_bytes`, `dropped_lines` on the event). Drops are reported, never
  silent, and a slow reader must not starve other streams or the control plane.
- Dockpilot does not sanitise application log content. Secrets an application
  prints appear in its logs.

## 9. Metrics

Live, viewer-scoped, ephemeral. No time series is stored server-side. With no
viewer there is no stats stream. A console may keep a short rolling buffer; it
must not present that as history. Scope is container-level (CPU, memory and its
limit, network, block I/O, restarts, health, uptime) and does not extend to host
OS monitoring.

## 10. Audit and coverage

Two kinds: **Managed** (Dockpilot operation history) and **Observed** (Docker
and external change). Docker events are observation, never current-state
authority.

Agent side: append-only bounded WAL, identity `(agent_id, incarnation, seq)`.
Server side: canonical archive plus a coverage ledger.

Coverage entry sources, frozen and non-interchangeable:

| Source | Meaning | Reasons |
|---|---|---|
| `SERVER_COVERAGE_START` | where this archive's coverage begins | `SERVER_NEVER_HAD`, `NEW_AUDIT_ARCHIVE`, `SERVER_DATABASE_REINITIALIZED` |
| `AGENT_GAP` | the Agent lost records of its own | none - an invented Agent reason is rejected |
| `AGENT_CONTINUITY_UNCERTAIN` | the previous incarnation did not close cleanly | none |
| `SERVER_RETENTION` | the Server discarded records on purpose | `SERVER_RETENTION_APPLIED`, `QUOTA_PRESSURE_BEFORE_AGENT_ACK` |
| `SERVER_CURSOR_REGRESSION` | the Server archive went backwards and can no longer obtain a range | `DATABASE_RESTORE`, `ARCHIVE_ROLLBACK`, `CURSOR_METADATA_LOSS`, `UNKNOWN` |

A ledger row's `entry_type` says what the row *is* — `LOWER_BOUND` where this
archive's coverage begins, `GAP` a hole in what was delivered, `REGRESSION` the
archive itself having moved backwards — while `source` and `reason` say why.
`GAP` and `REGRESSION` both count as effective coverage; `LOWER_BOUND` is the
floor beneath them. One source never spans two entry types.

An interface must not present all coverage loss as "the Agent lost logs". Only
`AGENT_GAP` is an Agent claim. The coverage start is immutable once established.

Current effective coverage and historical claims are different questions and are
answered separately.

## 11. SERVER_CURSOR_REGRESSION

**Trigger.** An operator restores a Server database backup. The restored archive
believes it acknowledged far less than it had. The Agent, which was not
restored, resumes from the acknowledgement this Server issued and has now
forgotten. The range between exists nowhere: the Server does not hold it, and
the Agent will not send it, because as far as the Agent is concerned it was
already confirmed.

It is **not** triggered by `CURSOR_BEHIND_FLOOR`. The Agent resumes at
`ServerACKedThrough+1`, which is at or above its own WAL floor, so that message
does not fire here. The signal is the Agent's resume position arriving above the
restored delivery cursor.

**Meaning.** Server-side coverage loss. The Agent lost nothing and its claim
history is untouched.

**Reason.** One of `DATABASE_RESTORE`, `ARCHIVE_ROLLBACK`,
`CURSOR_METADATA_LOSS`, `UNKNOWN`. The automatic recovery path records
`UNKNOWN`: it establishes that the persisted cursor is behind where the Agent
resumed, which a restored database explains and so do other causes it cannot
tell apart. A guess is worse than an admission, and the ledger entry is
permanent. `DATABASE_RESTORE` is reserved for a caller that can establish it.

**Recovery.** The blocked ranges are recomputed by the same function the ACK
check uses, so canonical records and existing coverage are already subtracted.
Only ranges lying entirely below the Agent's resume position are recorded. The
ACK is then retried once. Idempotent: a second attempt finds the ranges
explained and writes nothing.

**Negative guards.** A hole above the resume position is left refused - it may
still arrive. Archive rollback and Server identity mismatch fail before an audit
sync session exists and cannot be normalised this way. The coverage start stays
immutable. Nothing here weakens `AUDIT_ACK_INELIGIBLE` in general.

## 12. Backup and restore

Scope is Compose/env/override configuration. **Not** volume or bind-mount data.

- Automatic pre-write and pre-restore snapshots are kept. Manual backups are
  never auto-deleted.
- Restore is a multi-file transaction with a journal. Crash recovery:
  `PREPARING` → staging cleaned, operation interrupted; partial `COMMITTING` →
  rollback from the pre-restore snapshot; rollback succeeded → interrupted and
  rolled back; **rollback failed → restore recovery required**.
- Restore recovery required blocks every change to that project until an
  operator resolves it. It is reported as `restore_recovery_required` on the
  project, and `read_only` is true as well.
- Restore does not start anything afterwards.

Server DB disaster recovery is a different thing from Compose config restore.
Restoring a Server database can produce `SERVER_CURSOR_REGRESSION` coverage; see
§11 and `docs/operations/recovery.md`.

## 13. Errors

Machine-readable codes are frozen. Human messages are not.

| Code | HTTP | Meaning |
|---|---|---|
| `INVALID_REQUEST` | 400 | the request is malformed or contradicts itself |
| `NOT_FOUND` | 404 | no such resource, or the Server no longer knows it |
| `METHOD_NOT_ALLOWED` | 405 | wrong method for the route |
| `CONFLICT` | 409 | current state refuses it - a stale `expected_sha256`, a discarded operation |
| `TOO_LARGE` | 413 | over a transport limit |
| `CLIENT_CLOSED_REQUEST` | 499 | the caller hung up; nothing is recorded as a Server failure |
| `CAPABILITY_UNAVAILABLE` | 503 | the Agent cannot do it now - offline, Docker unreachable, capability false |
| `SERVER_BUSY` | 503 | Server database contention outlasted its timeout; retryable |
| `STREAM_UNAVAILABLE` | 500 | the HTTP stack cannot stream; a genuine Server-side failure |
| `INTERNAL` | 500 | an invariant failed; carries no detail |

500 is reserved for the Server accusing itself. Contention, offline Agents,
capability gaps, and caller mistakes are never 500.

Agent-side engine codes exist and are stable within the Agent:
`INVALID_SPEC`, `SPEC_MISMATCH`, `ILLEGAL_TRANSITION`, `PROJECT_BUSY`,
`AGENT_SHUTTING_DOWN`.

**Known contract gap.** These engine codes do not currently reach the API; an
operation's failure cause arrives only as free-text `error`. A console can
distinguish *that* an operation failed, its phase, and whether partial effects
are possible, but not *why* without parsing a message. `error_code` is reserved
on the operation view for this, to be added as an optional field in a later
interface revision - it needs a field on the Agent↔Server operation summary,
which is protobuf and could not be regenerated here. This is recorded as a
known non-blocking gap rather than worked around by string matching.

## 14. Credentials and registration

- **Join Token**: bootstrap only. Used for first enrollment and for
  re-registration under the existing contract.
- **Runtime credential**: what a registered Agent reconnects and restarts with.

Startup reads durable state first. A usable runtime credential means Runtime
Mode, and the Join Token source is not consulted - not read, not stat'ed. Only
when the state cannot authenticate the Agent is the bootstrap source evaluated.

`--join-token-file` is therefore not a runtime dependency. After successful
enrollment the file may be deleted or unmounted while the flag stays on the
command line, and the Agent restarts with the same identity and credential.
Dockpilot never deletes an operator's secret file.

There is no Join Token fallback on authentication failure. A revoked credential,
a different Server identity, or corrupt state does not become an automatic
re-enrollment because a token happens to be reachable. Revocation lives in the
Server Identity State and does not come back when the Audit database is rebuilt.

Archive binding: same identity + same generation + same archive is a normal
reconnect; a higher generation with a new archive is a rebind; a lower
generation is `ARCHIVE_ROLLBACK_DETECTED`; the same generation with a different
archive id is an invariant violation; a different Server identity is a trust
failure requiring re-registration. A restored database never lowers the archive
generation - it is minted from the Identity State's own monotonic counter.

## 15. Versioning

`CurrentProductProtocolVersion = 2`, `PreviousProductProtocolVersion = 1`. The
Server accepts N and N-1 on the Agent↔Server product protocol. That is what is
implemented and tested; nothing wider is promised.

The HTTP API is versioned by path (`/api/v1/...`). There is no negotiation
mechanism and none is being added.

## 16. Known non-blocking issues

- **Agent-local diagnostics are incomplete.** The Agent process writes no
  diagnostics of its own during operation; `docker logs` on an Agent container
  is empty across kills, partitions, reconnects and blocked audit sync. Fatal
  startup errors are printed. The Server-side state, capability reasons, and
  error codes remain available, and Audit integrity and recovery semantics are
  unaffected. P2, backlog.
- **Operation failure cause is not machine-readable** at the API. See §13.
  P2, backlog, with `error_code` reserved.

Neither blocks this freeze.

## 17. Explicitly not validated, and out of scope

| Item | Status |
|---|---|
| Long-duration soak (Stage 1, Stage 2, overnight) | **NOT RUN** — excluded from the Interface Freeze gate by project decision. Not a pass, not a failure: not run. |
| NFS / CIFS discovery roots | not validated |
| Rootless Docker | outside v1 support |
| Non-standard Docker socket, `DOCKER_HOST`, socket proxies | outside v1 support |
| `linux/arm64` at runtime | images build reproducibly for it; not exercised on hardware |
| Volume and bind-mount data backup | out of scope for v1 |
| Server-side metric history | out of scope for v1 |
| Arbitrary shell execution | never in scope; only whitelisted operations exist |

## 18. Validation evidence

Run at the frozen revision, on this change:

| Gate | Result |
|---|---|
| `go test ./...` | 788 tests, 54 packages, PASS |
| `go vet ./...` | PASS |
| `-race` on the touched packages | PASS |
| 10 static verifiers | PASS |
| Hardening matrix, 11 cases | PASS |
| Abuse matrix, 13 cases | PASS |
| Recovery matrix, incl. archive rebind and rollback | PASS |

Not re-run, and not needed for this change: the resource matrix - it measures
steady-state resource behaviour, and nothing here touches that path - and the
VM-hosted gates (clean-host install, real daemon restart, multi-Agent lab,
power cut), whose evidence stands at the revisions recorded in their own gate
documents.

See [`release/README.md`](release/README.md) for the full gate set and
[`release/v1-final-hardening.md`](release/v1-final-hardening.md) for the
campaign that produced it, including every finding and its severity.

## 19. Change policy after freeze

Allowed without an interface revision:

- internal refactoring of any kind
- bug fixes that preserve the contract
- new optional fields with backward-compatible semantics, after review

Requires an explicit interface revision:

- changing the meaning of any state, error code, or identifier
- redefining a stable identifier
- changing operation state or cancel semantics
- reusing an audit coverage source or reason for a different cause
- changing the credential lifecycle
- reusing an existing machine-readable error code for a different meaning

Removing a field, narrowing an enum, or changing an HTTP status for an existing
code is a breaking change and needs a new API version.
