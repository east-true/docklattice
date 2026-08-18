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
