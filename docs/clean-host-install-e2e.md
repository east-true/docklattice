# Clean-host container installation E2E

Status: PASS

This is the Phase 9 fail-closed installation gate for a fresh Linux Docker
host. The status is `PASS` only while a successful evidence directory produced
by the harness on the intended clean-host platform is attached. A source review
or a successful image build does not change this status; re-running the release
images on a different platform requires a new evidence directory.

## Recorded execution

    started_at              2026-08-18T22:06:55Z
    finished_at             2026-08-18T22:07:08Z
    kernel                  Linux 6.18.33.2-microsoft-standard-WSL2 x86_64
    docker_server_version   29.7.2
    cgroup                  v2, systemd driver
    release_version         1.0.0
    release_revision        391ce67aa0d25c852c5ec90b13a7d42919a109b2
    compose_version         5.3.1
    server_image_id         sha256:9f7e22eff72849e313fc0cbb3512a9080d95207e219e08111977db2ae00ad259
    agent_image_id          sha256:4f720e298ab86e15ecd661c891c8cb4b54925365563b4279bc90f895a3473e1e
    fixture_image_id        sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

Recorded assertion results:

| Assertion | Result |
| --- | --- |
| `registration` | PASS |
| `project_discovery` | PASS |
| `live_dashboard` | PASS |
| `compose_operation` | PASS |
| `backup_create_list` | PASS |
| `identity_reconnect` | PASS |
| `network_downloads` | FORBIDDEN |
| `image_builds` | FORBIDDEN |
| `image_pushes` | FORBIDDEN |

`STATUS` recorded `status=PASS`, and the run left no container, network,
runtime root, or Join Token behind.

## Inputs and safety boundary

Run `scripts/run-clean-host-install-e2e.sh` with four arguments:

```sh
./scripts/run-clean-host-install-e2e.sh \
  /absolute/new/evidence-directory \
  sha256:<exact-local-server-image-id> \
  sha256:<exact-local-agent-image-id> \
  sha256:<exact-local-fixture-image-id>
```

All three images must already exist in the local Engine. Server and Agent must
be matching non-development production targets with the same release version
and full source revision. The fixture must contain `/bin/sh`. No image is built, pulled, pushed, or downloaded
by this gate; every run uses
`--pull never`, and mutable tags are rejected.

The host must provide a local Linux Docker Engine, cgroup v2 visible through
the host `/proc` and `/sys/fs/cgroup`, and a readable/writable
`/var/run/docker.sock`. `DOCKER_HOST`, remote daemons, Docker Desktop, rootless
nonstandard sockets, and socket proxies are outside this gate. The preflight
runs a small local cgroup probe. Docker absence or an invalid platform fails
before either runtime state or the evidence directory is created.

The evidence path must be absolute, must not exist, and is never overwritten.
Runtime and secrets live under a separate fresh absolute `mktemp` root. Server
and Agent state use UID/GID 65532 and mode 0700; the generated one-day test TLS
key and certificate use mode 0600. The one-time Join Token is emitted by the
production Server CLI into this runtime root, is never copied into evidence,
and is deleted immediately after registration.

## Exact assertions

The harness starts the exact Server and Agent image IDs on an isolated Docker
network and requires all of the following before reporting PASS:

1. HTTPS readiness with the generated CA, exactly one registered ACTIVE Agent,
   live connection/Docker/Compose/discovery capabilities, and exactly one
   writable discovered fixture project at the identical absolute bind path.
2. A `compose.up` operation accepted through the production HTTP API, polled
   through the authoritative operation endpoint to exact `success`, creating
   exactly one running Compose fixture from the requested immutable image ID.
3. `backup.create` for `compose.yaml`, successful operation polling, and an
   exact one-record manual backup list with a valid manifest digest.
4. Removal and recreation of the Agent container without a Join Token, using
   the same state root, followed by an ACTIVE reconnect with the identical
   Agent ID, project UID, and backup metadata.

Any missing capability, extra host/project/fixture, terminal operation failure,
timeout, malformed response, wrong image ID, or cleanup failure produces FAIL.
There are no warning-only success paths.

## Evidence and cleanup

API responses are capped at 1 MiB. Docker uses its bounded local log driver;
captured logs are tail-limited and byte-limited. The complete evidence tree is
capped at 16 MiB by default (configurable only within the harness's narrow
4–64 MiB range). `assertions.env`, JSON responses, bounded logs, checksums, and
the final `STATUS` are retained. The directory is made read-only after completion
or failure so a subsequent run cannot silently amend it.

On every exit the harness removes the Agent, Server, fixture containers,
Compose-created network, isolated control network, Agent credential/state,
Server database/identity/TLS state, project fixture, and any remaining Join
Token. `status=PASS` is written only after every exact assertion succeeds and
the separate runtime root is fully removed.

Validate the checked-in static contract without Docker:

```sh
./scripts/verify-clean-host-install-harness.sh
```
