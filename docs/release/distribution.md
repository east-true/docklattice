# v1 image distribution

This repository defines two production image targets in the root Dockerfile:
`server` and `agent`. Both run as numeric UID/GID 65532. The Agent additionally
contains Docker CLI 29.7.2 and Docker Compose 5.5.0; it does not contain a
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
secret is documented in [`../operations/install.md`](../operations/install.md); no unauthenticated token
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

    docklattice-server-1.0.0.oci.tar  sha256:71587ca09aa4373c735adf04013d4753480d607f4998b93ce24ece28a94f8754
    docklattice-agent-1.0.0.oci.tar   sha256:fb29e4c0ca5604de9c9f1bb2e88c90b3f9821bc623cb4900db5efb5ec31b09ce

Each archive carries a two-entry manifest list:

| Archive | Platform | Manifest digest |
| --- | --- | --- |
| server | `linux/amd64` | `sha256:26dc91b25c6e591987788205aa1bac8610aa0d144fbae60423e83a526d37a4d0` |
| server | `linux/arm64` | `sha256:030c5ef5f6e8ec9f3c59869f30a6e9ab009b1b78af6911e460d56a14ee95e1ef` |
| agent | `linux/amd64` | `sha256:3a8beee05fdd8d9666c9db0bc255a7ae5190d53d597cae1e5ee39e3f340c3f5d` |
| agent | `linux/arm64` | `sha256:de7f64d69133722ca63158627fab2e04a1b2c6e4fe1a37ee321f5add7f810033` |

Two properties of the `Dockerfile` make this possible, and
`scripts/verify-distribution.sh` fails closed on either regressing:

- **No `EXPOSE`.** BuildKit renders the `EXPOSE` history entry from a parser
  node whose formatted value contains a heap address, so an image carrying
  `EXPOSE` receives a different config digest on every build even when every
  layer is identical. The Server's listening ports are recorded in the
  `io.docklattice.ports` label instead.
- **No `RUN` in the `server` or `agent` stage.** The unprivileged account and
  the state-directory skeleton are produced in a `$BUILDPLATFORM` stage and
  copied in, and the Go toolchain cross-compiles to `TARGETARCH` with CGO
  disabled. The multi-platform build therefore executes no target-architecture
  binary and needs no `binfmt_misc`/QEMU handler on the build host.

The local Image build is one part of the Phase 9 gate, not evidence that the
gate has passed. [`.github/workflows/release.yml`](../../.github/workflows/release.yml)
is the publication path for an Image-bearing release. A new SemVer tag must be
reachable from `main`; the workflow first reuses the complete CI gate, then:

1. builds and pushes `server` and `agent` for `linux/amd64` and `linux/arm64`;
2. refuses to overwrite a GHCR version tag with a different digest or any
   already-published GitHub Release;
3. scans both architectures and blocks fixable HIGH/CRITICAL vulnerabilities;
4. signs each Image index digest with keyless Cosign;
5. pushes GitHub build-provenance attestations for those exact digests;
6. attaches per-architecture CycloneDX SBOMs, complete Trivy JSON reports, Go
   dependency license texts, the exact `trivyignore.yaml` policy,
   `release-images.json`, and `SHA256SUMS` to the immutable GitHub Release.

Publication is retry-safe. The complete GitHub Release is assembled as a draft
before either public GHCR version tag is created. A retry removes only an
incomplete draft, accepts an existing version tag only when it already points
to the newly rebuilt expected digest, and otherwise fails closed. The draft is
published only after both version tags have been created and verified.

The Docker build keeps inline BuildKit provenance and SBOM disabled so the
deterministic Image index remains the subject. Cosign signatures and GitHub
provenance are attached as OCI referrers and do not rewrite that digest.

Every third-party Action in the workflow is pinned to a full commit SHA. The
workflow uses only the tag run's short-lived `GITHUB_TOKEN` and OIDC identity;
it has no long-lived Registry or signing key secret. Verify the static contract
with:

```sh
./scripts/verify-release-publishing.sh
```

The blocking scan is not allowed to hide unreviewed findings. The complete
unfiltered Trivy JSON is always retained and attached. The only suppressions
accepted by the gate are the ten path-scoped, expiring entries in
[`distribution/trivyignore.yaml`](../../distribution/trivyignore.yaml): eight
Go stdlib findings in the newest official Docker CLI 29.7.2 binary and two
`x/mod` findings in the newest official Compose 5.5.0 binary. DockLattice copies
those signed upstream release artifacts rather than rebuilding them. All ten
exceptions expire on 2026-09-15 and must be removed or consciously renewed
against a fresh upstream-version review; they do not suppress the published
reports.

## Deliberate upgrades

Update `Dockerfile` and `distribution/versions.env` in the same change. Obtain
Compose binary hashes from the official release assets for both supported
architectures, obtain LICENSE/NOTICE from the matching signed source tag, and
resolve OCI *index* digests from the registry. Do not substitute a mutable tag,
a per-architecture manifest digest for an index digest, or a hash copied from a
third-party mirror.
