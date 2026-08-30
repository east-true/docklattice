# Concepts

This is the mental model behind DockLattice, in English, for someone who has not
read the architecture record. It is a summary written to be *correct*, not
complete: [`architecture.md`](architecture.md) remains the authority, and each
section below names the architecture section it summarizes.

## The one-sentence model

> **The Agent is the authority on execution. The Server is the authority on the
> record.**

Everything else follows from that split (architecture section 2).

## Where truth lives

DockLattice deliberately refuses to become a second source of truth for things
Docker already knows. Each row below names the *only* component allowed to
answer that question:

| Question | Authority |
|---|---|
| What containers, images, networks, volumes exist right now | Docker Engine |
| What a Compose file says | The host filesystem |
| Whether an operation may run, and who holds the project lock | The DockLattice Agent |
| Audit records not yet synchronized | The Agent's bounded disk WAL |
| Synchronized audit records and their coverage | The Server's canonical archive |
| Whether an Agent is trusted at all | Server Identity State |

Consequences that are not negotiable: Docker runtime state is never replicated
into the Server database, Compose file contents are never stored in the Server
database, metrics are a live relay with no history, and logs are a live relay
with no central log store.

## Server and Agent

The **Server** is one process holding registration, operation history, and the
canonical Audit archive. It serves the browser UI and API over HTTPS.

The **Agent** is one container per Docker host. It talks to the host's Docker
socket and executes everything.

The connection is **Agent-initiated and outbound-only** — the Agent opens no
inbound port (architecture section 5.1). That is what makes hosts behind NAT,
behind a firewall, or on changing IP addresses work without any of the usual
inbound plumbing, and it shrinks the attack surface to one direction.

The transport is a single reverse gRPC connection, chosen by measurement rather
than preference: the transport prototype ran both a gRPC and a WebSocket
candidate through the same acceptance matrix, and the WebSocket candidate failed
the scale workload group. The raw results are kept under
`artifacts/transport-prototype/official`.

### Traffic classes

A single connection carrying cancellations, audit sync, queries, logs, and stats
has an obvious failure mode: a slow log stream starves a cancel. HTTP/2 stream
priority is not available to applications, so DockLattice owns scheduling itself
(architecture section 5.2):

| Class | Carries |
|---|---|
| P0 Control | cancel, heartbeat, operation phase and final result, protocol errors |
| P1 Durable sync | audit WAL sync, operation result recovery |
| P2 Interactive query | Docker and Compose queries, file reads |
| P3 Bulk interactive | logs, live Compose stdout/stderr relay |
| P4 Disposable live | stats |

P0 and P1 must never starve. Logs get a per-stream byte-rate cap and bounded
buffering, and explicitly report dropped bytes rather than lying. Stats are
latest-wins — an old sample is discarded, never queued.

Operation output is the deliberate exception to backpressure. If the UI consumed
Compose's stdout slowly and that pressure reached the pipe, the Compose process
itself would stall — so operation output is *always* drained, kept as a bounded
64 KiB tail, and marked `truncated` when the middle was dropped.

## Identity and trust

Three separate things must all be true before an Agent is trusted
(architecture section 6):

1. **Server Identity State** — the Server's own durable identity. Losing it is
   not the same as losing the database; see
   [identity recovery](operations/recovery.md).
2. **A Join Token** — short-lived, one-time, issued out of band by an operator
   who already has shell access to the Server state directory.
3. **An Agent credential** — signed at registration, valid 90 days, renewed
   automatically once half the lifetime remains. An Agent whose credential
   expired while offline must be re-enrolled with a token bound to its existing
   Agent ID.

A fourth layer governs *records* rather than *connections*: the Audit archive
identity, which pairs the Server identity with an archive generation. It is what
lets an Agent tell "I reconnected to my Server" apart from "I am now talking to a
Server that has lost its archive", and it is why an Agent that reconnects to a
rebuilt archive emits an explicit `ARCHIVE_REBOUND` record instead of silently
continuing (architecture sections 6.4 and 6.5).

## Discovery and project identity

The Agent scans its discovery roots for Compose projects and joins that
filesystem view to what Docker actually reports.

A **discovery root must be an identical absolute-path bind mount** —
`/srv/stacks:/srv/stacks`, never a remapped path (architecture sections 3.1 and
3.2). The reason is that the Agent hands paths to the Docker daemon, which
resolves them on the *host*; if the container's view of a path differs from the
host's, every bind mount DockLattice creates silently points somewhere else. The
Agent verifies this itself at start-up (the Path Identity Self-Check) and demotes
any root that fails to read-only with an explicit capability reason, rather than
trusting a path it cannot prove.

Projects are identified by content, not by name:

```
project_uid = hash(agent_id + canonical_working_dir)
```

The project *name* is deliberately not part of the identity, because the name is
one line in a `.env` file — putting it in the identity would make projects
vanish and reappear in the UI every time someone edited that line. A project
whose working directory changes is genuinely a different project, to Compose as
well as to DockLattice.

Compose projects Docker reports but that live outside any discovery root are
shown as **unmanaged projects**: viewable, not editable. This is a first-class
state, not an error, because it is extremely common in practice.

Two things DockLattice watches for, both of which are real operational accidents
rather than theoretical ones:

- **Project name collision** (architecture section 7.6). Two directories on one
  host using the same Compose project name means `compose up` in one will adopt,
  recreate, and delete the *other's* containers. Detection costs one SQL
  statement; DockLattice flags both projects and blocks mutating operations.
- **External configuration change** (architecture section 7.7). Discovery
  re-hashes the key files of each managed project every five minutes and records
  a change made outside DockLattice as observed audit. This is polling, not
  `fsnotify`, because one inotify watch per file per project hits
  `max_user_watches` on any real host, and the goal is after-the-fact
  observation rather than realtime reaction. Detection never triggers automatic
  re-application.

## Operations

Everything that changes state is an **Operation** with a Server-generated ID.
The types are:

```
container.start   container.stop    container.restart   container.remove
compose.pull      compose.up        compose.down
compose.start     compose.stop      compose.restart
compose.file.write   env.write      override.write
backup.create     backup.restore    discovery.rescan
```

### Lifecycle

```
requested → dispatched → running → success
                             ├──→ failed
                             ├──→ canceled      (explicit cancel, or timeout)
                             └──→ interrupted   (Agent restarted mid-flight)
      └─────────────────────────→ unknown       (connection lost after dispatch)
      └─────────────────────────→ rejected      (agent offline, lock contention,
                                                 or missing capability)
```

`canceled` and `interrupted` both mean partial effects are possible, and both
say so — but they record different causes, because "someone stopped this" and
"this died" call for different operator responses.

### Project lock

One exclusive lock per project, held by the Agent (architecture section 8.4).
Held by every mutating operation; held by no read. Contention waits at most two
seconds and then returns `409 PROJECT_BUSY` — DockLattice does not queue, because a
queued mutation is a mutation whose ordering nobody chose.

There is deliberately **no force-release API**. The lock's holder is the Agent
that is actually running the process; an API that could take it away would be an
API for corrupting a half-finished `compose up`.

### Idempotency and timeouts

The Agent keeps recent operation results in a bounded ring and returns the stored
result when the same operation ID arrives twice, rather than re-executing. This
is not a nicety — it is the precondition for recovering an operation's outcome
after a reconnect.

Timeouts are per type and route through the *same* cancellation path as a user
cancel. There is no second termination mechanism, and no forced kill after the
commit point that would break data consistency.

### Cancellation is not rollback

Cancelling stops further work. It does not undo what already happened, and
DockLattice never claims otherwise — an operation that was cancelled after partial
application says exactly that (architecture section 9).

Two disconnections that are explicitly *not* cancellations: the browser closing,
and the transport dropping. Neither one means the operator wanted the `compose
up` on that host to stop.

## Safe files

File editing is restricted by whitelist, not by heuristic (architecture section
10.1). The API contract carries only `(project_uid, relative_path)` — an absolute
path is refused at the boundary — and the editable set is:

```
compose.yaml | compose.yml | docker-compose.yaml | docker-compose.yml
compose.override.* | docker-compose.override.* | compose.*.yaml
.env | .env.*
```

within the project's canonical working directory. Writes are atomic, validated,
and preceded by an automatic snapshot.

**One write is one file.** Multi-file transactions exist for exactly one
operation — configuration restore — and nowhere else (architecture section 10.7).

**Concurrent edit detection** (architecture section 10.6) uses the same hashes
discovery already computes: a write carries the hash the editor started from, and
a write whose base hash no longer matches is refused rather than silently
overwriting whatever changed underneath it.

## Audit

Two kinds, kept distinct because they have genuinely different reliability
(architecture section 11.1):

**Managed audit** is what happened *through* DockLattice. It is generated from the
operation lifecycle itself — one completed operation is one audit record — rather
than from a parallel code path that could drift. It is never rate-limited and
never sampled. The actor is recorded only when it is knowable: `ui:<client_ip>`
or `webhook:<provider>`, and nothing invented.

**Observed audit** is what happened *outside* DockLattice, using Docker events as a
signal. Events are a whitelist (container create/start/die/stop/kill/destroy,
health status, rename; image pull/delete/tag; volume and network create/destroy)
and are suppressed under load: identical events within five seconds coalesce with
a count, and above twenty per second DockLattice emits one "event storm" summary
instead. A Docker event is treated as a *signal that state changed*, never as the
current state — meaningful transitions trigger exactly one inspect, and `start`
does not, because re-inspecting on every event turns an event storm into daemon
load.

If Docker cannot say who did something, the actor is `unknown`. DockLattice does
not guess, and does not try to become a host-level audit system.

### Durability

Audit records are written to a bounded WAL on the Agent's disk, fsynced after one
second or 64 KiB, and synchronized to the Server's canonical archive by cursor.
The Server acknowledges a watermark; the Agent frees WAL space up to it.

Two states describe a hole in the record, and the distinction matters
(architecture section 11.6):

- `AUDIT_GAP` — records are known to be missing, and the range is known.
- `AUDIT_CONTINUITY_UNCERTAIN` — continuity across a boundary cannot be proven.

DockLattice reports which one it is instead of collapsing both into a warning.

**The Server never refuses Agent audit ingest because of its own capacity
pressure.** Under pressure it evicts the oldest eligible canonical data and keeps
ingesting, because dropping the newest record to protect the oldest is backwards.

## Backups

A configuration backup is a `files.tar.gz` plus a manifest of per-file SHA-256,
mode, and size, stored on the **Agent** at mode 0700/0600 — it contains `.env`
(architecture section 13.1).

The bytes stay on the Agent on purpose: the Agent is what performs the restore,
the files are already there, and there is no reason to round-trip `.env` secrets
across the network. The Server indexes only metadata and fetches content on
demand.

Automatic snapshots are taken immediately before every file write and immediately
before every restore, retained at twenty per project. The Server decides
retention; the Agent executes the deletions it is told to perform.

Under disk pressure, **manual backups are never automatically deleted** — an
operator who deliberately took a backup gets to keep it.

## What DockLattice will not do

Every behaviour is classified in architecture section 18 as CORE, OPTIONAL,
FUTURE, or DO NOT BUILD. The DO NOT BUILD list for v1 includes Kubernetes and
Swarm orchestration, user accounts and roles, mTLS and certificate rotation, a
central log store, metric history, and any reimplementation of what Docker or
Compose already provides. These are decisions, not gaps.
