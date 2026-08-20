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
| [Resource matrix](resource-gate.md) re-run, 3 trials | workstation | PASS |

## Findings

Severity is about consequence to an operator, not about how hard it was to
find.

| # | Sev | Finding | State |
|---|---|---|---|
| 1 | P1 | The dashboard performed an unbounded heartbeat per host. One partitioned Agent — packets dropped, connection not reset — made `GET /api/v1/dashboard` return nothing for *every* Agent for more than twenty seconds. The Server's own liveness loop was already bounded; the dashboard's was not. | Fixed. Pinned by `internal/serverapi/dashboard_isolation_test.go`; validated by the lab's `partition-one`. |
| 2 | P1 | A deferred SQLite transaction that upgrades to a write is refused with `SQLITE_BUSY_SNAPSHOT` without the busy handler being invoked, so a concurrent write could fail immediately rather than wait. | Fixed. `Store.BeginWrite` takes an immediate write lock from a dedicated pool; `ErrBusy` answers 503 `SERVER_BUSY` instead of 500. |
| 3 | P2 | A client hanging up — a closed tab, an SSE stream the reader walked away from, a `curl` that hit `--max-time` — reached the internal-error arm: HTTP 500 plus a line in the Server's failure diagnostics. Every live stream ends this way, so a diagnostics log meant for finding real failures filled with teardown noise. | Fixed. Client cancellation answers 499 and records nothing. |
| 4 | P2 | The Agent process writes no diagnostics at all. Only the Server has a diagnostics writer, and an Agent's `docker logs` is empty across kills, restarts, partitions and an archive-rollback refusal — an unrecoverable condition with no local signal. An operator debugging a stuck Agent has the dashboard and nothing else. | **Open.** The rollback refusal is observable through the audit stream, which is how the recovery matrix asserts it, but that requires a working Server. |
| 5 | P1 | `--join-token-file` was read while the process was being assembled, before any durable state was loaded and therefore before anything knew whether an enrollment was needed. A container's argument list does not change between restarts, so an operator who followed the install guide's own instruction to remove the consumed token found the Agent refusing to start, and `restart: unless-stopped` could not recover it. On a fleet that is every Agent whose bootstrap secret was rotated or unmounted. | Fixed. The token is resolved on the enrollment path only; a registered Agent never opens the file. Pinned by eight tests in `internal/agentruntime/bootstrap_token_test.go` and the hardening matrix's `join-token-restart` case. |

| 6 | P1 | A restored Server database left a range that neither side could supply, so every ACK was refused, every session ended, and the Agent reconnected into the same refusal indefinitely — permanently OFFLINE with no automatic recovery. | Fixed. `SERVER_CURSOR_REGRESSION` coverage is now recorded for exactly the unobtainable range. See below. |

Fixes 1 and 2 predate this record; 3 was found by the multi-Agent lab's closing
invariant and fixed here.

Finding 5 was first recorded as P3 on the reasoning that an operator can drop
the flag. That was wrong twice over. The install guide tells operators to
remove the token after registration, so the product's own documented procedure
walked into it; and the recovery is a container recreation on every affected
host, which is exactly the fleet-wide manual intervention a restart policy
exists to avoid. An already-registered Agent that cannot restart is P1.

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
- **The resource gate's bounded-buffer assertion raced the consumer it was
  measuring.** A 1 KiB/s SSE reader has to reach a drop-carrying event in an
  in-order stream before the driver moves on; three of five observed trials did
  not, and failed. The driver now waits for that evidence, bounded by the case
  deadline.

### `db-restore`: an Agent that could never reconnect — fixed

**Severity: P1. Resolved.**

An operator restores a Server database backup. The restored archive believes it
acknowledged far less than it had. The Agent, which was not restored, resumes
from the acknowledgement this Server issued and has now forgotten. The range
between the two exists nowhere: the Server does not hold it, and the Agent will
not send it, because as far as the Agent is concerned it was already confirmed.

`CheckAndAdvanceACK` refused every ACK for that range, the session ended, and
the Agent reconnected into the same refusal indefinitely. The host stayed
OFFLINE and nothing recovered it.

**Why it only failed sometimes.** The hardening matrix took its snapshot
immediately after the baseline, before the Agent's first Audit sync had
established coverage. Restoring such a snapshot gives the Server a database with
no coverage row at all, and it then establishes coverage fresh at wherever the
Agent resumes — a real path, and the harmless one. Only when the snapshot
happened to contain an established coverage row did the restore strand a range.
That is what made the case look like it was interacting with
`join-token-restart`: the extra restart shifted timing, nothing more. The case
now waits for coverage to be established and acknowledged before snapshotting,
and asserts afterwards that a range was actually stranded, so it exercises the
same path every run.

**The fix.** The architecture had already reserved the answer — the
`SERVER_CURSOR_REGRESSION` source, its four reasons, the ledger entry type and
the API rendering all existed; only the producer was missing.
`auditstore.RecordCursorRegression` writes it, and `auditsync` calls it when an
ACK is refused, then retries once.

Three things keep it from becoming a general way past ACK eligibility:

- The blocked ranges are not supplied by the caller. They are recomputed by the
  same function the ACK check uses, which already subtracts canonical records
  and existing effective coverage — so a range the Server actually holds, or one
  another ledger entry already explains, cannot be covered by this.
- Only ranges lying entirely below the point the Agent resumed at are recorded.
  The Agent streams strictly forward from a start it derives from this Server's
  own acknowledgement, so anything below that will not arrive. A hole *above* it
  may still be filled, and stays refused.
- The reason comes from the reserved set. A caller that cannot tell why the
  archive moved backwards says `UNKNOWN` rather than assuming a restore.

**Coverage semantics.** `AGENT_GAP` is an Agent saying it lost records of its
own. `SERVER_CURSOR_REGRESSION` is the Server saying this archive can no longer
obtain a range because the archive itself went backwards. The Agent lost
nothing, and its claim history is not touched. The coverage start stays
immutable: it records where this archive began, and a restore does not change
that retroactively.

**Correcting the earlier entry.** This record previously said the failure was an
interaction with `join-token-restart`, then that the extra restart changed the
odds. Both were descriptions of the symptom. The cause is the unrecoverable
range between a restored Server's cursor and the Agent's resume position, and it
has nothing to do with either case.

**Archive generation.** Verified separately and needing no change: the archive
generation is minted from the Server Identity State's monotonic counter, not
from the restored database, so restoring an old archive row always yields a
generation ahead of everything already issued. The two-advance recovery seen
earlier came from restoring the Identity State *and* the database together,
which is a different case and is covered by recovery matrix case 1b.

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
The resource matrix has since been re-run at this revision: three trials,
`status=PASS`, peak RSS in the low tens of MiB against 256/512 MiB budgets, no
OOM. That run also resolved a question this record previously listed as open -
whether the slow-consumer `dropped_bytes` accounting worked at all. It does; the
gate's own assertion was racing the consumer's position in an in-order byte
stream, failing three of five observed trials and passing two with exact counts
of 10,773 and 80,514 bytes. The driver now holds the stream open until the
evidence exists. See [resource-gate.md](resource-gate.md).

Until the soak exists, the honest answer is NOT_READY — for want of evidence,
not because anything found here is unresolved.
