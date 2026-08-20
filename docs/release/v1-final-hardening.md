# v1 final hardening campaign

Verdict: **NOT_READY for interface freeze** — no open defect blocks it; two
pieces of evidence are missing.

This records a campaign that ran the existing gates against hosts they had
never been run on, added three gates for failures no existing gate could
produce, and fixed what that turned up. It is a record, not a gate: each gate's
own document holds its evidence.

## What was run, and where

All at revision `c6366b83dc31c712b58ace47fe384bffb15a2a32` unless noted.

| Gate | Where | Result |
|---|---|---|
| Unit and package tests | workstation | PASS — 765 tests, 54 packages |
| Static harness verifiers (8) | workstation | PASS |
| [Hardening matrix](hardening-matrix-e2e.md), 10 portable cases | workstation with unrelated Compose projects running | PASS |
| [Hardening matrix](hardening-matrix-e2e.md), `docker-daemon-restart` | `dp-vm-clean` guest | PASS |
| [Abuse matrix](abuse-matrix-e2e.md), 13 cases | workstation | PASS |
| [Recovery matrix](recovery-matrix-e2e.md), incl. new case 1b | workstation | PASS |
| [Clean-host installation](clean-host-install-e2e.md) | `dp-vm-clean` guest, never ran Dockpilot | PASS |
| [Multi-Agent lab](multi-agent-lab.md), 11 cases, 3 Agents | `dp-vm-lab` guest | PASS |
| [Power cut](power-cut.md) | `dp-vm-clean` guest, `virsh destroy` | PASS |
| [Soak](soak.md) stage 1 re-run | workstation | INCOMPLETE — stopped at 17.6 of 60 minutes |
| [Resource matrix](resource-gate.md) re-run | — | not run at this revision |

## Findings

Severity is about consequence to an operator, not about how hard it was to
find.

| # | Sev | Finding | State |
|---|---|---|---|
| 1 | P1 | The dashboard performed an unbounded heartbeat per host. One partitioned Agent — packets dropped, connection not reset — made `GET /api/v1/dashboard` return nothing for *every* Agent for more than twenty seconds. The Server's own liveness loop was already bounded; the dashboard's was not. | Fixed. Pinned by `internal/serverapi/dashboard_isolation_test.go`; validated by the lab's `partition-one`. |
| 2 | P1 | A deferred SQLite transaction that upgrades to a write is refused with `SQLITE_BUSY_SNAPSHOT` without the busy handler being invoked, so a concurrent write could fail immediately rather than wait. | Fixed. `Store.BeginWrite` takes an immediate write lock from a dedicated pool; `ErrBusy` answers 503 `SERVER_BUSY` instead of 500. |
| 3 | P2 | A client hanging up — a closed tab, an SSE stream the reader walked away from, a `curl` that hit `--max-time` — reached the internal-error arm: HTTP 500 plus a line in the Server's failure diagnostics. Every live stream ends this way, so a diagnostics log meant for finding real failures filled with teardown noise. | Fixed. Client cancellation answers 499 and records nothing. |
| 4 | P2 | The Agent process writes no diagnostics at all. Only the Server has a diagnostics writer, and an Agent's `docker logs` is empty across kills, restarts, partitions and an archive-rollback refusal — an unrecoverable condition with no local signal. An operator debugging a stuck Agent has the dashboard and nothing else. | **Open.** The rollback refusal is observable through the audit stream, which is how the recovery matrix asserts it, but that requires a working Server. |
| 5 | P3 | `--join-token-file` is read unconditionally at startup. A registered Agent does not need a token — the runtime says so — but the flag is resolved before the runtime is consulted, so an operator who removes the consumed token and runs under a restart policy finds the Agent refusing to start. | **Open.** Workaround: drop the flag after registration. A narrow fix would treat `ENOENT` as "no token" while keeping every other error fail-closed. |

Fixes 1 and 2 predate this record; 3 was found by the multi-Agent lab's closing
invariant and fixed here.

### Harness defects found by running the harnesses

None of these are product behaviour, and all are fixed. They are listed because
each one meant a gate had been reporting a result it had not established.

- **Fixture selection by list position.** The hardening, abuse and clean-host
  harnesses picked their target with `.projects[0]`. On a workstation running
  other people's Compose projects that aims real writes, backups and
  `compose.up` runs at somebody else's files. Every target is now resolved from
  `sha256(agent_id || NUL || canonical working dir)` computed by the harness,
  and a target that cannot be proved is a failure.
- **`docker-daemon-restart` restarted the Agent with `docker start`,** re-running
  the container's whole argument list including the `--join-token-file` the
  baseline had deliberately deleted.
- **The lab's run prefix carried an uppercase timestamp** while Compose
  lowercases project names, so every `com.docker.compose.project` filter matched
  nothing.
- **`catchup-fairness` recorded `catchup_all_cursors_advanced=PASS`** over three
  agents reading 23→23, 30→30 and 42→42, and one of those agents had no running
  fixture and therefore no backlog at all.
- **The lab ran privileged Docker-in-Docker on the workstation,** which cost the
  operator remote access twice. It now refuses outside a `dp-vm-*` guest.

## What is missing

1. **Stage 1 soak at this revision.** Architecture section 30 requires a soak
   after a transport or session change, and finding 1 is one. The recorded
   one-hour soak in [`soak.md`](soak.md) predates that fix. A re-run reached
   17.6 of 60 minutes before being stopped; over 35 samples nothing moved in a
   direction that would suggest accumulation — Server RSS 27.2→29.2 MiB (peak
   31.2), Agent RSS 25.2→26.3 MiB (peak 28.0), Server descriptors 15→16, Agent
   descriptors 11→10, threads at most 12 each, Audit lag constant at 1, zero
   gaps, zero OOM events, zero HTTP errors, host ACTIVE in every sample — but
   17.6 minutes is not a soak, and this is not evidence of anything.
2. **Stage 2 soak** (2–4 hours, mixed mode) has never run.
3. **Resource matrix at this revision.** The recorded PASS is from `f1d4087`.
   A separate open question sits inside it: with a deliberately slow SSE
   consumer the harness never observed `dropped_bytes`, although the field is
   mapped end to end (`internal/producttransport/live_bridge.go` →
   `internal/serverapi/backend.go` → `internal/webui/types.go`) and
   `internal/logrelay` accounts for every drop it makes. Whether that is the
   relay never dropping, or a drop notification queued behind the same stream it
   describes, is not established.

Until 1 and 3 exist, the honest answer is NOT_READY — for want of evidence, not
because anything found here is unresolved.
