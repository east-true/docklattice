# Transport Prototype

This directory is the disposable harness for `docs/architecture.md` Appendix
A. It exercises only the transport-neutral contract, the two candidate
adapters, synthetic workloads, and the shared metrics/assertions.

Quick wiring check (not admissible acceptance evidence):

```sh
go build -o /tmp/transport-prototype ./cmd/transport-prototype
/tmp/transport-prototype local --candidate grpc --scenario 1 \
  --time-scale 0.01 --output /tmp/grpc-quick
/tmp/transport-prototype local --candidate websocket --scenario 1 \
  --time-scale 0.01 --output /tmp/websocket-quick
```

Official A.6-A.9 matrix:

```sh
./prototype/run-matrix.sh
```

When Docker is unavailable but user systemd and unprivileged network namespaces
are available (including WSL2), use the equivalent native runner:

```sh
./prototype/run-native-matrix.sh
```

It puts every process in a separate user systemd scope and uses `prlimit` for
the FD limit. The netem trials recursively enter one shared user/network
namespace, set loopback up, and apply 10ms one-way delay plus 1% packet loss;
the resulting RTT target is 20ms.

The matrix deliberately fixes `GOMAXPROCS=1`, a one-CPU bound (quota or
affinity), FD limit, cgroup v2
memory and swap limits, payload/rate settings, and three repetitions. Loopback
runs use host networking; impaired runs use a private network namespace. The
native loopback qdisc and both Docker endpoints use 10ms per direction; both
target approximately 20ms RTT with 1% packet loss in each direction. Agent and
Server run in separate containers so
their RSS, Go heap, goroutine, FD, and cgroup evidence is not conflated.

One TLS certificate is shared by every trial in a matrix. The `control`
directory records its checksum, the executable/image identity, toolchain and
kernel details; each native trial also records the effective qdisc state.

`--memory-swap` equals `--memory`; this represents `memory.swap.max=0` rather
than relying on Docker's ambiguous zero/default spelling. The raw JSONL is
sampled every second. Scaled runs are stamped `official_timing=false`, and the
report command rejects them when `--require-official=true`.

The full sequential matrix takes roughly 11.5 hours plus setup overhead. It includes
matched 1-Agent scale baselines and writes under
`artifacts/transport-prototype/official`; interruption leaves completed raw
trials intact. Rerunning skips a trial only when its `acceptance.json` is
present and non-empty; an interrupted trial without that report is replaced.

To resume on another Linux/WSL2 host before the official release asset exists,
copy the whole repository including `artifacts/transport-prototype/official`,
not just the source tree. After publication, clone the repository, download the
official release asset, verify it against
`artifacts/transport-prototype/official/RELEASE-ASSET.sha256`, and extract it at
the repository root. Then run the same command:

```sh
./prototype/run-native-matrix.sh artifacts/transport-prototype/official
```

The preserved official executable, TLS certificate, and environment record in
`official/control` are reused. The runner verifies the executable against the
SHA-256 recorded at the start of the matrix and refuses a mismatched resume.
The destination needs user systemd scopes, cgroup v2, `unshare`, `ip`, `tc`,
`taskset`, and `prlimit`. Moving hosts is recorded by the remaining trials'
raw cgroup/kernel evidence; for the cleanest identical-control comparison,
finish both candidates on the same host whenever possible.

The completed raw matrix is not committed to normal Git history. Package it as
the immutable release asset described in Appendix A.13 with:

```sh
./prototype/package-official-artifacts.sh
```

After the matrix, Appendix A.5's non-decisional reality check is run once with
an available Docker Engine:

```sh
./prototype/run-compose-smoke.sh
```

It executes a real `docker compose up` against a disposable container emitting
the simulated Operation cadence (120 seconds, 50 lines/second, 200-byte lines).
Its log and summary are stored under the official artifact root but are marked
`acceptance_input=false` and never affect the transport verdict.
