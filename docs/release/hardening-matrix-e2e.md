# Hardening matrix E2E

Status: PASS

This gate drives a real Server container, a real Agent container, and a real
Docker workload into failures the product claims to survive, then asserts the
claim rather than the absence of a crash. It is deliberately adversarial: every
case injects a fault and every injection is paired with the contract from
[`../architecture.md`](../architecture.md) that the fault is supposed to exercise.

## Inputs and safety boundary

```sh
./scripts/run-hardening-matrix-e2e.sh \
  /absolute/new/evidence-directory \
  sha256:<exact-local-server-image-id> \
  sha256:<exact-local-agent-image-id> \
  sha256:<exact-local-fixture-image-id>
```

Images must already exist locally, must be matching non-development production
targets with the same release version and full source revision, and are
referenced only by exact image ID. Nothing is built, pulled, pushed, or
downloaded; every run uses `--pull never`. The evidence path must be absolute,
must not exist, and is never overwritten; runtime state lives under a separate
fresh `mktemp` root that is scrubbed on every exit.

Select a subset with `HARDENING_CASES`. The default is every case.

## Fixture identity

Every target this matrix touches is the fixture it created, and it proves that
before each request rather than assuming it.

The dashboard lists every Compose project the Agent can see. On a dedicated
test host that is exactly one project - the fixture - so a target taken from
the first entry was right by accident. On a host that is also running the
operator's own projects it is wrong most of the time, and the matrix would
drive its writes, backups, restores, and Compose runs into someone else's
files.

The fixture is therefore resolved by the Compose project name this run
generated **and** the discovery root it created, and the uid that comes back is
checked against the one the Agent must derive for that root:

```text
project uid = sha256(agent_id || NUL || canonical working directory)
```

No match and more than one match are both failures. The first entry is never
assumed. A guard in the single place every request passes through re-proves the
target immediately before the request is sent, for project-scoped URLs and for
operation bodies alike, so a case added later cannot forget it.

`./scripts/verify-fixture-selection.sh` exercises this logic directly against
the functions extracted from the runner: no projects, one project, many
projects, a fixture that sorts last, the same project name at a different root,
a uid the root cannot derive, and the request guard.

## Cases

| Case | Injected fault | Contract asserted |
| --- | --- | --- |
| `agent-sigkill` | `SIGKILL` to an idle Agent | 11.5: the incarnation advances, no clean close is claimed for the killed incarnation, and `AUDIT_CONTINUITY_UNCERTAIN` names the previous incarnation |
| `operation-interrupt` | `SIGKILL` while a health-gated `compose.up` is running | 11.5: the operation comes back `interrupted` and admits `partial_effects_possible` instead of staying nonterminal or reporting clean |
| `server-sigkill` | `SIGKILL` to the Server | 6.4: identity and archive generation survive, the Agent reconnects, and the canonical cursor never regresses |
| `network-partition` | the Agent is disconnected from the control network | the session must actually end, and the Agent must return without consuming a Join Token |
| `compose-interrupt` | cancel a running `compose.down` | 9.6: the operation reaches a terminal state, no Compose child survives, and repeating the cancel stays stable |
| `concurrent-edit` | an out-of-band edit, then a write carrying the stale digest | 10.6: the write is refused, the refusal names the conflict and the current digest, and the file is untouched |
| `concurrent-operations` | two writes racing on one project with the same expected digest | exactly one write succeeds and the file carries exactly one marker, never a blend |
| `db-restore` | the Server database is replaced with an older snapshot | 6.4/6.5: identity and generation are untouched and the acknowledged cursor is not left below its pre-restore watermark |
| `disk-pressure` | the Agent state is moved to an 8 MiB filesystem | 14.3: a degraded-storage reason is reported through capability reason, capabilities stay enabled, and allowed reads keep working |
| `audit-gap` | the WAL loss caused by that pressure | 11.6: every reported gap carries its precision, a design-named source, and an ordered range - never a silent hole |
| `join-token-restart` | a registered Agent restarts with `--join-token-file` still on its command line and the consumed token deleted | the Agent returns ACTIVE with its original identity, and no second host appears |
| `docker-daemon-restart` | the host Engine is restarted | the Agent returns ACTIVE and the Docker capability recovers |

## The closing invariant check

A case that only asserts its own outcome can still leave the product in a state
nobody can describe. Every case therefore ends with the same shared check, and
the case fails if any of it fails:

| Invariant | How it is observed |
| --- | --- |
| Exactly one ACTIVE session; no stale or duplicated one | the fleet dashboard |
| Every Operation this run requested is terminal, with a durable revision | `GET /agents/{id}/operations/{id}` for each tracked ID |
| The project lock is free | a fresh lock-taking Operation that must not come back `PROJECT_BUSY` |
| No restore journal survives a settled scenario | the Agent's `restore-journal` directory |
| No staging file is orphaned | the project directory, for `.dockpilot-*` entries |
| Every Audit coverage entry names its source, precision, and an ordered range | the host Audit page |
| The acknowledged cursor never passes the Server delivery cursor | the same page |
| Docker's containers are present in Dockpilot's view | `docker ps` against `GET /hosts/{id}/containers` |
| The Compose file Dockpilot reads is the file on disk | the file route's digest against `sha256sum` |
| The project secret reached neither an API answer nor a container log | a marked secret in the fixture `.env` |

`db-restore` is the one case that legitimately breaks the second invariant: an
older Server database has no record of the Operations requested after the
snapshot. It asserts separately that the Server refuses to describe such an
Operation with a typed `CONFLICT` or `NOT_FOUND` rather than inventing a status,
and the tracked set restarts there because a discarded history cannot be
re-proved.

`docker-daemon-restart` stops every container on the machine, so it runs only
with `HARDENING_ALLOW_DOCKER_DAEMON_RESTART=1` and a non-interactive
`systemctl restart docker`. Without both it records an explicit skip reason
rather than being silently dropped. It has since been executed, in a disposable
VM rather than on a working machine - see the second recorded execution below.

The case restarts the Agent the way every other restart case in this matrix
does, without the `--join-token-file` the baseline deliberately deleted. It
originally used `docker start`, which re-ran the container's whole argument
list including that flag, and the Agent refused to start against a file the
harness had removed on purpose. That was a harness defect and not a product
one: nothing in the product deletes a consumed token.

## Recorded execution

    started_at              2026-08-19T20:40:51Z
    finished_at             2026-08-19T20:49:20Z
    docker_server_version   29.7.2
    release_version         1.0.0
    release_revision        fd04135f6f063c05dd93810addfa46819ef81b6c
    server_image_id         sha256:eae3d5bf9504c296eb911b3a277d3a8a99856c405167faa768787bd00484b186
    agent_image_id          sha256:78b649789bbbc1e5a4a624c30198abccf6129e86075d7b38625058d137341fbf
    fixture_image_id        sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

Recorded assertion results:

| Assertion | Result |
| --- | --- |
| `agent_sigkill_continuity_uncertain` | PASS (incarnation 1 → 2) |
| `operation_interrupt_terminal_after_kill` | PASS |
| `operation_interrupt_partial_effects_admitted` | PASS |
| `server_sigkill_identity_preserved` | PASS |
| `server_sigkill_cursor_monotonic` | PASS |
| `network_partition_session_ended` | PASS |
| `network_partition_reconnect_without_token` | PASS |
| `compose_interrupt_terminal` | PASS |
| `compose_interrupt_no_orphan_child` | PASS |
| `compose_interrupt_repeated_cancel_stable` | PASS |
| `concurrent_edit_conflict_identified` | PASS |
| `concurrent_edit_file_unmodified` | PASS |
| `concurrent_operations_single_winner` | PASS |
| `concurrent_operations_file_not_blended` | PASS |
| `db_restore_identity_preserved` | PASS |
| `db_restore_ack_watermark_not_regressed` | PASS |
| `db_restore_prior_operation_explained` | PASS |
| `disk_pressure_reason_reported` | PASS |
| `disk_pressure_capabilities_preserved` | PASS |
| `disk_pressure_allowed_read_works` | PASS |
| `audit_gap_every_gap_is_described` | PASS |
| `invariants_agent_sigkill` | PASS |
| `invariants_operation_interrupt` | PASS |
| `invariants_server_sigkill` | PASS |
| `invariants_network_partition` | PASS |
| `invariants_compose_interrupt` | PASS |
| `invariants_concurrent_edit` | PASS |
| `invariants_db_restore` | PASS |
| `invariants_concurrent_operations` | PASS |
| `invariants_degraded_storage` | PASS |
| `docker_daemon_restart` | SKIPPED_NOT_AUTHORIZED (executed separately, below) |

Observed detail worth keeping:

- The 8 MiB filesystem produced a genuine WAL eviction, and the Server recorded
  it as `{"type":"GAP","precision":"exact","source":"AGENT_GAP"}` with an ordered
  range - the `DISK_PRESSURE` path of 11.6 working end to end rather than a
  synthetic flag.
- Every capability reported `DEGRADED_STORAGE: FILESYSTEM_FREE_LOW` as its
  reason while staying enabled, which is exactly what 14.3 asks for.
- The losing racing write failed with
  `SAFE_FILE_CONFLICT: "compose.yaml": expected <a>, current <b>`, so a UI has
  both digests it needs to show a diff.
- The interrupted `compose.up` came back as
  `{"status":"interrupted","partial_effects_possible":true,"error":"agent restarted while operation was nonterminal"}`.
- Every one of the nine closing invariant checks passed, including the one run
  against an Agent in `DEGRADED_STORAGE` on an 8 MiB filesystem, where the lock
  probe is refused for storage rather than for a lock - which is itself proof
  that the lock was free.

`STATUS` recorded `status=PASS`, and the run left no container, network, runtime
root, or Join Token behind.

`join-token-restart` is not in the default selection and is asked for by name.
It has to run before any case that replaces the Agent container, since the flag
still being on the argument list is the whole point; but an extra Agent restart
at the head of the sequence makes `db-restore` more likely to hit an open
product defect - a Server restored behind the Agent's WAL floor leaves a range
neither side can explain, and the Agent never reconnects. That defect is
described in the [campaign record](v1-final-hardening.md) and pinned by
`internal/auditstore/restore_floor_test.go`. Running this case separately keeps
`db-restore` measuring the restore contract rather than the defect's odds.
The case asserts both of its preconditions - the consumed token is gone, and the
container still carries `--join-token-file` - before it does anything, so it
cannot quietly degrade into a plain restart test.

## Second recorded execution: the ten portable cases at the current revision

The matrix was re-run after the fixture-safety, write-transaction, dashboard
heartbeat and client-cancellation changes, on a developer workstation that was
also running unrelated Compose projects - a host the original evidence never
covered.

    started_at              2026-08-20T10:30:10Z
    finished_at             2026-08-20T10:38:34Z
    docker_server_version   29.7.2
    release_revision        c6366b83dc31c712b58ace47fe384bffb15a2a32
    server_image_id         sha256:0c05818885eb56673b95608de83bb2b0ea7401ad8ed23c9018809ad87c4de6ee
    agent_image_id          sha256:0d221f24ed5cb744e9b3b785bdbdf738cb3b950827951b4856e09acb9fda99f2
    selected_cases          the ten cases above; docker-daemon-restart excluded

`STATUS` recorded `status=PASS`. The fixture project was selected by the UID
derived from this run's own Agent id and project root, with one unrelated
Compose project on the host at the time.

## Third recorded execution: `docker-daemon-restart` against a real service manager

Run inside `dp-vm-clean`, a disposable Ubuntu 24.04 guest, so that
`systemctl restart docker` acted on a machine nobody was using.

    started_at              2026-08-20T10:31:58Z
    finished_at             2026-08-20T10:32:10Z
    docker_server_version   29.1.3
    guest                   dp-vm-clean (libvirt)
    release_revision        c6366b83dc31c712b58ace47fe384bffb15a2a32

| Assertion | Result |
| --- | --- |
| `fixture_identity_verified` | PASS |
| `docker_daemon_restart` | PASS |
| `invariants_docker_daemon_restart` | PASS |

The restart was real and the guest's own service manager is the proof:
`systemctl show docker -p ActiveEnterTimestamp` moved to a time between the
baseline dashboard and the post-restart dashboard. The Agent returned ACTIVE
with its original identity and the Docker capability recovered.

Validate the checked-in static contract without Docker:

```sh
./scripts/verify-hardening-matrix-harness.sh
```

## Fourth recorded execution: with `join-token-restart`, after the bootstrap fix

    docker_server_version   29.7.2
    release_revision        eebfe3de574f2f27d1262340f04a4bc46687af23
    server_image_id         sha256:f4425b262c75747768142cf0f609ad85f356836a712df14b3929d0531ff2d294
    agent_image_id          sha256:43a7952dd353dec9733f61e080a771d2e642b1b0e5f0b6021702dbfda5a0c4c4
    selected_cases          join-token-restart, run as its own invocation

`STATUS` recorded `status=PASS`, including:

| Assertion | Result |
| --- | --- |
| `join_token_restart_without_bootstrap_secret` | PASS |
| `join_token_restart_identity_preserved` | PASS |
| `invariants_join_token_restart` | PASS |

The ten default cases were run separately at the same revision and recorded
`status=PASS`.

The case was written against the defect and checked in both directions. Run
alone against the previous images it reproduces the report exactly:

    status=FAIL
    reason=join-token-restart: a registered Agent did not return ACTIVE after
           restarting with a consumed Join Token file

    dockpilot agent failed: configure agent runtime: inspect Join Token file:
    lstat /var/lib/dockpilot/join-token: no such file or directory

Against the images built from the fix it passes, and the Agent's log for the
whole run is empty.
