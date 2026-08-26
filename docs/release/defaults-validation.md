# v1 operational defaults validation

Status: validation campaign reopened; memory validated, remaining rows tracked below
Source: architecture section 19 and Appendix B.2  
Promotion gate: [v1 implementation plan](v1-implementation-plan.md) Phase 8
(passed 2026-08-18; see `resource-gate.md`)

The values below are product defaults now, so implementation can proceed
without reopening frozen architecture decisions. A default is not called
validated merely because a unit test confirms its numeric value. Values tied
to real I/O, host filesystems, Docker, Compose, or cgroups remain provisional
until the production resource matrix passes.

Binary storage units in product configuration use IEC bytes (`KiB`, `MiB`,
`GiB`). UI text may render user-friendly units but must not change enforcement.

## Fixed policy semantics

These rules are implementation contracts and are not workload-tuning results:

- WAL retention is 256 MiB or 14 days, whichever is reached first.
- WAL durability triggers fsync after 1 second or 64 KiB, whichever comes first.
- Active operations are never evicted from the 24-hour/500-result ring.
- The free-space floor is `max(1 GiB, 5%)`.
- Managed Audit is never rate-limited; Observed Audit is coalesced for 5 seconds
  and capped at 20 events/second with a storm summary.
- Cancellation uses the same operation path as timeout and gives eligible
  process groups 10 seconds between SIGTERM and SIGKILL.
- Offline is declared after 90 seconds with a 30-second heartbeat interval.
- Server Audit pressure warns at 80%, becomes aggressive at 95%, and continues
  ingest while evicting the oldest eligible canonical data.
- Credentials live for 90 days and renewal starts when 50% of lifetime remains.
  An Agent whose credential expires offline must register with a join token.
- Discovery visits at most 1,000 directories/second by default. This provisional
  I/O safety value is part of the scan-budget tuning category and must be
  validated on both local and slow filesystems before release.

`internal/config.V1Defaults` is the executable source for these values and
validates cross-field invariants.

## Validation ownership

| Defaults | Pre-integration evidence | Final evidence |
|---|---|---|
| Exact values and relationships | config unit tests | config dump in release E2E |
| Credential lifetime/renewal | fake-clock issue, activation, expiry, revocation tests | reconnect/offline E2E |
| Operation result/tail limits | deterministic ring/truncation tests | crash/reconnect E2E |
| WAL size/age/fsync | fake-clock and fault-injected WAL tests | production disk-WAL matrix |
| Operation timeouts | fake-clock state-machine tests | real normal/slow/hung Docker and Compose fixtures |
| Retention/disk/reserve | isolated quota fault tests | full storage-pressure matrix |
| Discovery budget/interval | synthetic tree boundary tests | real 200,001-directory and slow-filesystem fixtures |
| Stats interval/ring | fake viewer clock and bounded-ring tests | real Docker viewer 0/1/N soak |
| Agent/Server memory | component allocation tests | production cgroup matrix, three repetitions |

## Integration resource matrix

The runner will execute a production Server plus production Agents with the
real WAL, snapshots/backups, Compose child processes, discovery, and the
Appendix A workload mix. The baseline limits are Agent 512 MiB and Server
1 GiB. If the baseline fails, compare adjacent limits without changing
protocol or queue semantics.

Required observations:

- process RSS, Go heap after GC, goroutines, and file descriptors;
- cgroup `memory.current`, `memory.peak`, `memory.max`, `memory.events.local`,
  `memory.stat`, and `memory.pressure`;
- bounded transport/application queue occupancy;
- P0/P1 latency and Audit cursor progress;
- WAL, backup, restore-journal, and discovery I/O volume.

Each case runs three times. Any OOM/OOM-kill, persistent RSS/heap/anon/buffer
growth, broken P0/P1 guarantee, unexplained Audit range, or contract violation
fails. File cache and `memory.events.local.max` are diagnostic and do not fail a
case by themselves.

## Focused fault matrices

Storage tests use an isolated quota or disposable filesystem; they never fill
the developer host filesystem. They verify the architecture's eviction order,
manual-backup protection, one newest automatic snapshot, 64 MiB emergency
reserve allowlist, admission rejection, degraded reason, and recovery
hysteresis.

Timeout tests give every real operation normal, slow, and hung fixtures. A
timeout must travel through the normal cancellation state machine. Normal p99
must retain operational headroom beneath the configured deadline.

Discovery tests include a 200,001-directory fixture and a deliberately slow
filesystem. Reaching either 200,000 directories or 60 seconds first must return
a partial result with `truncated=true` and the last scanned path.

The long soak runs for at least one hour and combines reconnects, process
crashes, stream churn, disk pressure, and active logs/stats. An overnight soak
is required before the v1 release candidate is signed.

## Promotion record

Phase 8 passed on 2026-08-18. It promotes the one row whose final evidence it
was defined to produce - Agent/Server memory - and nothing else. The other
rows in "Validation ownership" name final evidence this matrix does not
generate, so they stay provisional and are listed below with what each still
needs. A blanket `Status: validated` would claim evidence that does not exist.

**Environment and identity**

```text
source_revision   f1d4087eb94921f07ce3c6fafddcbf0261314bf3
server_image      sha256:44202ec0ffeddec84b6dba8711b8f2cc353e69f9f876e9c104afb6fe47887125
agent_image       sha256:22492be1c6a6ad695521ac704ae550711cfceb57e8e6d1883eee9bed939b0e04
fixture_image     sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4
kernel            Linux 6.18.33.2-microsoft-standard-WSL2 x86_64
docker_engine     29.7.2
cgroup            v2, systemd driver
compose           5.3.1, pinned in the Agent image
started/completed 2026-08-19T11:46:15Z / 2026-08-19T12:00:22Z
```

Artifact tree carries a 215-entry `SHA256SUMS` manifest and per-trial
`workload-evidence.sha256`. The matrix command is the one in
`resource-gate.md` with `RESOURCE_CASE_SECONDS=600`.

**Per-case result**: trial 1, 2, 3 all `status=PASS`;
`prototype_acceptance_reused=false`. Three-run resource summaries, the
Appendix A A.9 bounds actually measured, and the items deliberately not
measured are recorded in `resource-gate.md`.

**Changed defaults**: none. The baseline limits held with large headroom -
peak Agent RSS 27.4-30.5 MiB against 256 MiB and peak Server RSS 30.7-32.0 MiB
against 512 MiB - so no architecture-authorized tuning value was adjusted and
no adjacent-limit comparison was needed.

**Promotion status by ownership row**

| Defaults | Status | Outstanding final evidence |
|---|---|---|
| Agent/Server memory | validated | none; production cgroup matrix, three repetitions |
| Retention/disk/reserve | partial | Phase 6 quota fault matrix passed on a disposable size-limited tmpfs; full storage-pressure matrix outstanding |
| Exact values and relationships | validated | the exact release Server Image emitted `dockpilot defaults` byte-for-byte equal to `distribution/v1-defaults.json` in the current-revision clean-host E2E |
| Credential lifetime/renewal | provisional | reconnect/offline E2E |
| Operation result/tail limits | partial | real 525-operation/500-entry eviction passed; output-tail truncation remains unit-only and needs real process output plus reconnect evidence |
| WAL size/age/fsync | provisional | production disk-WAL matrix |
| Operation timeouts | partial | real health-gated Compose cancellation passed; configured normal/slow/hung timeout boundaries remain unmeasured |
| Discovery budget/interval | policy decision required | 200,000 directories is unreachable under the simultaneous 60-second and 1,000 directories/second defaults; interval and slow-filesystem evidence also remain |
| Stats interval/ring | partial | the resource matrix measured real Docker collection with 0, 1, and 6 viewers and the current-revision Stage 1 soak passed; the 120-sample browser bound remains |

The one-hour soak required before the v1 release candidate is signed has now
passed ([`soak.md`](soak.md), Stage 1); the overnight soak has not been run.
No row above changes as a result. The soak owns direction over time, not the
values in this table, so a passing soak promotes nothing here on its own. The
`Stats interval/ring` row retains its separate browser-ring evidence
requirement even though the resource matrix supplies its real Docker viewer
0/1/N observation and the current-revision Stage 1 soak has passed.

## Reopened validation campaign — 2026-08-25

The previous table stayed provisional even after later gates supplied part of
its requested evidence. This campaign separates what is already demonstrated
from what still has no final observation; no row is promoted by inference.

The release binary now exposes a read-only `dockpilot defaults` command. Its
complete JSON output is pinned by `distribution/v1-defaults.json`, checked by a
unit test, and required by the clean-host harness from inside the exact Server
Image. The latest 2026-08-25 clean-host run at revision
`299ac64d30c9ce3761f2690c7c51ae468502d947` recorded
`defaults_config_dump=PASS`, so the exact-values row is validated.

The operation result count is no longer merely a deterministic unit claim. The
abuse matrix submitted 525 real operations against the 500-entry Agent result
ring and verified that the oldest result returned 404 while the newest reached
a terminal state. The 64 KiB output tail still lacks equivalent real-process
and reconnect evidence, so the combined row is partial rather than validated.

The resource matrix already measured the viewer-scoped collection contract:
zero viewers produced zero subscriptions and frames, one Matrix viewer produced
frames, and six simultaneous per-Container stats viewers remained bounded. The
browser's 120-sample ring is pinned in code tests but has not been observed over
a complete real sampling window, so this row is also partial.

The discovery row contains a policy contradiction that a larger fixture cannot
resolve. At 1,000 directories/second, a 60-second scan can visit at most about
60,000 directories before `max_duration` wins; `max_directories=200000` cannot
become the stopping reason while all three defaults are active. A product
decision must either lower the directory cap to a reachable value, raise the
duration budget, or explicitly document the directory count as a secondary
defence that is not independently reachable under v1 defaults.
