# Live Metrics — design

Status: proposal, revision 2. Not implemented.

Goal: an operator opens the console and sees, for every Docker host Dockpilot
manages and everything running on it, what it is using right now. Nothing is
stored, nothing is alerted on, and nothing about Audit or command handling
changes.

Revision 2 corrects five things revision 1 got wrong. They are marked ▲ and the
reasoning is kept, because each one is a trap the next person could fall into
the same way.

## 1. What already exists

Most of this feature is already in the tree, and that decides the shape of the
rest.

| Piece | Where | State |
|---|---|---|
| Per-container sampling, on demand | `internal/livestats` | works |
| Sample: CPU, memory + limit, net RX/TX, block r/w, restarts, health, uptime | `producttransport.StatsSample` | works |
| Agent handler | `agentproduct.Handler.StreamStats` | works |
| Server → browser SSE | `GET /api/v1/live/stats` | works |
| Traffic class for metrics | `transport.ClassLive` (P4), latest-wins | works |
| Default sample interval | `config.Defaults.StatsSampleInterval` = 2s | works |
| Docker event subscription | `dockeradapter.SubscribeEvents` | works, used by observed audit |

`livestats.Hub` already behaves the way §6 and §7 ask, and not because of this
feature: one relay per container shared by all viewers, collection started by
the first subscriber and stopped after the last, and `Subscription.put` keeping
only the newest sample while counting what it discarded.

## 2. What is missing

1. A host row. Nothing reports what the host as a whole is doing.
2. A stream that carries a whole host's worth of metrics in one frame.
3. Compose service and image context on the rows.
4. The matrix view itself.

## 3. Transport: a new stream, not a wider request

▲ Revision 1 proposed adding a `scope` enum to `StatsStreamRequest`. That was
wrong twice over.

A single enum cannot say "host **and** all containers", which is what the data
flow needed. And `StatsSample` is container-shaped — it has no place for load
average, filesystem capacity, or a container count — so the host case would have
arrived in a message that cannot describe it.

The single-container stream stays exactly as it is. Existing Agents and Servers
are untouched. Alongside it:

```proto
service AgentSession {
  // existing RPCs unchanged
  rpc StreamMetricsMatrix(MetricsMatrixRequest) returns (stream MetricsMatrixFrame);
}

message MetricsMatrixFrame {
  int64          observed_at_unix_nano = 1;
  WorkloadSummary workload             = 2;  // the host row
  repeated StatsSample containers      = 3;  // reuses today's sample verbatim
  uint64         dropped_frames        = 4;  // what this consumer missed
}
```

One frame is a complete picture of the host at one instant. Reusing
`StatsSample` for the container rows means the two paths cannot drift.

## 4. Negotiation: capability, not protocol version

▲ Revision 1 said the Server would send scoped requests only to sessions
reporting the current protocol version. That protection does not work.

`CurrentProductProtocolVersion` is 2 today. Adding a field and leaving the
version at 2 means an Agent built before this feature also reports 2 — the check
cannot tell pre-feature v2 from post-feature v2, and the Server would send a
request the Agent silently ignores. Raising the version to 3 would work, but it
drags the whole N/N-1 support policy open for a feature that does not need it.

Capability negotiation is smaller and self-correcting. The Agent's heartbeat
capability already carries this kind of fact, so it gains one optional flag:

```
Capability{ ..., MetricsMatrix bool }
```

An Agent that does not know the field does not set it, which is exactly the
answer the Server needs. The Server opens the matrix stream only where the flag
is true; everywhere else the host row is present and says why it is empty, in
the same place capabilities already put reasons. Nothing is inferred from
silence.

## 5. Frame-level latest-wins

▲ Revision 1 would have multiplexed 200 containers into one P4 stream while
relying on the existing per-subscription "keep the latest" rule. That is safe
per container and unsafe per stream: with one latest slot for the whole stream,
a busy container's samples overwrite everyone else's, and a quiet container can
starve indefinitely.

The frame is the unit instead. Every `StatsSampleInterval` the Agent builds one
frame holding the current value for every container it is watching, and the
newest whole frame wins. Dropping a frame loses one round of everything, and the
next frame carries every container again, so nothing starves and nothing queues.
`dropped_frames` reports how many rounds a consumer missed.

## 6. What the host row actually contains

▲ Revision 1 proposed reading host CPU, memory, load and network from `/proc`.
Measured on this machine, that is partly wrong and entirely unsafe to promise:

| File | In a bridge-networked container | Verdict |
|---|---|---|
| `/proc/net/dev` | shows `lo eth0` — the container's own interfaces, host shows 26 | **wrong data, silently** |
| `/proc/stat`, `/proc/meminfo`, `/proc/loadavg` | identical to the host (same `MemTotal`, same load) | right *here* |

The network case settles it: the official deployment is a container, and host
network RX/TX read this way would be the Agent's own traffic presented as the
host's. The others happen to be right on this kernel with no `/proc`
virtualization in the way — but that is a property of the environment, not a
promise Dockpilot can make for every host it is installed on.

So v1 does not read host `/proc` at all. The host row is **the Docker workload
this Agent manages**, built from sources that mean the same thing in every
deployment:

| Row field | Source | Namespace-independent because |
|---|---|---|
| CPU capacity, memory capacity | Docker Engine `info` (`NCPU`, `MemTotal`) | the daemon runs on the host |
| containers running / total | Docker Engine `info` | same |
| CPU / memory / network / block I/O in use | sum of the container samples in this frame | measured per container by the Engine |
| managed filesystems: total and free | `statfs` on discovery roots and the Agent state directory | these are the paths Dockpilot writes to, whatever they are mounted from |

`dockeradapter` gains an `Info()` accessor; it does not have one yet.

The row is labelled as what it is. It is not "Host CPU" — it is the workload
Dockpilot manages on that host, next to the capacity the Engine reports. An
operator reading it is not misled about what is excluded, and the honest gap is
stated in the UI rather than papered over with a number that is right on some
hosts.

Real host OS metrics — CPU steal, per-NIC traffic, every mount — remain out of
scope, as they were before this feature. Adding them means either host `/proc`
access or namespace entry, which is a deployment and security change with its
own design, not a field on a frame.

## 7. Managed filesystems

Reported for the discovery roots and the Agent state directory, deduplicated by
filesystem so two roots on one mount appear once. Called "Managed filesystems",
not "Host disk", because that is what it is.

## 8. Compose service metrics

No collector, and this holds for every aggregate row. Host workload, project and
service rows are all projections of the same container samples in the same
frame — there is never a second measurement of the same thing to disagree with
the first. The Server aggregates from the
container samples already in the frame, joined to the project/service mapping
discovery holds.

Per metric, because they do not mean the same thing:

| Metric | Aggregate |
|---|---|
| CPU percent, memory usage, network RX/TX, block I/O, restarts | sum |
| memory limit | **unbounded** if any member is unlimited — not a number |
| memory percent | **not computed** when the limit is unbounded |
| health | worst of the members |
| uptime | minimum — a service is only as old as its youngest container |

▲ Revision 1 summed memory limits and marked the result "partial". A number
labelled partial still gets read as a number and divided into. Unbounded is a
different kind of answer and is presented as one.

Every service row states how many containers it covers.

## 9. Container membership while the stream is open

▲ Revision 1 had no answer for this. A container started after the viewer
subscribed would never appear, and a matrix that claims to show what is running
would quietly be showing what was running when the page opened.

The stream owns a membership set:

- On subscribe, a snapshot of running containers seeds it.
- While a viewer is present, `dockeradapter.SubscribeEvents` — already running
  for observed audit, filtered to containers — drives start/die/destroy into the
  set. Membership follows the same event stream the audit trail does, so the two
  cannot disagree about what exists.
- A bounded periodic reconcile (one list call per interval, not per container)
  repairs anything a missed event would have stranded, so a dropped event
  degrades a frame rather than the view.

A container that leaves takes its relay with it through the existing teardown
path.

## 9a. Acceptance conditions

Three properties that are easy to lose in implementation and expensive to
notice later. They are conditions, not aspirations: each has a test in §15.

**A frame is self-consistent.** Membership is snapshotted once at the start of
assembly, and both the container rows and the workload aggregate are computed
from that one set. A container dying mid-assembly must not produce a frame whose
summary counts nine and whose rows list eight. The next frame showing eight is
the correct outcome; a frame disagreeing with itself is not. Metrics are live,
so no cross-frame transaction is needed — only that one frame means one thing.

**Event consumers do not compete.** `dockeradapter.SubscribeEvents` calls
`api.Events` per invocation and returns its own channels, so observed audit and
matrix membership already get independent Docker subscriptions and cannot steal
each other's events. That is verified, not assumed, and this design depends on
it. If that ever becomes a single shared subscription, a fan-out boundary has to
arrive in the same change — but no event bus is built for it now. The cost is
one extra `/events` connection per Agent while a matrix viewer exists, released
with the last viewer.

**An unmapped container is still shown.** Docker events surface a new container
before discovery has mapped it to a project and service. Such a row appears with
its metrics and with `project`/`service` reported as unknown — it is never
hidden. The Docker Engine is the authority for what is running and discovery
metadata is context; letting a failed metadata join erase a running container
would invert that, and would make the matrix quietly wrong exactly when
something has just been deployed.

## 10. What this scales, and what it does not

▲ Revision 1 claimed relay and goroutine counts would be proportional to hosts
rather than containers, while also stating there would be 200 relays for 200
containers. Both cannot be true, and the second one is.

| Quantity | Order | Changed by this design |
|---|---|---|
| Browser → Server streams | O(hosts being viewed) | yes — one per host view, not per container |
| Server → Agent streams | O(hosts being viewed) | yes — one per host, fanned out to N browsers |
| Agent Docker stats collectors | **O(running containers)** | no — unchanged, and this is the real cost |
| Agent collectors while nobody is watching | 0 | unchanged |

The protocol change fixes stream count. It does not and cannot make the Docker
stats collection sublinear — something has to ask the Engine about each
container. That is a real cost, it is stated here rather than discovered later,
and it is what the 200- and 500-container Resource Matrix runs exist to measure.

## 11. Keeping Audit and commands unaffected

Metrics are `ClassLive` (P4). P0 control and P1 durable are already declared
protected from P3/P4 starvation, and this feature uses that policy rather than
adding to it.

- No backlog anywhere: the Agent holds one frame, the subscription holds one,
  the Server holds the newest per host.
- Nothing is collected without a viewer.
- N browsers on one host produce one Agent stream.
- Drops are counted and shown, as logs already do.

## 12. Sampling interval

`StatsSampleInterval` (2s) already exists and already applies per relay; the
frame is assembled on the same tick. No second knob until something measured
asks for one. Discovery's five-minute cycle is untouched and unrelated.

## 13. UI

One view, existing components. Rows are host → project → service → container;
columns CPU, memory, network, block I/O, and image for container rows. Sorting
and filtering only — no charts, because a chart implies a history this feature
does not keep.

The host row shows managed workload against Engine-reported capacity, and says
plainly that host OS metrics are not included. Hosts whose Agent predates the
capability show the reason where capability reasons already appear.

## 14. Deliberately not built

Historical metrics, time-series storage, retention, downsampling, alerting,
thresholds, Prometheus compatibility, process-level metrics, image-level runtime
metrics, prediction. None started, none prepared for.

`docs/interface-freeze.md` §9 says metrics are live, viewer-scoped, ephemeral,
with no server-side history. This stays inside that sentence; the section is
revised to name the new stream, the capability, and the host row's meaning.

## 15. Test plan

- Frame assembly: every watched container present in every frame; a busy
  container cannot displace a quiet one.
- Membership: container started after subscribe appears; destroyed container
  leaves; a missed event is repaired by reconcile.
- Aggregation: the per-metric table above, including unbounded memory limit and
  memory percent being withheld, worst health, youngest uptime, container count.
- Capability: no matrix stream is opened against an Agent without the flag, and
  the host row carries a reason.
- Lifecycle: first viewer starts collection, second shares it, last stops it;
  reconnect does not duplicate; Agent restart and Server restart recover.
- Backpressure: a stalled consumer coalesces frames, `dropped_frames` reports
  it, and the Audit cursor keeps advancing throughout.
- Frame self-consistency: a container removed during assembly yields a frame
  whose summary and rows agree, and the next frame reflects the removal.
- Unmapped containers: a container present in Docker but absent from discovery
  appears with unknown project/service and correct metrics.
- Scaling: stream counts stay O(hosts); collector count is confirmed
  O(containers). Measured at 200 and 500 containers rather than asserted — the
  question is where the bottleneck is, not whether a pre-chosen number passes.
  Recorded per run: relay and goroutine counts, Docker stats cost, Agent and
  Server RSS and CPU, file descriptors, frame serialization size, Agent→Server
  bandwidth, and whether P0/P1 were starved. The supported range is decided from
  those numbers; finding a limit at 500 is a measurement, not a design failure.

## 16. Validation

Resource Matrix re-run on the new revision, three trials, existing acceptance
conditions, not reusing current evidence. Idle and metrics-active measured
separately, since the design rests on idle costing nothing. The combined-load
case adds metrics to the existing logs + compose + audit mix, and the Audit
cursor must advance throughout.
