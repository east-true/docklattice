# Long-running soak

Status: Stages 1, 2, and 3 PASS at current `main` revision
`7249c29a84018ff5a6b2bb351e4a5525ec7840d8`.

Long-duration soak is **excluded from the v1 Core Interface Freeze gate by
project decision**. The release campaign adopted all three stages as a
separate Release Candidate gate, and all three are now established at the
candidate revision.

Every other gate in this directory injects something and asserts what survives.
This one injects nothing. It runs the product for hours and asserts that
ordinary operation leaves nothing behind, because the failures it looks for -
a stream that never releases its buffer, a session that outlives its
connection, a WAL that grows faster than it is truncated, an Audit cursor that
never catches up - do not fail a request. They accumulate, and only time makes
them visible.

## Runner

Run the static verifier first. It needs no Docker and starts nothing:

```sh
./scripts/verify-soak-harness.sh
```

Then the soak itself:

```sh
SOAK_MODE=active SOAK_SECONDS=3600 SOAK_SAMPLE_SECONDS=30 SOAK_SETTLE_SECONDS=120 \
  ./scripts/run-soak-e2e.sh \
  /absolute/path/to/new-soak-evidence \
  '<server image ID>' '<agent image ID>' '<fixture image ID>'
```

The safety boundary is the one every matrix here keeps: exact local image IDs
with `--pull never`, a throwaway runtime root, an evidence directory that must
not already exist, a local Linux Engine on cgroup v2, and evidence sealed
read-only under a `SHA256SUMS` manifest. Nothing is built, pulled, or fetched.

| Setting | Default | Meaning |
|---|---|---|
| `SOAK_MODE` | `mixed` | `idle`, `active`, or `mixed` |
| `SOAK_SECONDS` | 3600 | Length of the measured run; must cover at least 16 samples |
| `SOAK_SAMPLE_SECONDS` | 30 | Sampling interval, at least 5 s |
| `SOAK_SETTLE_SECONDS` | 90 | Quiet window after the workload, at least two samples |
| `SOAK_STATE_CEILING_KIB` | 262144 | Ceiling for the Agent state directory |

## Fixture safety

A soak drives real Compose operations against the same host for hours, so it
resolves its target exactly as the hardening and abuse matrices do: by the
Compose project name it generated, at the discovery root it created, confirmed
against the uid the Agent must derive for that root. A request that names any
other project is refused by the harness before it is sent. See
[`hardening-matrix-e2e.md`](hardening-matrix-e2e.md) for the same rule stated
in full.

This is what makes the soak safe to run on a working machine. It is not what
makes it *correct* to run there - a host doing other work adds noise to every
slope below - but nothing it does can reach a project it did not create.

## Modes

**Idle** leaves the background loops alone: heartbeat, discovery, Docker
events, Audit sync, retention. No user operations at all. This is the mode that
finds timers that are never stopped and sessions that are never released,
because nothing else is happening to hide them.

**Active** repeats a workload cycle: project file and environment reads,
activity and inventory queries, Compose `ps` and `config`, a log stream and a
stats stream opened and closed, a deliberately slow log consumer every fourth
cycle, a real backup every third, a cancelled operation every sixth, a Compose
restart every eighth, and a network partition and reconnect every twelfth.

**Mixed** alternates the two in blocks of four samples, so each phase is long
enough to show its own slope.

## What is measured

Per sample, for both the Server and the Agent:

```text
process RSS            memory retained rather than reused
threads                runtime growth behind goroutines that never end
open file descriptors  streams and files opened but not closed
cgroup memory.current  the same from the hierarchy the Agent reports from
cgroup memory.events   oom and oom_kill counters
```

And for the fleet:

```text
Agent state size       a WAL or backup set that grows without bound
Audit ack / delivery   the sync lag between them
Audit coverage gaps    and the revision behind them
host state             ACTIVE at every single sample, not merely at the end
sampling HTTP status   every sampling request answered
```

## How a soak fails

Absolute ceilings are the resource gate's job, and it already measures them
against the real limits. What a soak can prove is direction, and direction is
noisy: any single reading can be high.

A metric fails when **every quarter median is above the one before it** *and*
the last quarter exceeds the first by more than its tolerance. Noise does not
produce four consecutive rises; a leak always does. A run that rises and then
settles - the normal shape of a warm-up - passes, which is the point.

Two measurements are judged differently:

- **Agent state size** grows legitimately while work happens, because every
  backup the soak takes is retained on purpose and retention only evicts near
  the budget. What must not happen is that it keeps growing after the workload
  stops, so it is judged across the settle window instead, against a 10%
  tolerance and the configured ceiling.
- **OOM counters, HTTP failures, and host state** are not trends at all. One
  is a failure.

The Server log is also read at the end: SQLite contention or a failed API
request during a soak fails the run, whatever the slopes say.

## Closing invariants

A soak that ends clean on every slope has still leaked if it leaves state
behind, so it closes with the same invariant check every hardening scenario
uses: exactly one ACTIVE Agent, a project lock that is free, no surviving
restore journal, no staging orphan, Audit coverage whose every gap names its
source and precision, an acknowledged cursor that never passed the delivery
cursor, the Compose file on disk matching the digest Dockpilot reports, and the
project secret absent from all recorded evidence.

## Stages

Running twelve hours first would spend a night to learn something an hour
would have shown. The stages exist so that a leak is found at the cheapest
length that can reveal it:

| Stage | Length | Mode |
|---|---|---|
| 1 | 1 hour | active |
| 2 | 2-4 hours | mixed |
| 3 | 8-12 hours | mixed, overnight |

A failure at any stage is fixed before the next is attempted. A later stage
does not re-prove an earlier one; it looks for the slower accumulation the
shorter run could not resolve.

All three stages have passed.

## Recorded execution

### Stage 3 - eight-hour mixed overnight soak

    verdict                 PASS
    mode                    mixed
    duration                28800 s measured, 120 s settle, 918 judged samples
    sample interval         30 s
    started / finished      2026-08-27T06:13:48Z / 2026-08-27T14:16:00Z
    kernel                  Linux 6.8.0-138-generic x86_64
    docker_engine           29.1.3
    release_version         0.1.0-rc.1-validation.7249c29
    release_revision        7249c29a84018ff5a6b2bb351e4a5525ec7840d8
    server_image_id         sha256:f6feba191571eb4a14f2ef7cb5b75e258df49f837f8ceccae79042933a0aa743
    agent_image_id          sha256:6066a3373c29cce6a9e3e1cb688a158689336065bbdd3a7a0400b02741a841e3
    fixture_image_id        sha256:dc2d74b28e4cf8984fa52af1f39bc7c3d9c73760b41a74d629f5d11b1ab28616
    evidence                2.43 MiB, 498-entry SHA256SUMS

The quiet disposable VM again had no unrelated Compose project. Alternating
four-sample active and idle blocks drove 913 cycles, 1,028 stream opens and
closes, 457 operations, and 38 partition-and-reconnect injections.

| Metric | Quarter medians | Growth | Monotonic | Verdict |
|---|---|---:|---|---|
| `server.rss_kib` | 30344, 31132, 31192, 31364 | +3.36% | yes | PASS |
| `server.threads` | 10, 11, 11, 11 | +10% | no | PASS |
| `server.fds` | 16, 16, 16, 16 | 0% | no | PASS |
| `agent.rss_kib` | 27504, 28600, 29720, 30704 | +11.63% | yes | PASS |
| `agent.threads` | 11, 11, 11, 11 | 0% | no | PASS |
| `agent.fds` | 10, 10, 10, 10 | 0% | no | PASS |
| `audit.lag` | 1, 1, 1, 1 | 0% | no | PASS |
| `audit.coverage_revision` | 0, 0, 0, 0 | 0% | no | PASS |

All 919 recorded samples kept the Agent ACTIVE, with no OOM event, failed HTTP
sample, or Audit coverage gap. Audit lag briefly reached 2 in one observed
sample and returned to 1; every quarter median remained 1. Peak RSS was 34016
KiB for the Server and 32368 KiB for the Agent. Agent state reached 5328 KiB
and stayed exactly flat across all five settle samples. The Server log had no
SQLite contention or failed API request, the closing invariant check passed,
and cleanup left no Container or soak network behind. Both the read-only VM
evidence and the transferred copy independently passed their `SHA256SUMS`
verification.

The RSS quarter medians remain monotonic, but their eight-hour growth is 3.36%
and 11.63%, both inside the predefined 30% tolerance. Threads, descriptors,
Audit direction, and settled state are flat, so Stage 3 and the adopted
Release Candidate soak gate pass. This evidence does not claim that RSS can
never grow beyond an eight-hour window; it establishes the bounded window and
verdict defined by this gate.

An earlier Stage 3 attempt was discarded after the disposable VM powered off
before producing `STATUS` or `SHA256SUMS`. Its three fixture Containers and two
networks were identified by exact harness names and removed before this run;
none of its samples are included above.

### Stage 2 - two-hour mixed soak

    verdict                 PASS
    mode                    mixed
    duration                7200 s measured, 120 s settle, 234 judged samples
    sample interval         30 s
    started / finished      2026-08-26T01:30:48Z / 2026-08-26T03:33:00Z
    kernel                  Linux 6.8.0-137-generic x86_64
    docker_engine           29.1.3
    release_version         0.1.0-rc.1-validation.7249c29
    release_revision        7249c29a84018ff5a6b2bb351e4a5525ec7840d8
    server_image_id         sha256:f6feba191571eb4a14f2ef7cb5b75e258df49f837f8ceccae79042933a0aa743
    agent_image_id          sha256:6066a3373c29cce6a9e3e1cb688a158689336065bbdd3a7a0400b02741a841e3
    fixture_image_id        sha256:dc2d74b28e4cf8984fa52af1f39bc7c3d9c73760b41a74d629f5d11b1ab28616
    evidence                780 KiB, 154-entry SHA256SUMS

The quiet disposable VM again had no unrelated Compose project. Alternating
four-sample active and idle blocks drove 229 cycles, 258 stream opens and
closes, 113 operations, and 9 partition-and-reconnect injections.

| Metric | Quarter medians | Growth | Monotonic | Verdict |
|---|---|---:|---|---|
| `server.rss_kib` | 30136, 30648, 31108, 31560 | +4.73% | yes | PASS |
| `server.threads` | 10, 11, 11, 11 | +10% | no | PASS |
| `server.fds` | 16, 16, 16, 16 | 0% | no | PASS |
| `agent.rss_kib` | 27180, 27688, 28160, 28588 | +5.18% | yes | PASS |
| `agent.threads` | 10, 10, 11, 11 | +10% | no | PASS |
| `agent.fds` | 10, 10, 10, 10 | 0% | no | PASS |
| `audit.lag` | 1, 1, 1, 1 | 0% | no | PASS |
| `audit.coverage_revision` | 0, 0, 0, 0 | 0% | no | PASS |

All 235 recorded samples kept the Agent ACTIVE, with no OOM event, failed HTTP
sample, or Audit coverage gap. Peak RSS was 32572 KiB for the Server and 29376
KiB for the Agent; the final samples were below those peaks. Agent state
reached 1360 KiB and stayed exactly flat across all five settle samples. The
Server log had no SQLite contention or failed API request, the closing
invariant check passed, and cleanup left no Container or soak network behind.
The transferred evidence independently passed its `SHA256SUMS` verification.

Both RSS quarter series again rise monotonically, by 4.73% and 5.18%. They
remain far inside the 30% tolerance and the other retained-resource indicators
are flat, so Stage 2 passes. Stage 3 subsequently measured the longer overnight
window above.

### Stage 1 - current release-candidate revision

    verdict                 PASS
    mode                    active
    duration                3600 s measured, 120 s settle, 118 judged samples
    sample interval         30 s
    started / finished      2026-08-26T00:11:14Z / 2026-08-26T01:13:26Z
    kernel                  Linux 6.8.0-137-generic x86_64
    docker_engine           29.1.3
    release_version         0.1.0-rc.1-validation.7249c29
    release_revision        7249c29a84018ff5a6b2bb351e4a5525ec7840d8
    server_image_id         sha256:f6feba191571eb4a14f2ef7cb5b75e258df49f837f8ceccae79042933a0aa743
    agent_image_id          sha256:6066a3373c29cce6a9e3e1cb688a158689336065bbdd3a7a0400b02741a841e3
    fixture_image_id        sha256:dc2d74b28e4cf8984fa52af1f39bc7c3d9c73760b41a74d629f5d11b1ab28616
    evidence                492 KiB, 98-entry SHA256SUMS

The quiet disposable VM had no unrelated Compose project. The run drove 113
cycles, 254 stream opens and closes, 84 operations, and 9
partition-and-reconnect injections.

| Metric | Quarter medians | Growth | Monotonic | Verdict |
|---|---|---:|---|---|
| `server.rss_kib` | 29860, 30552, 30936, 31216 | +4.54% | yes | PASS |
| `server.threads` | 10, 10, 10, 10 | 0% | no | PASS |
| `server.fds` | 15, 16, 16, 16 | +6.67% | no | PASS |
| `agent.rss_kib` | 26980, 27560, 27864, 28136 | +4.28% | yes | PASS |
| `agent.threads` | 11, 11, 11, 11 | 0% | no | PASS |
| `agent.fds` | 10, 10, 10, 10 | 0% | no | PASS |
| `audit.lag` | 1, 1, 1, 1 | 0% | no | PASS |
| `audit.coverage_revision` | 0, 0, 0, 0 | 0% | no | PASS |

All 119 recorded samples kept the Agent ACTIVE, with no OOM event, failed HTTP
sample, or Audit coverage gap. Peak RSS was 31548 KiB for the Server and 28684
KiB for the Agent. Agent state reached 1112 KiB and stayed exactly flat across
all five settle samples. The Server log had no SQLite contention or failed API
request, the closing invariant check passed, and cleanup left no Container or
soak network behind. The transferred evidence independently passed its
`SHA256SUMS` verification.

Both RSS quarter series still rise monotonically, by 4.54% and 4.28%. The last
samples are below their peaks and the growth is far inside the 30% tolerance,
so Stage 1 passes. Stage 2 remains necessary to distinguish slow Go runtime
settling from accumulation that an hour is too short to expose.

### Earlier Stage 1 - one-hour active soak

    verdict                 PASS
    mode                    active
    duration                3600 s measured, 120 s settle, 118 judged samples
    sample interval         30 s
    started / finished      2026-08-20T04:04:53Z / 2026-08-20T05:07:03Z
    kernel                  Linux 7.0.0-28-generic x86_64
    docker_engine           29.7.2
    release_version         1.0.0
    release_revision        ddf11ad1745bfa689f6f8438d257f8bead19137a
    server_image_id         sha256:6d4f5756aa3a43143bd35c499ecedf16ad64749e3f5d8dba50f19f5ac45df6a4
    agent_image_id          sha256:11dbb69caedbe67e47090c04c40aa714fbfdee90b4a7384324738cf05cf5b5ad
    fixture_image_id        sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4
    evidence                516 KiB, 98-entry SHA256SUMS

Workload actually driven: 113 cycles, 254 stream opens and closes, 84
operations, 9 partition-and-reconnect injections.

Per-metric quarter medians and verdicts:

| Metric | Quarter medians | Growth | Monotonic | Verdict |
|---|---|---:|---|---|
| `server.rss_kib` | 29916, 29932, 30660, 30880 | +3.22% | yes | PASS |
| `server.threads` | 12, 12, 12, 13 | +8.33% | no | PASS |
| `server.fds` | 16, 16, 16, 16 | 0% | no | PASS |
| `agent.rss_kib` | 26812, 26940, 27192, 27468 | +2.45% | yes | PASS |
| `agent.threads` | 12, 12, 12, 12 | 0% | no | PASS |
| `agent.fds` | 10, 10, 10, 10 | 0% | no | PASS |
| `audit.lag` | 1, 1, 1, 1 | 0% | no | PASS |
| `audit.coverage_revision` | 0, 0, 0, 0 | 0% | no | PASS |

Hard checks: no OOM event, no failed sampling request, and the Agent ACTIVE at
every one of the 118 samples. Agent state settled at 1108 KiB with 0% growth
across the settle window, three orders of magnitude below its ceiling. The
Server log carried no SQLite contention and no failed API request. The closing
invariant check passed.

**What is worth carrying into Stage 2.** Both RSS series rose in every quarter.
The rise is small - 3.22% and 2.45% over an hour, well inside the 30%
tolerance, with peaks of 32176 KiB and 30212 KiB and last samples below those
peaks - and the shape is consistent with the Go scavenger returning pages
slowly rather than with retention. But "monotonic yet small" is exactly the
shape that a longer run resolves and an hour cannot: the same slope sustained
over twelve hours would not stay inside tolerance. Stage 2 exists to tell those
two readings apart, and this is the number it should be compared against.

**Environment note.** This run shared the host with nine unrelated Compose
projects belonging to the operator, recorded as `other_projects_on_host=9`.
The harness touched none of them - every target was checked against the fixture
identity it derived - but their CPU and I/O are noise in every slope above. A
quiet host would produce a cleaner measurement, not a different verdict.

### Earlier runs, which are not evidence

- A three-minute active shakedown that exercised every workload branch, the
  trend verdict, and the closing invariants end to end. It proves the harness
  works, not that the product does.
- A one-hour active run that was **discarded rather than recorded**. Its
  descriptor counts were structurally zero: `/proc/<pid>/fd` cannot be listed
  from the host for a process owned by another uid, and both containers run as
  65532, so the run reported a passing trend for a metric it never measured.
  The count is now taken from inside the container, where the process can see
  its own descriptors. A metric that cannot fail is worse than an absent one,
  so the run was thrown away rather than published with a footnote.

## What this gate does not do

It does not replace the [resource gate](resource-gate.md). That gate measures
the product against its real cgroup limits across three repetitions and owns
the memory defaults; this one measures direction over time and owns none of
them. A soak passing does not promote a default, and the resource gate passing
does not make a soak unnecessary.

It also does not run `docker-daemon-restart` or any other host-wide
disruption. Those belong to a disposable host, not to a machine that has been
running the operator's containers for the whole soak.

## Superseded incomplete re-run at revision c6366b8

The earlier 2026-08-20 recorded run predates the dashboard heartbeat fix, and
architecture section 30 asks for a soak after a transport or session change. A
re-run was started at revision `c6366b83dc31c712b58ace47fe384bffb15a2a32` and
stopped by the operator at 17.6 of 60 minutes. It did not establish Stage 1 at
that revision and is retained only as historical context; the complete run at
`7249c29` above now supplies the current evidence.

What the 35 samples it did produce showed, recorded because discarding it would
be worse than labelling it:

| Series | First | Peak | Last |
|---|---|---|---|
| Server RSS | 27.2 MiB | 31.2 MiB | 29.2 MiB |
| Agent RSS | 25.2 MiB | 28.0 MiB | 26.3 MiB |
| Server descriptors | 15 | 16 | 16 |
| Agent descriptors | 11 | 11 | 10 |
| Server / Agent threads | 11 / 11 | 12 / 12 | 12 / 12 |
| Agent state on disk | 48 KiB | — | 356 KiB |
| Audit lag | 1 | 1 | 1 |

Zero gaps, zero OOM events, zero HTTP errors, and `host_state` ACTIVE in every
sample. Seventeen minutes is not a soak: none of this is evidence that nothing
accumulates, only that nothing had accumulated visibly by then.
