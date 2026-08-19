# v1 image distribution

This repository defines two production image targets in the root Dockerfile:
`server` and `agent`. Both run as numeric UID/GID 65532. The Agent additionally
contains Docker CLI 29.6.2 and Docker Compose 5.3.1; it does not contain a
daemon, Buildx, or a shell-command execution wrapper.

The exact image indexes, artifact checksums, and upstream license-file
checksums are recorded in `distribution/versions.env`. Run the local consistency
check before any build:

```sh
./scripts/verify-distribution.sh
```

For a release build, pass an immutable source revision and its commit time:

```sh
./scripts/build-release-images.sh 1.0.0 <full-git-commit> <commit-unix-time>
```

The local operator command for provisioning a one-time Agent registration
secret is documented in `docs/install-containers.md`; no unauthenticated token
issuance endpoint is exposed by either image.

The script creates multi-platform OCI archives for `linux/amd64` and
`linux/arm64` under `dist/`, normalizes file timestamps through BuildKit, and
creates and prints each archive's SHA-256 sidecar. It refuses to overwrite an
existing archive and never loads, tags, pushes, or publishes an image. Network
access is required only to fetch the digest-pinned images, Go modules,
checksum-pinned Compose binary, and checksum-pinned license texts.

## Recorded reproducible build

`scripts/build-release-images.sh 1.0.0 <revision> 1787059535` was executed twice
on the reference host, each run producing both targets for both platforms. The
two runs produced byte-identical archives:

    dockpilot-server-1.0.0.oci.tar  sha256:1a31d1a03a3e6cd352c8d4d0e835d6a4a4c784e5d38c8bde91e651c2049755b0
    dockpilot-agent-1.0.0.oci.tar   sha256:18b26942e220d215ba84955a9dd8defc00bb529fa7da930ea8c5982320ebeade

Each archive carries a two-entry manifest list:

| Archive | Platform | Manifest digest |
| --- | --- | --- |
| server | `linux/amd64` | `sha256:a99fe66cce196c4b1174980565cfbd8b322f85c4d5e383ee1b0643c8cf10a3b8` |
| server | `linux/arm64` | `sha256:e2f7f176b3583f6530e9cfd72728f8049b81ecc63abfc0c6d41f6d76f471ffa8` |
| agent | `linux/amd64` | `sha256:223036296b41a1c09e48774bb6634efbd79df27909bcecaf343c9d10fe70d48c` |
| agent | `linux/arm64` | `sha256:803781c5b7c10cfb5734d655ce172e35394d2b7ab7e19abff1aabbda6008fbdd` |

Two properties of the `Dockerfile` make this possible, and
`scripts/verify-distribution.sh` fails closed on either regressing:

- **No `EXPOSE`.** BuildKit renders the `EXPOSE` history entry from a parser
  node whose formatted value contains a heap address, so an image carrying
  `EXPOSE` receives a different config digest on every build even when every
  layer is identical. The Server's listening ports are recorded in the
  `io.dockpilot.ports` label instead.
- **No `RUN` in the `server` or `agent` stage.** The unprivileged account and
  the state-directory skeleton are produced in a `$BUILDPLATFORM` stage and
  copied in, and the Go toolchain cross-compiles to `TARGETARCH` with CGO
  disabled. The multi-platform build therefore executes no target-architecture
  binary and needs no `binfmt_misc`/QEMU handler on the build host.

The image build is one part of the Phase 9 gate, not evidence that the gate has
passed. Release automation must still attach the Go dependency notices, image
SBOMs, vulnerability results, tests, and clean-host E2E evidence described by
the implementation plan.

## Deliberate upgrades

Update `Dockerfile` and `distribution/versions.env` in the same change. Obtain
Compose binary hashes from the official release assets for both supported
architectures, obtain LICENSE/NOTICE from the matching signed source tag, and
resolve OCI *index* digests from the registry. Do not substitute a mutable tag,
a per-architecture manifest digest for an index digest, or a hash copied from a
third-party mirror.
