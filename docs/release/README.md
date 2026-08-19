# Release evidence

Dockpilot v1 is complete. Every phase of
[`../implementation-plan.md`](../implementation-plan.md) has passed, and the
gates below were executed against the release images built from revision
`f1d4087`.

This directory exists because "the tests pass" is not evidence. Each gate here
records what it ran, on which images, on which kernel, and what it deliberately
did *not* measure.

## Gates

| Gate | Status | What it proves |
|---|---|---|
| [Production resource matrix](resource-gate.md) | PASS | Real Server and Agent containers under the Appendix A workload mix, measured through cgroup v2 across three trials. Peak RSS 27–31 MiB against 256/512 MiB budgets, zero OOM events. |
| [Clean-host installation](clean-host-install-e2e.md) | PASS | The documented install procedure on a fresh Linux Docker host, including the host-driven Agent upgrade and identity reconnect. |
| [Recovery matrix](recovery-matrix-e2e.md) | PASS | All three Server-side loss outcomes from architecture section 6.1, with real containers and a real reconnect. |
| [Hardening matrix](hardening-matrix-e2e.md) | PASS | Injected failures the product claims to survive: Agent and Server kills, network partition, interrupted operation, cancelled Compose run, racing writes, rolled-back Server database, and a filesystem too small for the WAL. |
| [Abuse matrix](abuse-matrix-e2e.md) | PASS | Inputs the product must refuse: path escapes, secret exposure, operation ID rebinding, replayed Join Token, foreign CA, tampered backup archive, non-identical discovery bind, self-directed operations, malformed and oversized requests, and a Compose project name collision. |
| [Reproducible distribution](distribution.md) | PASS | `linux/amd64` and `linux/arm64` release images whose two independent build runs produced byte-identical archives. |

Supporting record:

| Document | Subject |
|---|---|
| [Defaults validation](defaults-validation.md) | Which operational defaults have final evidence and which remain provisional, row by row. |

## Reading the gates honestly

Two of these documents say things a marketing README would omit, and that is the
point:

- **[Defaults validation](defaults-validation.md)** promotes exactly one
  ownership row — Agent/Server memory — to `validated`. Every other row still
  names the final evidence it lacks. The resource matrix was defined to produce
  memory evidence, so it promotes memory evidence and nothing else.
- The one-hour combined soak and the overnight soak that the plan requires
  before a release candidate is signed **have not been run**.

Two known non-blockers are recorded in the gates themselves: the hardening
matrix records `docker-daemon-restart` as `SKIPPED_NOT_AUTHORIZED` unless the
host grants non-interactive `sudo systemctl restart docker`, and the abuse
matrix's `operation_flood_rejected` counter is 0 by the nature of that case —
the real bounds are covered by its `operation-bounds` case instead.

## Running the gates yourself

Every harness is fail-closed and has a static verifier that checks its safety
boundary before it is trusted. Run the verifiers first; they need no Docker:

```sh
./scripts/verify-release-scope.sh
./scripts/verify-distribution.sh
./scripts/verify-resource-harness.sh
./scripts/verify-product-resource-workload.sh
./scripts/verify-clean-host-install-harness.sh
./scripts/verify-recovery-matrix-harness.sh
./scripts/verify-hardening-matrix-harness.sh
./scripts/verify-abuse-matrix-harness.sh
```

`verify-release-scope.sh` audits the transitive dependency graph of
`./cmd/dockpilot` — not a directory — for any behaviour architecture section 18
classifies as FUTURE or DO NOT BUILD. Auditing the graph is what keeps the
disposable Appendix A prototype out of scope for the right reason: the release
binary does not link it, and it would come back into scope the moment anything
imported it.

The matrices themselves require a local Linux Docker Engine on cgroup v2 and
refuse to run otherwise. Each gate document states its own inputs and safety
boundary; all of them use exact image IDs with `--pull never`, run in a
throwaway runtime root, and seal their evidence read-only with a `SHA256SUMS`
manifest.

## Transport prototype

The Appendix A transport prototype that selected reverse gRPC over WebSocket is
kept isolated from product code and is not part of the release binary. Its
compact reports are committed to Git and its raw results are a
checksum-addressed bundle; see
`artifacts/transport-prototype/official/README.md`.
