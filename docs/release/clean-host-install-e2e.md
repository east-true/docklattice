# Clean-host container installation E2E

Status: PASS

This is the Phase 9 fail-closed installation gate for a fresh Linux Docker
host. The status is `PASS` only while a successful evidence directory produced
by the harness on the intended clean-host platform is attached. A source review
or a successful image build does not change this status; re-running the release
images on a different platform requires a new evidence directory.

## What "clean host" means here

This gate asserts that the Agent discovers **exactly one** project and that it
is the fixture at the fixture's root, verified against the uid the Agent must
derive for that root, `sha256(agent_id || NUL || working directory)`. That
assertion is the contract, not an implementation detail: a fresh host is the
platform this procedure is documented for.

A host that already manages other Compose projects cannot satisfy it, and the
assertion is not relaxed so that such a host can pass. The harness now
distinguishes that case: when the dashboard shows projects besides the fixture,
the run records `status=SKIPPED_NOT_CLEAN` with the count, instead of reporting
a product failure for something the product did correctly. A development
machine that is running other stacks is a skip, not a pass and not a bug.

## Effective defaults contract

Before starting either product mode, the harness runs `dockpilot defaults`
inside the exact release Server Image and compares the complete JSON output
byte-for-byte with `distribution/v1-defaults.json`. This proves which defaults
the shipped binary contains rather than inferring them from source or tests.

The current-revision execution below records `defaults_config_dump=PASS` from
the Image itself. Its captured `v1-defaults.json` is covered by the evidence
checksum manifest.

## Recorded execution

    started_at              2026-08-19T12:15:53Z
    finished_at             2026-08-19T12:16:07Z
    kernel                  Linux 6.18.33.2-microsoft-standard-WSL2 x86_64
    docker_server_version   29.7.2
    cgroup                  v2, systemd driver
    release_version         1.0.0
    release_revision        f1d4087eb94921f07ce3c6fafddcbf0261314bf3
    compose_version         5.3.1
    server_image_id         sha256:44202ec0ffeddec84b6dba8711b8f2cc353e69f9f876e9c104afb6fe47887125
    agent_image_id          sha256:22492be1c6a6ad695521ac704ae550711cfceb57e8e6d1883eee9bed939b0e04
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

## Second recorded execution: a genuinely clean host

The first execution ran on a WSL2 developer machine that happened to have no
other Compose projects. This one ran on a disposable libvirt guest where
Dockpilot had never run, provisioned by
[`../../scripts/vm-lab-provision.sh`](../../scripts/vm-lab-provision.sh) with
Docker installed from Ubuntu's own repository and nothing else - which is what
this gate is defined for.

    started_at              2026-08-20T10:39:06Z
    finished_at             2026-08-20T10:39:16Z
    guest                   dp-vm-clean, Ubuntu 24.04
    kernel                  Linux 6.8.0-137-generic x86_64
    docker_server_version   29.1.3
    cgroup                  v2, systemd driver
    release_version         1.0.0
    release_revision        c6366b83dc31c712b58ace47fe384bffb15a2a32
    compose_version         5.3.1
    server_image_id         sha256:0c05818885eb56673b95608de83bb2b0ea7401ad8ed23c9018809ad87c4de6ee
    agent_image_id          sha256:0d221f24ed5cb744e9b3b785bdbdf738cb3b950827951b4856e09acb9fda99f2
    fixture_image_id        sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

Every assertion above recorded PASS, with `network_downloads`, `image_builds`
and `image_pushes` FORBIDDEN as before. The images were transferred to the guest
with `docker save`/`docker load` and their IDs were preserved exactly, so the
`--pull never` exact-ID contract held across the transfer.

An earlier attempt on the same guest recorded `SKIPPED_NOT_CLEAN` because a
previous gate had left one Compose project behind. That is the harness working:
the gate refuses to claim a clean-host result on a host that is not clean.

## Current-revision execution: effective defaults

This execution ran on the clean `dp-vm-scaling` Ubuntu 24.04 libvirt guest.
Both product Images were built from and labelled with the same committed
revision, transferred with `docker save`/`docker load`, and invoked by exact
Image ID.

    started_at              2026-08-25T02:26:12Z
    finished_at             2026-08-25T02:26:22Z
    kernel                  Linux 6.8.0-137-generic x86_64
    docker_server_version   29.1.3
    cgroup                  v2, systemd driver
    release_version         0.0.0-validation.20260825
    release_revision        818c534f0852fccfea243e10533e3e6fa8a4ed47
    compose_version         5.3.1
    server_image_id         sha256:df1b4374ae7a5b2a2766045f7fef54f787da1db52a0fefb2254eeb4c60bc19ad
    agent_image_id          sha256:628e6ed867004a1f1ee9eb988a13fbb1d81d48d3006bda38f3fb5dfbd5d837e0
    fixture_image_id        sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

`defaults_config_dump`, registration, project discovery, live dashboard,
Compose operation, backup create/list, and identity reconnect all recorded
`PASS`. Network downloads, Image builds, and Image pushes remained
`FORBIDDEN`. The evidence checksum manifest verified after transfer, and the
run left no Container or Compose project behind.

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
