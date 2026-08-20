# Release evidence

Dockpilot v1 is complete. Every phase of
[`../implementation-plan.md`](../implementation-plan.md) has passed. The
hardening, abuse, and recovery matrices were executed against release images
built from revision `fd04135`; the resource matrix, clean-host installation,
and distribution gates were executed against revision `f1d4087` and are
unaffected by the changes since, which touch the restore transaction, the Audit
WAL tail repair, and the harnesses themselves.

This directory exists because "the tests pass" is not evidence. Each gate here
records what it ran, on which images, on which kernel, and what it deliberately
did *not* measure.

## Gates

| Gate | Status | What it proves |
|---|---|---|
| [Production resource matrix](resource-gate.md) | PASS | Real Server and Agent containers under the Appendix A workload mix, measured through cgroup v2 across three trials. Peak RSS 27–31 MiB against 256/512 MiB budgets, zero OOM events. |
| [Clean-host installation](clean-host-install-e2e.md) | PASS | The documented install procedure on a fresh Linux Docker host, including the host-driven Agent upgrade and identity reconnect. |
| [Recovery matrix](recovery-matrix-e2e.md) | PASS | All three Server-side loss outcomes from architecture section 6.1, with real containers and a real reconnect. |
| [Hardening matrix](hardening-matrix-e2e.md) | PASS | Injected failures the product claims to survive: Agent and Server kills, network partition, interrupted operation, cancelled Compose run, racing writes, rolled-back Server database, and a filesystem too small for the WAL. Every case closes with the same invariant check over locks, operations, journals, staging, Audit coverage, and secrets. |
| [Abuse matrix](abuse-matrix-e2e.md) | PASS | Inputs the product must refuse: path escapes, secret exposure, operation ID rebinding, replayed Join Token, foreign CA, tampered backup archive, non-identical discovery bind, self-directed operations, malformed and oversized requests, and a Compose project name collision. |
| [Reproducible distribution](distribution.md) | PASS | `linux/amd64` and `linux/arm64` release images whose two independent build runs produced byte-identical archives. |
| [Long-running soak](soak.md) | NOT RUN | Accumulation that no injected fault can show: retained memory, threads and descriptors that are never released, Agent state that never settles, an Audit cursor that never catches up. The harness and its verifier are complete; no stage has produced evidence. |

## Re-running the matrices on a working host

The recorded execution in each gate document is the release evidence: revision
`fd04135` / `f1d4087`, on the platform stated there. It is not replaced by
anything below.

The hardening, abuse, and recovery matrices were re-run afterwards on a
developer machine that was also running ten unrelated Compose containers, to
check the fixture-safety and write-transaction changes against a host the
original evidence never covered. Hardening passed all ten selected cases,
abuse passed three consecutive full runs, and recovery passed. Those runs used
images built from a working tree rather than a tagged revision, so they are a
verification result and not release evidence, and no gate document's status or
recorded execution is derived from them.

`docker-daemon-restart` was not among the selected cases: it stops every
container on the machine, and that machine was not disposable.

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
  before a release candidate is signed **have not been run**. What changed is
  that there is now a harness to run them with, and a
  [document](soak.md) that says plainly which stages are outstanding - which is
  all three. A harness is not evidence.

Two known non-blockers are recorded in the gates themselves: the hardening
matrix records `docker-daemon-restart` as `SKIPPED_NOT_AUTHORIZED` unless the
host grants non-interactive `sudo systemctl restart docker`, and the abuse
matrix's `operation_flood_rejected` counter is 0 by the nature of that case —
the real bounds are covered by its `operation-bounds` case instead.

A third is worth stating in the same place: the harnesses now refuse to run
against any project they did not create, and the clean-host gate reports
`SKIPPED_NOT_CLEAN` rather than a product failure on a host that already
manages other Compose projects. Both are consequences of these gates being run
on a working machine for the first time; see
[hardening-matrix-e2e.md](hardening-matrix-e2e.md) for the rule and
[clean-host-install-e2e.md](clean-host-install-e2e.md) for what "clean" means
in that gate.

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
./scripts/verify-fixture-selection.sh
./scripts/verify-soak-harness.sh
```

`verify-fixture-selection.sh` and `verify-soak-harness.sh` do more than read the
runners. The first extracts the fixture-selection functions from the hardening
and abuse runners and exercises them against a host with no projects, one
project, many projects, a fixture that sorts last, the same project name at
another root, and a uid the root cannot derive. The second does the same for the
soak's leak verdict, against a real leak, ordinary noise, a settled warm-up, and
a growing Audit lag. Both fail if the behaviour they describe stops being true.

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
