# Real-container recovery matrix E2E

Status: PASS

This is the Phase 9 fail-closed recovery gate. It proves, across a real Server
container, a real Agent container, and a real reconnect, the three Server-side
loss outcomes that [`../architecture.md`](../architecture.md) section 6.1
distinguishes. The package-level matrix in
`internal/serverbootstrap/recovery_matrix_test.go` covers the bootstrap decision
itself; this gate covers what an operator actually observes.

## Inputs and safety boundary

```sh
./scripts/run-recovery-matrix-e2e.sh \
  /absolute/new/evidence-directory \
  sha256:<exact-local-server-image-id> \
  sha256:<exact-local-agent-image-id>
```

Both images must already exist in the local Engine, must be matching non-development
production targets with the same release version and full source revision, and are
referenced only by exact image ID. Nothing is built, pulled, pushed, or downloaded;
every run uses `--pull never`. `DOCKER_HOST`, remote daemons, and socket proxies are
outside this gate.

The evidence path must be absolute, must not exist, and is never overwritten.
Runtime state lives under a separate fresh absolute `mktemp` root; Server and
Agent state use UID/GID 65532 and mode 0700, and the generated one-day test TLS
key and certificate use mode 0600. Join Tokens are emitted by the production
Server CLI into the runtime root, never copied into evidence, and deleted as
soon as registration completes. The evidence tree is capped, checksummed, and
made read-only when the run ends.

## Cases

**Control — Server restart that loses nothing.** Section 6.4 calls the same
identity, same generation, same archive_id a normal reconnect. This case runs
first on purpose: without it, a reconnect defect is indistinguishable from a
recovery defect, and both loss cases below would fail for the wrong reason.

**Case 1 — Audit database lost, Identity State preserved.** The database and its
WAL/SHM sidecars are removed while the Server is stopped; the Identity State is
left untouched and the assertion checks both facts. After restart the Server must
keep its `server_identity_id`, advance `archive_generation`, and the Agent —
never restarted, holding no Join Token — must return to ACTIVE under its original
Agent ID. That can only happen through automatic credential authentication plus
an automatic Archive Rebind.

**Case 1b — the rebind is recorded, and a rollback is refused.** Two things
follow from section 6 that no dashboard state shows. An Agent that adopts a new
Archive must leave a trace, or the gap it just crossed becomes unattributable
afterwards; and an Agent presented with a generation *below* the one it holds
must refuse, because adopting it would write newer history into an older archive
under the same identity. The first is asserted from the Agent's own audit
stream. The second is produced the way an operator produces it: the Server's
Identity State and database are snapshotted together while it is stopped, the
Agent is moved past that generation, and then the consistent old backup is
restored.

The refusal is observed where it has an effect rather than in a log line - the
Agent process writes no diagnostics at all, only the Server has a diagnostics
writer - so what is checked is that the restored archive's cursor does not move
however much the Agent goes on observing.

Recovering afterwards takes two generation advances rather than one. The Agent
holds generation N; losing the restored database once mints generation N again
with a *new* archive id, which the Agent refuses just as firmly as it refused
the rollback. Only a generation strictly past the one it holds is adoptable.
That is an operational fact about restoring a Server backup, not a harness
detail.

**Case 2 — Identity State lost, Audit database preserved.** The Server would
present a different `server_identity_id` over an Archive belonging to the old
one, so it must fail closed. The assertion requires a non-zero exit and output
naming the Archive identity refusal; a clean exit fails the gate.

**Case 3 — both Server stores lost.** With nothing left to contradict, the Server
creates a new trust domain. The untouched Agent must not be accepted, no ACTIVE
host may be reported, and only manual re-registration with a fresh Agent state
and a new Join Token restores service.

## Recorded execution

    started_at              2026-08-19T21:00:20Z
    finished_at             2026-08-19T21:05:47Z
    docker_server_version   29.7.2
    release_version         1.0.0
    release_revision        fd04135f6f063c05dd93810addfa46819ef81b6c
    server_image_id         sha256:eae3d5bf9504c296eb911b3a277d3a8a99856c405167faa768787bd00484b186
    agent_image_id          sha256:78b649789bbbc1e5a4a624c30198abccf6129e86075d7b38625058d137341fbf

Observed identity and archive facts:

| Fact | Value |
| --- | --- |
| baseline `server_identity_id` | `010f5a9cc4a10e1b62340d8922ddac76` |
| baseline `archive_generation` | 1 |
| case 1 `server_identity_id` | `010f5a9cc4a10e1b62340d8922ddac76` (unchanged) |
| case 1 `archive_generation` | 2 (advanced) |
| case 3 `server_identity_id` | `bd2a9e9def75605da2434146dddfb0ff` (new trust domain) |

Recorded assertion results:

| Assertion | Result |
| --- | --- |
| `plain_restart_reconnect` | PASS |
| `database_loss_identity_preserved` | PASS |
| `database_loss_generation_advanced` | PASS |
| `database_loss_automatic_reconnect` | PASS |
| `identity_loss_with_database_fails_closed` | PASS |
| `both_stores_lost_new_trust_domain` | PASS |
| `both_stores_lost_old_agent_rejected` | PASS |
| `both_stores_lost_manual_reregistration` | PASS |

`STATUS` recorded `status=PASS`, and the run left no container, network, runtime
root, or Join Token behind.

## Second recorded execution: with case 1b, at the current revision

    started_at              2026-08-20T10:43:22Z
    finished_at             2026-08-20T10:55:43Z
    docker_server_version   29.7.2
    release_revision        c6366b83dc31c712b58ace47fe384bffb15a2a32
    server_image_id         sha256:0c05818885eb56673b95608de83bb2b0ea7401ad8ed23c9018809ad87c4de6ee
    agent_image_id          sha256:0d221f24ed5cb744e9b3b785bdbdf738cb3b950827951b4856e09acb9fda99f2

| Fact | Value |
| --- | --- |
| baseline `archive_generation` | 1 |
| case 1 `archive_generation` | 2 (advanced, identity unchanged) |
| generation the Agent was moved to | 3 |
| generation presented after the rollback | 2 |
| restored archive head before the settle window | cursor 63 |
| restored archive head after it | cursor 63 — nothing delivered |
| last rebind cursor at generation 3 | 92 |
| generation advances needed to recover | 2 (to generation 4) |

| Assertion | Result |
| --- | --- |
| `archive_rebind_recorded` | PASS |
| `archive_rollback_not_adopted` | PASS |
| `archive_rollback_no_downward_rebind` | PASS |
| every case 1, 2 and 3 assertion above | PASS |

`STATUS` recorded `status=PASS`.

Validate the checked-in static contract without Docker:

```sh
./scripts/verify-recovery-matrix-harness.sh
```
