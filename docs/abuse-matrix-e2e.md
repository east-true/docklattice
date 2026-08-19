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

## Cases

| Case | What is sent | Contract asserted |
| --- | --- | --- |
| `path-abuse` | eight escape attempts per direction: `..`, absolute paths, a trailing slash, an unmanaged file, `.git/config` | 10.7: every read and every write is refused with a client status, and nothing is created outside the project root |
| `secret-exposure` | a `.env` value written through the API | the value is masked in both the file read and the environment listing without `reveal`, and appears nowhere in the Server database or the Audit record |
| `operation-id-reuse` | one operation ID rebound to a different kind, target, and project | each rebinding is refused with 409 and the original record is untouched |
| `operation-flood` | a burst of 40 operations | every request gets a decision, the Agent stays ACTIVE, and the Server keeps answering |
| `self-protection` | `container.stop`, `container.restart`, and `container.remove` aimed at the Agent's own container ID | each is refused and the Agent is still running and ACTIVE afterwards |
| `request-abuse` | malformed JSON, an unknown field, a wrong method, `limit=0`, a malformed cursor, an unknown query parameter, and a 2 MB body | each is refused with a client status, none with a server error, and the Server is healthy afterwards |
| `name-collision` | a second project directory claiming the same Compose project name | 7.6: both projects are marked as colliding and a mutation on one is refused with 409 |

## Recorded execution

    started_at              2026-08-19T13:44:58Z
    finished_at             2026-08-19T13:45:32Z
    docker_server_version   29.7.2
    release_version         1.0.0
    release_revision        3ac48ce
    server_image_id         sha256:30e59dfde2baa93b38423a56bef8132f153a4b00049e02f70cb6c4118578f3f4
    agent_image_id          sha256:5037d3ede732e6d972d5599e1e2089a5d8c16b499bd26301c57afad700ee4a4e
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
| `name_collision_detected` | PASS |
| `name_collision_mutation_refused` | PASS |

Observed detail worth keeping:

- Every self-directed container operation failed with
  `DOCKER_MUTATION_DENIED: DENY_PROTECTED_CONTAINER`, which is the policy
  decision rather than an incidental Docker error.
- The oversized body was refused with 413 rather than being buffered.
- `operation_flood_rejected` is 0: forty `discovery.rescan` operations complete
  faster than the bounded active index fills, so this run exercised
  responsiveness and decision coverage, not the bound itself. The bound has
  package-level coverage in `internal/operation`.

This gate found one defect, now fixed: the dashboard, single-host, and project
environment routes silently ignored unrecognised query parameters while their
sibling routes refused them. The environment route was the consequential one,
because a caller passing `reveal=1` or misspelling the key received masked
values while believing it had asked for revealed ones.

Validate the checked-in static contract without Docker:

```sh
./scripts/verify-abuse-matrix-harness.sh
```
