# Real-container recovery matrix E2E

Status: PASS

This is the Phase 9 fail-closed recovery gate. It proves, across a real Server
container, a real Agent container, and a real reconnect, the three Server-side
loss outcomes that [`architecture.md`](architecture.md) section 6.1
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

**Case 2 — Identity State lost, Audit database preserved.** The Server would
present a different `server_identity_id` over an Archive belonging to the old
one, so it must fail closed. The assertion requires a non-zero exit and output
naming the Archive identity refusal; a clean exit fails the gate.

**Case 3 — both Server stores lost.** With nothing left to contradict, the Server
creates a new trust domain. The untouched Agent must not be accepted, no ACTIVE
host may be reported, and only manual re-registration with a fresh Agent state
and a new Join Token restores service.

## Recorded execution

    started_at              2026-08-19T12:16:18Z
    finished_at             2026-08-19T12:21:45Z
    docker_server_version   29.7.2
    release_version         1.0.0
    release_revision        f1d4087eb94921f07ce3c6fafddcbf0261314bf3
    server_image_id         sha256:44202ec0ffeddec84b6dba8711b8f2cc353e69f9f876e9c104afb6fe47887125
    agent_image_id          sha256:22492be1c6a6ad695521ac704ae550711cfceb57e8e6d1883eee9bed939b0e04

Observed identity and archive facts:

| Fact | Value |
| --- | --- |
| baseline `server_identity_id` | `4d3919a2b24c8dd52970c4de402edc81` |
| baseline `archive_generation` | 1 |
| case 1 `server_identity_id` | `4d3919a2b24c8dd52970c4de402edc81` (unchanged) |
| case 1 `archive_generation` | 2 (advanced) |
| case 3 `server_identity_id` | `b454a9f94aa149ba5879de6cd5684a17` (new trust domain) |

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

Validate the checked-in static contract without Docker:

```sh
./scripts/verify-recovery-matrix-harness.sh
```
