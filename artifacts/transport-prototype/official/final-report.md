# Transport Prototype final report

- Matrix complete: **true**
- Candidate A single connection: **true**
- Candidate B single connection: **false**
- Two-connection fallback required: **false**
- Recommendation: **REVERSE_GRPC** — only Candidate A passed every single-connection acceptance group

## Median decisions

| Group | Result |
|---|---|
| `grpc/loopback/scenario-1/baseline` | PASS |
| `grpc/loopback/scenario-1/paused` | PASS |
| `grpc/loopback/scenario-2/rate-100` | PASS |
| `grpc/loopback/scenario-2/rate-20` | PASS |
| `grpc/loopback/scenario-2/rate-50` | PASS |
| `grpc/loopback/scenario-3/baseline-1-agent` | PASS |
| `grpc/loopback/scenario-3/scale` | PASS |
| `grpc/loopback/scenario-4/cancellation` | PASS |
| `grpc/netem/scenario-1/baseline` | PASS |
| `grpc/netem/scenario-1/paused` | PASS |
| `grpc/netem/scenario-2/rate-100` | PASS |
| `grpc/netem/scenario-2/rate-20` | PASS |
| `grpc/netem/scenario-2/rate-50` | PASS |
| `websocket/loopback/scenario-1/baseline` | PASS |
| `websocket/loopback/scenario-1/paused` | PASS |
| `websocket/loopback/scenario-2/rate-100` | PASS |
| `websocket/loopback/scenario-2/rate-20` | PASS |
| `websocket/loopback/scenario-2/rate-50` | PASS |
| `websocket/loopback/scenario-3/baseline-1-agent` | PASS |
| `websocket/loopback/scenario-3/scale` | FAIL |
| `websocket/loopback/scenario-4/cancellation` | PASS |
| `websocket/netem/scenario-1/baseline` | PASS |
| `websocket/netem/scenario-1/paused` | PASS |
| `websocket/netem/scenario-2/rate-100` | PASS |
| `websocket/netem/scenario-2/rate-20` | PASS |
| `websocket/netem/scenario-2/rate-50` | PASS |

### `grpc/loopback/scenario-1/baseline`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.002 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 9.459 |  |
| 2b | operation progress latency | PASS | 3/3 | 1.603 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.056 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 1794.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.000 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.000 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.000 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 2.225 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 22044672.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.587 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.079 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 2.015 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21827584.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.340 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.443 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 5.081 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 8.009 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/loopback/scenario-1/paused`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.001 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 9.552 |  |
| 2b | operation progress latency | PASS | 3/3 | 1.925 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.055 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 1794.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.000 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.000 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.000 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.272 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 2.295 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21987328.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.464 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.281 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -2.797 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 22155264.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.432 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.464 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 4.726 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 7.080 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/loopback/scenario-2/rate-100`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.001 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 5.285 |  |
| 2b | operation progress latency | PASS | 3/3 | 1.845 |  |
| 3a | audit cursor advances | PASS | 3/3 | 18000.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.008 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.024 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.024 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.024 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.375 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21651456.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.270 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.401 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -1.829 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21798912.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.455 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 0.916 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 3.365 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 6.094 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/loopback/scenario-2/rate-20`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.001 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 8.618 |  |
| 2b | operation progress latency | PASS | 3/3 | 2.205 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.056 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.024 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.024 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.024 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.080 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.412 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 22036480.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.686 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.801 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 1.644 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 22122496.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.755 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.697 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 5.883 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 7.640 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/loopback/scenario-2/rate-50`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.001 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 8.331 |  |
| 2b | operation progress latency | PASS | 3/3 | 0.986 |  |
| 3a | audit cursor advances | PASS | 3/3 | 9000.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.026 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.024 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.024 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.024 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.277 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21925888.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.852 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.596 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -1.145 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 22106112.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.686 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.536 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 4.993 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 7.497 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/loopback/scenario-3/baseline-1-agent`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 0.173 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21626880.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.151 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 0.277 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 1.133 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21757952.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.606 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/loopback/scenario-3/scale`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 6.384 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 2.012 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 22020096.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 2.439 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 2/3 | 6.329 | 26.403 % growth across window medians |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 2/3 | 12.202 | 51.955 % growth across window medians |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 27435008.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 4.374 |  |
| diag.server.incremental_rss | server incremental RSS per Agent | PASS | 3/3 | 298792.421 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/loopback/scenario-4/cancellation`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 2a | cancel ACK latency | PASS | 3/3 | 5.534 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 2.085 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 2.695 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21626880.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.258 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.015 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 2.743 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21757952.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 1.242 |  |
| 8.buffer | buffer recovery | PASS | 3/3 | 0.000 |  |
| 8a | goroutine recovery | PASS | 3/3 | 0.800 |  |
| 8b | RSS recovery | PASS | 3/3 | 1.038 |  |
| 8c | gRPC stream/window recovery | PASS | 3/3 | 0.000 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/netem/scenario-1/baseline`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.020 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 54.127 |  |
| 2b | operation progress latency | PASS | 3/3 | 38.716 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.090 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 1794.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.026 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.000 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.017 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.941 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 2.161 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21680128.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.107 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.607 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -3.745 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21966848.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.848 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 21.186 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 39.394 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 52.828 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/netem/scenario-1/paused`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.001 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 52.887 |  |
| 2b | operation progress latency | PASS | 3/3 | 37.944 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.276 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 1794.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.000 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.009 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.035 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.567 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 3.001 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21868544.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.745 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.663 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -0.016 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21942272.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.699 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 22.098 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 36.901 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 52.639 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/netem/scenario-2/rate-100`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.022 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 55.501 |  |
| 2b | operation progress latency | PASS | 3/3 | 31.589 |  |
| 3a | audit cursor advances | PASS | 3/3 | 17999.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.067 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.094 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.155 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.207 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.123 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21520384.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.083 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.551 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -3.454 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21979136.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.772 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 21.562 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 46.243 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 56.362 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/netem/scenario-2/rate-20`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.024 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 48.249 |  |
| 2b | operation progress latency | PASS | 3/3 | 34.414 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.094 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.103 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.155 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.164 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 2.317 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21753856.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.336 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.404 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 5.673 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 22224896.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.985 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 22.074 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 41.541 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 51.326 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `grpc/netem/scenario-2/rate-50`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.020 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 52.014 |  |
| 2b | operation progress latency | PASS | 3/3 | 40.446 |  |
| 3a | audit cursor advances | PASS | 3/3 | 9000.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.183 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.103 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.129 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.155 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.455 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 11.970 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21667840.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.436 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.463 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 4.947 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21970944.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.584 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 22.121 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 46.883 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 53.278 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/loopback/scenario-1/baseline`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.001 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 5.435 |  |
| 2b | operation progress latency | PASS | 3/3 | 4.018 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 2/3 | 0.055 | 96.148s max |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 1794.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.000 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.000 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.000 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.496 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21794816.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.953 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.916 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 57.323 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21749760.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.990 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.711 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 4.236 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 7.060 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/loopback/scenario-1/paused`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.001 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 9.234 |  |
| 2b | operation progress latency | PASS | 3/3 | 0.789 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.927 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 1794.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.000 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.000 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.000 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.794 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 5.200 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21876736.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.663 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.203 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 2/3 | 76.847 | 76.847 % growth across window medians |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21798912.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.811 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.649 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 4.651 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 7.400 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/loopback/scenario-2/rate-100`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.002 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 6.232 |  |
| 2b | operation progress latency | PASS | 3/3 | 1.984 |  |
| 3a | audit cursor advances | PASS | 3/3 | 18000.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.011 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.024 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.024 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.024 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.408 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21614592.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.447 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.798 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 4.022 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21528576.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.729 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.494 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 3.940 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 6.149 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/loopback/scenario-2/rate-20`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.004 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 8.358 |  |
| 2b | operation progress latency | PASS | 3/3 | 4.716 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.056 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.024 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.024 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.024 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.973 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21606400.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.217 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.498 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 91.348 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21610496.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.915 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.864 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 4.446 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 7.368 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/loopback/scenario-2/rate-50`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.002 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 6.444 |  |
| 2b | operation progress latency | PASS | 3/3 | 1.249 |  |
| 3a | audit cursor advances | PASS | 3/3 | 9000.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.867 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.024 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.024 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.024 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.443 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21700608.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.088 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.034 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 54.488 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21557248.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.901 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 1.574 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 4.386 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 5.998 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/loopback/scenario-3/baseline-1-agent`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 0.177 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21626880.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.000 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 0.459 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -5.425 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21757952.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.000 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/loopback/scenario-3/scale`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 5.596 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 9.321 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21889024.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 2.469 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 3.954 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 6.600 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 28467200.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 2.494 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.server.incremental_rss | server incremental RSS per Agent | PASS | 3/3 | 355705.263 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | FAIL | 1/3 | 0.000 | config=true agents=20/20 samples=601 errors=0 register/coverage/heartbeat=20/20/20; duration=600.0/600.0s min_audit=5.00/s min_log=199932.7B/s stats=0.499/s echo=0.000/s; config=true agents=20/20 samples=601 errors=7 register/coverage/heartbeat=20/20/20; duration=600.1/600.0s min_audit=5.00/s min_log=161942.1B/s stats=0.500/s echo=0.000/s |

### `websocket/loopback/scenario-4/cancellation`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 2a | cancel ACK latency | PASS | 3/3 | 3.220 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 4.474 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 4.293 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21233664.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 2.548 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 3.428 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 6.462 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 20918272.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 1.838 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 8.buffer | buffer recovery | PASS | 3/3 | 0.000 |  |
| 8a | goroutine recovery | PASS | 3/3 | 0.800 |  |
| 8b | RSS recovery | PASS | 3/3 | 1.019 |  |
| 8c | WebSocket stream/credit recovery | PASS | 3/3 | 0.000 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/netem/scenario-1/baseline`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.026 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 76.749 |  |
| 2b | operation progress latency | PASS | 3/3 | 34.423 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 1.391 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 1794.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.026 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.000 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.024 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.861 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21577728.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.762 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.151 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 19.572 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21635072.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 1.493 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 30.055 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 51.501 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 69.805 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/netem/scenario-1/paused`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.006 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 66.602 |  |
| 2b | operation progress latency | PASS | 3/3 | 55.666 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3599.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 1.488 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 1794.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.008 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.000 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.007 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.912 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 4.724 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21626880.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.926 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.929 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 12.572 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21512192.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.653 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 31.650 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 48.737 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 63.548 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/netem/scenario-2/rate-100`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.043 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 61.343 |  |
| 2b | operation progress latency | PASS | 3/3 | 41.197 |  |
| 3a | audit cursor advances | PASS | 3/3 | 17998.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 0.062 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.111 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.155 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.207 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.665 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21696512.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 0.591 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 2.369 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -3.682 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21700608.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.825 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 29.309 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 50.716 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 63.612 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/netem/scenario-2/rate-20`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.024 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 67.073 |  |
| 2b | operation progress latency | PASS | 3/3 | 53.541 |  |
| 3a | audit cursor advances | PASS | 3/3 | 3600.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 1.611 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.103 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.155 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.198 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 1.738 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 4.220 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21704704.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.246 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.706 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | -28.736 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21573632.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.896 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 30.631 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 54.482 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 65.626 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

### `websocket/netem/scenario-2/rate-50`

| ID | Check | Result | Passes | Median | Failed observations |
|---|---|---|---:|---:|---|
| 1 | operation delay | PASS | 3/3 | 0.028 |  |
| 2a | cancel ACK latency | PASS | 3/3 | 58.400 |  |
| 2b | operation progress latency | PASS | 3/3 | 55.881 |  |
| 3a | audit cursor advances | PASS | 3/3 | 9000.000 |  |
| 3b | audit ACK stall | PASS | 3/3 | 1.352 |  |
| 4a | audit throughput | PASS | 3/3 | 0.000 |  |
| 4b | audit lag slope | PASS | 3/3 | 0.000 |  |
| 5 | stats latest-wins | PASS | 3/3 | 894.000 |  |
| 6.2 | log-2 isolation | PASS | 3/3 | 0.130 |  |
| 6.3 | log-3 isolation | PASS | 3/3 | 0.164 |  |
| 6.4 | log-4 isolation | PASS | 3/3 | 0.207 |  |
| 7.agent.anon.trend | agent cgroup anon memory trend | PASS | 3/3 | 2.074 |  |
| 7.agent.buffer.trend | agent bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.agent.heap.trend | agent Go heap after GC cycles trend | PASS | 3/3 | 0.000 |  |
| 7.agent.oom | agent cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.agent.rss | agent RSS | PASS | 3/3 | 21696512.000 |  |
| 7.agent.rss.trend | agent process RSS trend | PASS | 3/3 | 1.343 |  |
| 7.agent.websocket_buffer.trend | agent WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| 7.server.anon.trend | server cgroup anon memory trend | PASS | 3/3 | 1.636 |  |
| 7.server.buffer.trend | server bounded buffer memory trend | PASS | 3/3 | 0.000 |  |
| 7.server.heap.trend | server Go heap after GC cycles trend | PASS | 3/3 | 56.117 |  |
| 7.server.oom | server cgroup OOM | PASS | 3/3 | 0.000 |  |
| 7.server.rss | server RSS | PASS | 3/3 | 21389312.000 |  |
| 7.server.rss.trend | server process RSS trend | PASS | 3/3 | 0.752 |  |
| 7.server.websocket_buffer.trend | server WebSocket transport-buffer trend | PASS | 3/3 | 0.000 |  |
| diag.echo.p50 | query Echo RTT p50 | PASS | 3/3 | 29.727 |  |
| diag.echo.p95 | query Echo RTT p95 | PASS | 3/3 | 51.392 |  |
| diag.echo.p99 | query Echo RTT p99 | PASS | 3/3 | 60.725 |  |
| protocol | official timing and controls | PASS | 3/3 | 0.000 |  |
| protocol.clock | monotonic sampling axis | PASS | 3/3 | 0.000 |  |
| workload | workload and logical-contract integrity | PASS | 3/3 | 0.000 |  |

## A.10 tie-break

| Priority | Criterion | Candidate A | Candidate B | Favored |
|---:|---|---|---|---|
| 1 | adapter implementation size | 861 hand-written non-test Go lines | 843 hand-written non-test Go lines | B |
| 2 | hand-written correctness logic | HTTP/2 multiplexing, flow control and cancellation delegated to grpc-go | framing, five-class scheduler, per-stream byte/message credit and cancellation implemented locally | A |
| 3 | dependencies/license | 6 third-party build modules: grpc-go/protobuf + x/net,x/sys,x/text,genproto (Apache-2.0/BSD-3-Clause) | 1 third-party build module: coder/websocket (ISC); shared contract protobuf counted outside adapter | B |
| 4 | observability | standard channelz plus common metrics | custom scheduler/credit gauges plus common metrics | A |
| 5 | version negotiation/skew | shared handshake; protobuf/gRPC evolution | shared handshake; custom frame evolution | A |
| 6 | 20ms RTT + 1% loss degradation | median Echo p99 +45.559ms | median Echo p99 +57.463ms | A |
