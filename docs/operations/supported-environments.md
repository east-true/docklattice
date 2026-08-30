# Supported and unsupported environments

This is the v1 release-gate statement of where DockLattice is supported. It
restates architecture sections 3.1, 6, and 14 for operators; the architecture
record remains authoritative if the two ever disagree.

## Supported

**Host and kernel**

- Linux with a local Docker Engine.
- cgroup v2, readable through the host `/proc` and `/sys/fs/cgroup`. The Agent
  reports container resource state from the cgroup hierarchy, so cgroup v1 and
  hybrid hierarchies are not covered by the release gate.
- `linux/amd64` and `linux/arm64`. Both are built and published from the same
  source revision; `scripts/build-release-images.sh` produces one OCI archive
  per component containing both.

**Docker**

- The standard Docker socket at `/var/run/docker.sock`, mounted into the Agent
  container read-write, with the socket's numeric host GID added as a
  supplementary group. The Agent itself runs as UID/GID 65532.
- Docker Engine API 1.40 or newer (`internal/dockeradapter.MinimumAPIVersion`).
  The Agent negotiates the API version at start-up and reports the engine
  version it actually reached as a capability.
- Compose is not taken from the host. The Agent image bundles the exact plugin
  version DockLattice validated for this release (`io.docklattice.compose.version`
  on the Agent image), so host Compose presence, absence, or version does not
  affect behaviour.

**Discovery roots**

- Each discovery root must be an **identical absolute-path bind mount**:
  `/srv/stacks:/srv/stacks`. A remapped path fails the Path Identity
  Self-Check, and the affected root is demoted to read-only with
  `fs_write:false` and a capability reason.
- `:ro` roots are a **first-class supported mode**, not a degraded one. The
  Agent reports `fs_write:false`, and the UI disables editing for that root.
- `:rw` roots are supported when file editing and backup restore are intended.
  UID 65532 must hold the corresponding host filesystem access.
- The Agent state directory may be a named volume; discovery roots may not.

**Deployment shape**

- The Agent runs separately from the Compose projects it manages.
- Agent upgrades are performed from the host. Self-protection prevents
  DockLattice from replacing its own container; see
  [`install.md`](install.md).

## Not supported in v1

These are explicit declarations, not gaps awaiting a fix. Each is refused or
outside the tested boundary rather than silently degraded.

| Environment | Why |
|---|---|
| Rootless Docker's nonstandard socket path | Architecture 3.1 declares it out of scope for v1; the socket path and privilege model differ from the tested boundary. |
| `DOCKER_HOST`, TCP daemons, socket proxies | The Agent measures container cgroups through the host `/proc` and `/sys/fs/cgroup`; a remote or proxied daemon breaks that and the identical-path bind-mount contract. |
| Docker Desktop | Its containers' cgroups are not visible through the host hierarchy the Agent reads. |
| Hosts where running containers is prohibited by policy | The Agent is a container. |
| cgroup v1 or hybrid hierarchies | Resource observation and the release resource gate both assume cgroup v2. |
| Windows and macOS hosts | The Agent is a Linux container binding a Linux Docker socket. |
| Kubernetes and Swarm orchestration | Architecture section 18 classifies these as DO NOT BUILD. |
| mTLS, a private CA, certificate rotation | Architecture section 18 classifies these as DO NOT BUILD for v1. Server transport uses a single server certificate the operator provides. |

## Preflight

`scripts/run-clean-host-install-e2e.sh` fails closed on platform violations
before it creates either runtime state or an evidence directory. It rejects
`DOCKER_HOST`, remote daemons, Docker Desktop, nonstandard rootless sockets,
and socket proxies, and it runs a small local cgroup probe.

The same preflight guards the Phase 8 resource matrix
([`../release/resource-gate.md`](../release/resource-gate.md)), which is why both gates require a
local Linux Engine on cgroup v2.
