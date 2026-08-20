# Abuse matrix E2E

Status: PASS

The browser-facing HTTP API is the product's one untrusted surface, and a
project directory is written by people outside Dockpilot. This gate sends the
things the product must refuse and asserts the refusal - not merely that nothing
crashed. It complements [`hardening-matrix-e2e.md`](hardening-matrix-e2e.md),
which breaks the running system rather than feeding it bad input.

## Inputs and safety boundary

```sh
./scripts/run-abuse-matrix-e2e.sh \
  /absolute/new/evidence-directory \
  sha256:<exact-local-server-image-id> \
  sha256:<exact-local-agent-image-id> \
  sha256:<exact-local-fixture-image-id>
```

Same boundary as the other container gates: exact local image IDs only, nothing
built or pulled, an absolute evidence directory that must not already exist, and
a separate `mktemp` runtime root scrubbed on every exit. Select a subset with
`ABUSE_CASES`.

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

Two cases in this matrix create a second project of their own - the name
collision case and the protected Compose project case - and both are registered
as fixtures the same way. The protected-project case previously searched the
dashboard for any readable project to use as its "unrelated" control and ran a
real `compose.up` against it; it now uses this run's own baseline fixture.

## Cases

| Case | What is sent | Contract asserted |
| --- | --- | --- |
| `path-abuse` | eight escape attempts per direction: `..`, absolute paths, a trailing slash, an unmanaged file, `.git/config` | 10.7: every read and every write is refused with a client status, and nothing is created outside the project root |
| `secret-exposure` | a `.env` value written through the API | the value is masked in both the file read and the environment listing without `reveal`, and appears nowhere in the Server database or the Audit record |
| `operation-id-reuse` | one operation ID rebound to a different kind, target, and project | each rebinding is refused with 409 and the original record is untouched |
| `operation-flood` | a burst of 40 operations | every request gets a decision, the Agent stays ACTIVE, and the Server keeps answering |
| `self-protection` | `container.stop`, `container.restart`, and `container.remove` aimed at the Agent's own container ID | each is refused and the Agent is still running and ACTIVE afterwards |
| `request-abuse` | malformed JSON, an unknown field, a wrong method, `limit=0`, a malformed cursor, an unknown query parameter, and a 2 MB body | each is refused with a client status, none with a server error, and the Server is healthy afterwards |
| `token-single-use` | one Join Token presented by a second Agent after the first consumed it | the replay is refused and the registered host count does not change |
| `wrong-server-ca` | an Agent handed a CA that did not sign this Server | it never reaches registration and no host appears |
| `backup-tamper` | bytes flipped inside a stored backup archive, then a restore | the restore is refused on the entry digest and the live project file is untouched |
| `non-identical-bind` | a discovery root whose container path differs from its host path | 3.1/3.2: filesystem write capability is disabled with a reason and a write is refused |
| `operation-bounds` | a second mutation while a health-gated `compose.up` holds the project, then 525 operations against a 500-entry result ring | the contender is refused with `PROJECT_BUSY` and the holder still completes; the evicted oldest record answers 404 rather than being served from the Server cache |
| `name-collision` | a second project directory claiming the same Compose project name | 7.6: both projects are marked as colliding and a mutation on one is refused with 409 |
| `protected-compose-project` | a `compose.down` aimed at the Compose project the Agent itself belongs to | the mutation is refused with `DENY_PROTECTED_PROJECT`, the Agent survives, and an unrelated project still works |

## Recorded execution

    started_at              2026-08-19T20:53:03Z
    finished_at             2026-08-19T20:59:04Z
    docker_server_version   29.7.2
    release_version         1.0.0
    release_revision        fd04135f6f063c05dd93810addfa46819ef81b6c
    server_image_id         sha256:eae3d5bf9504c296eb911b3a277d3a8a99856c405167faa768787bd00484b186
    agent_image_id          sha256:78b649789bbbc1e5a4a624c30198abccf6129e86075d7b38625058d137341fbf
    fixture_image_id        sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

Recorded assertion results:

| Assertion | Result |
| --- | --- |
| `path_abuse_all_refused` | PASS (16 attempts, all 400) |
| `secret_exposure_masked_without_reveal` | PASS |
| `secret_exposure_absent_from_server_storage` | PASS |
| `operation_id_reuse_refused` | PASS (kind, target, and project rebinding all 409) |
| `operation_id_reuse_original_intact` | PASS |
| `operation_flood_server_responsive` | PASS (40 accepted, 0 rejected) |
| `self_protection_refused` | PASS |
| `self_protection_agent_survives` | PASS |
| `request_abuse_all_refused_with_client_status` | PASS |
| `request_abuse_server_healthy` | PASS |
| `token_single_use_replay_refused` | PASS |
| `token_single_use_no_extra_host` | PASS |
| `wrong_server_ca_never_registers` | PASS |
| `backup_tamper_restore_refused` | PASS |
| `backup_tamper_project_untouched` | PASS |
| `non_identical_bind_fs_write_disabled` | PASS |
| `non_identical_bind_write_refused` | PASS |
| `operation_bounds_project_busy` | PASS |
| `operation_bounds_ring_evicts_oldest` | PASS (525 requested, oldest answered 404) |
| `operation_bounds_newest_still_readable` | PASS |
| `name_collision_detected` | PASS |
| `name_collision_mutation_refused` | PASS |
| `protected_compose_project_denied` | PASS |
| `protected_compose_project_unrelated_still_allowed` | PASS |
| `protected_compose_project_agent_survives` | PASS |

Observed detail worth keeping:

- Every self-directed container operation failed with
  `DOCKER_MUTATION_DENIED: DENY_PROTECTED_CONTAINER`, which is the policy
  decision rather than an incidental Docker error.
- The oversized body was refused with 413 rather than being buffered.
- `operation_flood_rejected` is 0: forty `discovery.rescan` operations complete
  faster than any bound is reached, so that case exercises responsiveness and
  decision coverage. The bounds themselves are `operation-bounds`, which holds
  the project lock open with a health-gated `compose.up` and overruns the result
  ring on purpose.
- The refused contender reported
  `PROJECT_BUSY: project "<uid>" is locked by operation "<holder>"`, so the
  project lock is demonstrably what refused it, and the holder still finished
  `success` afterwards.
- The replayed Join Token failed at registration with
  `agentruntime: register: agent credential request rejected: HTTP 401`, and the
  Agent exited 1 rather than retrying a dead secret forever.
- The Agent given a foreign CA exited 1 with
  `tls: failed to verify certificate: x509: certificate signed by unknown
  authority`, so it refused the Server before presenting its Join Token.
- The modified backup archive was refused with
  `invalid backup archive: digest mismatch for "compose.yaml"`, and the live
  file's digest was identical before and after.
- The non-identical bind produced `fs_write` disabled with reason
  `PATH_IDENTITY_MISMATCH` and `compose` disabled with `no verified Compose
  discovery root`, and a write answered 409.
- The `compose.down` aimed at the Agent's own Compose project failed with
  `agentruntime: Compose denied: DENY_PROTECTED_PROJECT: target Compose project
  contains a protected Agent`, and a `compose.up` on an unrelated project
  succeeded in the same run - the denial is aimed, not a blanket refusal of
  Compose mutations. The Agent carries the Compose labels a Compose deployment
  would give it, which is what `IdentifySelf` reads to build its protection set.

This gate found one defect, now fixed: the dashboard, single-host, and project
environment routes silently ignored unrecognised query parameters while their
sibling routes refused them. The environment route was the consequential one,
because a caller passing `reveal=1` or misspelling the key received masked
values while believing it had asked for revealed ones.

## Second recorded execution: at the current revision

Re-run after the fixture-safety, write-transaction, dashboard heartbeat and
client-cancellation changes, on a developer workstation that was also running
unrelated Compose projects.

    started_at              2026-08-20T10:39:38Z
    docker_server_version   29.7.2
    release_version         1.0.0
    release_revision        c6366b83dc31c712b58ace47fe384bffb15a2a32
    server_image_id         sha256:0c05818885eb56673b95608de83bb2b0ea7401ad8ed23c9018809ad87c4de6ee
    agent_image_id          sha256:0d221f24ed5cb744e9b3b785bdbdf738cb3b950827951b4856e09acb9fda99f2

All thirteen cases ran and `STATUS` recorded `status=PASS`. Two of them -
`protected-compose-project` and `name-collision` - are the ones that used to
pick their target by list position; on this host that would have aimed a real
`compose.up` at somebody else's project. Both now resolve their target from the
UID derived from this run's own Agent id and project root.

Validate the checked-in static contract without Docker:

```sh
./scripts/verify-abuse-matrix-harness.sh
```
