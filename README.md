# Dockpilot

Dockpilot is a small internal-network control plane for viewing and operating
multiple Docker hosts from one web interface. Each host runs an outbound-only
Container Agent; the central Server owns registration, operation history, and
the canonical Audit archive.

> Dockpilot is not affiliated with or endorsed by Docker, Inc. Docker and the
> Docker logo are trademarks or registered trademarks of Docker, Inc.

## Status

Product v1 is complete. Every phase of `docs/implementation-plan.md` has
passed, including the Phase 9 release gate at revision `f1d4087`. Dockpilot uses
one Agent-initiated reverse gRPC connection with application-owned P0-P4
scheduling. The authoritative scope and invariants are in
`docs/architecture.md`.

v1 covers:

- typed provisional v1 operational defaults;
- runnable `dockpilot server` / `dockpilot agent` TLS processes and local
  one-time Join Token issuance;
- separate Server Identity State and SQLite operational storage;
- signed Agent credentials, renewal, archive binding, and durable revocation;
- verified Agent path identity, self-protection, discovery, Docker/Compose
  operations, safe files, configuration backup/restore, logs, and live stats;
- bounded Agent Audit WAL, Server canonical Audit archive, coverage/ACK sync,
  observed Docker events, and crash-safe Managed Operation Audit;
- embedded Server UI/API for dashboard, environment, operations, files,
  backups, logs, and live stats.

Unit, race, and static checks pass. Four gates passed against the release
images built from `f1d4087`: the production cgroup resource matrix over three
trials (`docs/resource-gate.md`), the clean-host container installation E2E
(`docs/clean-host-install-e2e.md`), the real-container recovery matrix covering
all three Server-side loss outcomes (`docs/recovery-matrix-e2e.md`), and a
reproducible `linux/amd64` + `linux/arm64` release build whose two independent
runs produced byte-identical archives (`docs/distribution.md`).

The Appendix A transport prototype and its synthetic workloads remain isolated
from product code.

## Development

The project requires the Go version declared in `go.mod`.

```sh
go test ./...
go test -race ./...
go build ./cmd/dockpilot
```

The official transport evidence is split between compact reports committed to
Git and a checksum-addressed release bundle. See
`artifacts/transport-prototype/official/README.md`.

Operator documentation:

- `docs/supported-environments.md` - where v1 is supported, and what is
  explicitly not supported;
- `docs/install-containers.md` - container preparation, privilege boundary, and
  host-driven Agent upgrade;
- `docs/recovery.md` - matched Server Identity/DB backup recovery and Agent
  re-enrollment;
- `docs/degraded-storage-recovery.md` - the `DEGRADED_STORAGE` operator
  procedure;
- `docs/resource-gate.md` - the production resource gate and its evidence
  contract;
- `docs/clean-host-install-e2e.md` - the clean-host installation gate.
- `docs/recovery-matrix-e2e.md` - the real-container recovery matrix gate.

Release-scope and harness contracts are checked statically:

```sh
./scripts/verify-release-scope.sh
./scripts/verify-distribution.sh
./scripts/verify-resource-harness.sh
./scripts/verify-product-resource-workload.sh
./scripts/verify-clean-host-install-harness.sh
./scripts/verify-recovery-matrix-harness.sh
```

## Scope

Dockpilot delegates runtime state and actions to Docker Engine and Docker
Compose. It does not aim to replace SSH, Kubernetes, CI systems, log/metric
platforms, or application-aware backup tools. Consult the architecture before
adding functionality; every feature is classified as CORE, OPTIONAL, FUTURE,
or DO NOT BUILD.

## License

Apache License 2.0.

The embedded frontend is first-party: `internal/webui/assets` is three
hand-written files with no third-party code and no external references, so it
carries no license material beyond Dockpilot's own.

Third-party license and NOTICE texts for the Go binary are generated at release
time from the pinned `go.sum` versions rather than vendored into the
repository:

```sh
./scripts/generate-license-inventory.sh
```

It covers every module actually linked into `./cmd/dockpilot`, writes a
checksummed `INVENTORY.tsv` under `dist/licenses/`, and fails closed if any
module has no recoverable license text. The Container Agent image's separately
bundled programs are covered by `distribution/IMAGE-LICENSES.md` and the
`/licenses` tree inside the image.
