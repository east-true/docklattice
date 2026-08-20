# Long-running soak

Status: NOT RUN. The harness is complete and verified; no stage has produced
evidence yet.

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

All three stages are outstanding.

## Recorded execution

None. This section carries a stage's environment, sample count, per-metric
quarter medians, and verdict once that stage has produced a sealed evidence
directory. A harness that runs is not a gate that passed.

What has been run so far, and is not evidence:

- A three-minute active shakedown that exercised every workload branch, the
  trend verdict, and the closing invariants end to end. It proves the harness
  works, not that the product does.
- A one-hour active run that was **discarded rather than recorded**. Its
  descriptor counts were structurally zero: `/proc/<pid>/fd` cannot be listed
  from the host for a process owned by another uid, and both containers run as
  65532, so the check silently passed on a metric it never measured. The count
  is now taken from inside the container. The same run was also edited while it
  was executing, which invalidates it on its own - `sh` reads a script by byte
  offset, so the trailing statements would have resumed mid-token.

Stage 1 has to be run again on the corrected harness before this section says
anything.

## What this gate does not do

It does not replace the [resource gate](resource-gate.md). That gate measures
the product against its real cgroup limits across three repetitions and owns
the memory defaults; this one measures direction over time and owns none of
them. A soak passing does not promote a default, and the resource gate passing
does not make a soak unnecessary.

It also does not run `docker-daemon-restart` or any other host-wide
disruption. Those belong to a disposable host, not to a machine that has been
running the operator's containers for the whole soak.
