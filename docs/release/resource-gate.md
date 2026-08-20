# Production resource gate

Status: PASS

This is the implementation-plan Phase 8 product gate. It runs the production
Server and Agent images; it does not reuse Appendix A prototype acceptance,
synthetic candidate adapters, or prototype artifacts as product evidence.
A three-trial matrix has been executed against the production images and all
three trials returned `status=PASS`; see "Recorded execution" below.

## Runner

Run the static verifier first:

```sh
./scripts/verify-resource-harness.sh
```

The matrix requires a local Linux Docker Engine using cgroup v2, working
Docker/buildx permission, the exact v1 Docker socket, OpenSSL, curl, jq, and an
already-present fixture image. Remote daemons and Docker Desktop fail
preflight because their container cgroups cannot be measured through the host
`/proc` and `/sys/fs/cgroup` used by the runner.

```sh
export RESOURCE_FIXTURE_IMAGE='<immutable local fixture image reference>'
export RESOURCE_CASE_SECONDS=600
./scripts/run-resource-matrix.sh \
  /absolute/path/to/new-resource-artifacts \
  '<server image reference>' \
  '<agent image reference>' \
  "$(pwd)/scripts/run-product-resource-workload.sh"
```

The reviewed production driver is `scripts/run-product-resource-workload.sh`;
validate its fail-closed contract with
`scripts/verify-product-resource-workload.sh`. It uses only already-present
images and the local HTTPS/Docker endpoints. It never builds, pulls, pushes, or
downloads an image. Docker-driven observed Audit is intentionally variable, so
the driver proves cursor generation/durability/ACK progress rather than writing
a synthetic production Audit stream.

The artifact directory must not exist. The default aggregate cap is 512 MiB
and can be adjusted with `RESOURCE_ARTIFACT_MAX_BYTES`, up to 2 GiB. Preflight
also requires the destination filesystem to retain at least the cap plus
1 GiB free. Runtime state and secrets are scrubbed after each trial; evidence,
sizes, image IDs, and checksums remain. Docker's bounded local log driver keeps
diagnostic SIGQUIT and GC output from growing without limit.

The runner always performs exactly three trials with Agent `memory.max` at
512 MiB and Server `memory.max` at 1 GiB. It records:

- `memory.current`, `memory.peak`, `memory.max`, `memory.events.local`,
  `memory.stat`, and `memory.pressure` start/end snapshots;
- timestamped cgroup values, process RSS, and FD counts during the workload;
- Go post-GC heap evidence from `GODEBUG=gctrace=1`;
- a bounded end-of-trial SIGQUIT goroutine-header count;
- container inspection, image identity, runtime-state sizes, workload output,
  per-trial verdicts, and a SHA-256 manifest.

The diagnostic SIGQUIT intentionally terminates each process only after the
workload and final cgroup sample. OOM state is read from Docker inspection and
cgroup counters. An increase in `memory.events.local.max` is retained as a
diagnostic and is not by itself a failure; any `oom` or `oom_kill` increase is
a failure.

## Workload-driver contract

The fourth argument is deliberately required. An idle Server and Agent must
never be mistaken for Phase 8 evidence. The harness supplies these variables:

```text
DOCKPILOT_BASE_URL
DOCKPILOT_CA_FILE
DOCKPILOT_DASHBOARD_FILE
DOCKPILOT_PROJECT_ROOT
DOCKPILOT_COMPOSE_PROJECT
DOCKPILOT_SERVER_CONTAINER
DOCKPILOT_AGENT_CONTAINER
RESOURCE_CASE_SECONDS
RESOURCE_VERDICT_FILE
```

The driver receives the trial number and evidence directory as two positional
arguments. It must use the production HTTPS API and connected production
Agent, keep all its output within the evidence directory, finish within the
case bound, and write every line below exactly once to `RESOURCE_VERDICT_FILE`:

```text
PRODUCT_SERVER_AGENT=1
REAL_COMPOSE_CHILD=1
REAL_WAL_FSYNC=1
BACKUP_SNAPSHOT_IO=1
DISCOVERY_SCAN=1
APPENDIX_A_MIX=1
P0_P1_PASS=1
AUDIT_CONTINUITY_PASS=1
BOUNDED_BUFFERS_PASS=1
RESOURCE_TREND_PASS=1
```

Those assertions must be backed by raw driver evidence: real Compose child
processes, real disk WAL/fsync, backup and snapshot writes, bounded discovery,
the Appendix A traffic/churn mix, P0/P1 latency, Audit cursor progress, queue
occupancy, and trend evaluation. The shell runner checks the assertions and
resource boundary; it does not infer missing product telemetry from prototype
metrics.

The driver must also leave these non-empty raw files in its evidence directory:

```text
compose-child-processes.tsv
p0-p1-latency.jsonl
audit-cursor-progress.jsonl
bounded-buffers.jsonl
io-evidence.tsv
resource-trend.json
```

The three JSONL files must parse as one or more JSON values,
`resource-trend.json` must contain `{"pass":true}`, and each TSV must contain a
header plus at least one observation. Evidence must not contain Join Tokens,
private keys, file contents, or other secrets.

## Recorded execution

The workload driver is implemented, statically verified, and executed. The
matrix ran three trials against the production images and every trial returned
`status=PASS`, so `defaults-validation.md` is no longer provisional.

```text
started_at             2026-08-19T11:46:15Z
completed_at           2026-08-19T12:00:22Z
source_revision        f1d4087eb94921f07ce3c6fafddcbf0261314bf3
kernel                 Linux 6.18.33.2-microsoft-standard-WSL2 x86_64
docker_server_version  29.7.2
cgroup                 v2, systemd driver
repetitions            3
case_seconds           600
sample_seconds         2
prototype_acceptance_reused  false
```

Peak process RSS stayed far inside both limits, and cgroup peak never reached
the 80% warning threshold in any trial:

| trial | role   | peak RSS | budget   | peak anon | cgroup peak | FD | `events.local.max` |
| ----- | ------ | -------- | -------- | --------- | ----------- | -- | ------------------ |
| 1     | agent  | 28.1 MiB | 256 MiB  | 27.1 MiB  | 34.8 MiB    | 26 | 0                  |
| 1     | server | 30.4 MiB | 512 MiB  | 17.8 MiB  | 26.2 MiB    | 34 | 0                  |
| 2     | agent  | 28.6 MiB | 256 MiB  | 20.9 MiB  | 33.6 MiB    | 25 | 0                  |
| 2     | server | 30.6 MiB | 512 MiB  | 15.1 MiB  | 24.2 MiB    | 33 | 0                  |
| 3     | agent  | 27.9 MiB | 256 MiB  | 14.6 MiB  | 35.3 MiB    | 24 | 0                  |
| 3     | server | 30.4 MiB | 512 MiB  | 15.0 MiB  | 22.9 MiB    | 33 | 0                  |

The budget column is the `AgentRSSTargetBytes` / `ServerRSSTargetBytes` default,
not the cgroup limit: the runner sets `memory.max` to 512 MiB and 1 GiB so
pressure is observable without an OOM kill masking the measurement.

`memory.events.local.oom` and `.oom_kill` were 0 in every trial. Post-GC live
heap stayed within 1-4 MB for the Agent over 111-115 GC cycles and within 1-3 MB
for the Server over 269-275 cycles. End-of-trial goroutine headers were 28
(Agent) and 22 (Server) in all three trials.

Appendix A A.9 items measured by this gate:

| A.9 item                              | bound                | trial 1 | trial 2 | trial 3 |
| ------------------------------------- | -------------------- | ------- | ------- | ------- |
| 2. `cancel_ack_latency_ms` p99        | <= 500 ms            | 84 ms   | 73 ms   | 71 ms   |
| 3. canonical cursor advance           | strictly forward     | 0 regressions | 0 | 0 |
| 3. `audit_ack_watermark_stalled_seconds` max | <= 10 s      | 0 s     | 0 s     | 1 s     |
| 7. `oom` / `oom_kill`                 | 0                    | 0 / 0   | 0 / 0   | 0 / 0   |
| 8. goroutines                         | <= 105% baseline     | 28      | 28      | 28      |
| 8. RSS recovery                       | <= 120% baseline     | pass    | pass    | pass    |

Audit coverage gap samples were 0 in every trial, and the ACK cursor was level
with the canonical cursor (lag 0) at the end of each trial. Bounded stream
buffers dropped 69,363 / 383,481 / 20,790 bytes with exact drop accounting and
recovered after stream stop in all three trials. The real restore journal was
observed on disk with a peak of 1,545 / 1,545 / 1,547 bytes across 27-41
non-zero samples per trial.

### Anonymous memory judgment

`architecture.md:1774` makes `anon Memory 지속 증가` a failure condition and
`architecture.md:1755` requires the whole observation window's trend to decide
it. The gate therefore splits the window into quarters and fails only when
every quarter slopes upward *and* the final quarter mean exceeds 120% of the
lowest earlier quarter mean - the same construct Appendix A uses for
`audit_sync_lag_records` (`기울기가 지속 양수가 아닐 것`,
`architecture.md:2198`), with Appendix A item 8's own 120% tolerance. An
endpoint comparison of three-sample averages is not usable here because
anonymous memory is a GC sawtooth: the verdict would follow which side of a
collection each window landed on. Rising-quarter counts in this run were 2-3 of
4 for the Agent and 1-3 of 4 for the Server, so no role approached the failure
shape, which requires all four.

### Not measured by this gate

`operation_progress_event_latency_ms` (Appendix A A.9 item 2, bound 1000 ms) is
not measured. The driver records canonical Audit visibility instead, which is a
different metric on the Audit plane and is bounded by A.9 item 3 (forward
progress and stall) rather than by a fixed percentile. Appendix A A.9 items 1,
4, 5, and 6 - operation completion delta across the stop window, steady-state
`audit_synced_rate` vs `audit_generated_rate`, Stats latest-wins backlog, and
per-stream log isolation throughput - are Appendix A transport-prototype
acceptance items and are not re-derived from product images here.

## Second recorded execution: at the current revision

Re-run after the fixture-safety, write-transaction, dashboard-heartbeat and
client-cancellation changes. Three trials, `status=PASS`.

    started_at              2026-08-20T11:37:54Z
    completed_at            2026-08-20T11:48:52Z
    source_revision         8693bf1937f58d139b8629fcbf01cabfc470c026
    kernel                  Linux 7.0.0-29-generic x86_64
    docker_server_version   29.7.2
    cgroup                  v2, systemd driver
    case_seconds            600
    repetitions             3
    server_image_id         sha256:0c05818885eb56673b95608de83bb2b0ea7401ad8ed23c9018809ad87c4de6ee
    agent_image_id          sha256:0d221f24ed5cb744e9b3b785bdbdf738cb3b950827951b4856e09acb9fda99f2

| Trial | Agent peak RSS / limit | Agent first → last | Server peak RSS / limit | Server first → last | Drops accounted |
|---|---|---|---|---|---|
| 1 | 30.6 / 256 MiB | 29.2 → 25.4 MiB | 31.5 / 512 MiB | 29.9 → 30.4 MiB | 65,016 B |
| 2 | 29.6 / 256 MiB | 28.6 → 25.2 MiB | 31.5 / 512 MiB | 29.4 → 29.4 MiB | 57,078 B |
| 3 | 30.9 / 256 MiB | 29.2 → 25.2 MiB | 31.2 / 512 MiB | 29.7 → 29.6 MiB | 18,900 B |

Every trial's `resource-trend.json` recorded `pass: true`, no cgroup OOM event
occurred in either role, and neither role's `memory.peak` reached 80% of its
limit. Peak RSS sits where the first execution put it - low tens of MiB against
budgets two orders of magnitude larger.

### One assertion was flaky, and is not any more

The bounded-buffer case requires a deliberately slow SSE consumer
(`--limit-rate 1k`) to *read* an event carrying an exact drop count. The stream
is delivered in order, so whether the consumer had reached the first
drop-carrying event by the time the driver finished its other work was a race
between the emitters' start-up and the consumer's position in the byte stream.

Across five trials observed before the fix, three reached that assertion with a
capture holding no drop accounting at all and failed
(`slow consumer produced no explicit bounded-buffer drop accounting`), while two
recorded exact drops - 10,773 and 80,514 bytes. The product's accounting path
was never in question: `internal/logrelay` counts every byte it drops, and the
count is carried through `producttransport` and `serverapi` to the
`dropped_bytes` field of the SSE event.

The driver now holds the slow stream open until a drop-carrying event has
actually been read, bounded by the case deadline. The assertion is unchanged: a
drop must still be produced by the bounded queue and observed by a slow client.
Three consecutive trials passed it afterwards.

This is also the resolution of an earlier open question in the campaign record,
which noted `dropped_bytes` never being observed and could not say whether that
was the relay never dropping or a notification queued behind the stream it
described. It was neither: the relay drops and reports, and the consumer had
simply not read that far.
