# Hardening matrix E2E

Status: PASS

This gate drives a real Server container, a real Agent container, and a real
Docker workload into failures the product claims to survive, then asserts the
claim rather than the absence of a crash. It is deliberately adversarial: every
case injects a fault and every injection is paired with the contract from
[`architecture.md`](architecture.md) that the fault is supposed to exercise.

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
| `docker-daemon-restart` | the host Engine is restarted | the Agent returns ACTIVE and the Docker capability recovers |

`docker-daemon-restart` stops every container on the machine, so it runs only
with `HARDENING_ALLOW_DOCKER_DAEMON_RESTART=1` and a non-interactive
`systemctl restart docker`. Without both it records an explicit skip reason
rather than being silently dropped.

## Recorded execution

    started_at              2026-08-19T13:23:00Z
    finished_at             2026-08-19T13:30:22Z
    docker_server_version   29.7.2
    release_version         1.0.0
    release_revision        f1d4087eb94921f07ce3c6fafddcbf0261314bf3
    server_image_id         sha256:ead4628e28352ab53ccc73dc1b5d90545398234d9d989b30206ebbeb74805e57
    agent_image_id          sha256:e45a78076c2af409579c8072b89b7d7255d12bcf263e0b4ffddea358c12954af
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
| `disk_pressure_reason_reported` | PASS |
| `disk_pressure_capabilities_preserved` | PASS |
| `disk_pressure_allowed_read_works` | PASS |
| `audit_gap_every_gap_is_described` | PASS |
| `docker_daemon_restart` | SKIPPED_NOT_AUTHORIZED |

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

`STATUS` recorded `status=PASS`, and the run left no container, network, runtime
root, or Join Token behind.

Validate the checked-in static contract without Docker:

```sh
./scripts/verify-hardening-matrix-harness.sh
```
