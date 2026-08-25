# syntax=docker/dockerfile:1.18.0@sha256:dabfc0969b935b2080555ace70ee69a5261af8a8f1b4df97b9e7fbcf6722eddf

# Every external image and remote file in this Dockerfile is immutable. Keep
# distribution/versions.env and scripts/verify-distribution.sh in sync when
# deliberately upgrading one of them.

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG SOURCE_DATE_EPOCH=0

# Pinned to the build host's platform: the toolchain cross-compiles to
# TARGETARCH with CGO disabled, so no target-architecture emulation is needed to
# produce the release binary.
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine3.24@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags='-s -w -buildid=' -o /out/dockpilot ./cmd/dockpilot

# The Docker Official Image is used only as an immutable source for the Docker
# CLI and its CA bundle. Buildx and its bundled Compose plugin are not copied.
FROM docker:29.7.2-cli-alpine3.24@sha256:000bb62ff495f986c9f5578eb67cc2cb98b91138eda81d7762d5371eb8a497fe AS docker-cli

# Compose is acquired separately so its exact release artifact checksum is
# visible and independently enforced for each supported architecture.
FROM scratch AS compose-amd64
ADD --checksum=sha256:c57ab918abd5b05ca7e7d0f275875dd1330a695074f309dc9eab1b49efafcd4b \
    https://github.com/docker/compose/releases/download/v5.5.0/docker-compose-linux-x86_64 /docker-compose

FROM scratch AS compose-arm64
ADD --checksum=sha256:ff42489f5a9b879d5d117c5ffea6defc27390b3286da8ad52cbc9c6ab5df590e \
    https://github.com/docker/compose/releases/download/v5.5.0/docker-compose-linux-aarch64 /docker-compose

ARG TARGETARCH
FROM compose-${TARGETARCH} AS compose

# The unprivileged account and the state directory skeleton are architecture
# independent: Alpine's /etc/passwd and /etc/group are identical across the
# architectures of one pinned image digest. Building them here and copying the
# result keeps the runtime stages free of any RUN, so a cross-architecture
# release build needs no target emulation at all.
FROM --platform=$BUILDPLATFORM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS rootfs
RUN addgroup -S -g 65532 dockpilot && \
    adduser -S -D -H -u 65532 -G dockpilot dockpilot && \
    mkdir -p /skel/state

# License text is architecture-independent, so this stage also runs natively.
FROM --platform=$BUILDPLATFORM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS licenses
RUN mkdir -p /licenses/docker-cli /licenses/docker-compose
ADD --checksum=sha256:2d81ea060825006fc8f3fe28aa5dc0ffeb80faf325b612c955229157b8c10dc0 \
    https://raw.githubusercontent.com/docker/cli/v29.7.2/LICENSE /licenses/docker-cli/LICENSE
ADD --checksum=sha256:a8c869fbda819afb8d80e0ac19bac52e766bc6c19cb38cf94f52d64c4be2aab6 \
    https://raw.githubusercontent.com/docker/cli/v29.7.2/NOTICE /licenses/docker-cli/NOTICE
ADD --checksum=sha256:58d1e17ffe5109a7ae296caafcadfdbe6a7d176f0bc4ab01e12a689b0499d8bd \
    https://raw.githubusercontent.com/docker/compose/v5.5.0/LICENSE /licenses/docker-compose/LICENSE
ADD --checksum=sha256:b7dca0a6b01fa7365e4892877a6321179ee343d72ee87a96cfc222141b99a1e6 \
    https://raw.githubusercontent.com/docker/compose/v5.5.0/NOTICE /licenses/docker-compose/NOTICE

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS server
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="Dockpilot Server" \
      org.opencontainers.image.description="Dockpilot central control plane" \
      org.opencontainers.image.source="https://github.com/east-true/dockpilot" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0" \
      io.dockpilot.role="server" \
      io.dockpilot.ports="8080/tcp,8443/tcp"
# The listening ports are recorded as a label rather than with EXPOSE. BuildKit
# renders the EXPOSE history entry from a parser node that embeds a heap
# address, so an image carrying EXPOSE gets a different config digest on every
# build and cannot satisfy the reproducible-build gate.
COPY --from=rootfs /etc/passwd /etc/group /etc/
COPY --from=rootfs --chown=65532:65532 --chmod=0700 /skel/state /var/lib/dockpilot
COPY --from=build --chmod=0555 /out/dockpilot /usr/local/bin/dockpilot
COPY LICENSE NOTICE /licenses/Dockpilot/
COPY distribution/IMAGE-LICENSES.md /licenses/README.md
USER 65532:65532
VOLUME ["/var/lib/dockpilot"]
ENTRYPOINT ["/usr/local/bin/dockpilot"]
CMD ["server", "--listen", "0.0.0.0:8080", "--agent-listen", "0.0.0.0:8443", "--allow-public-bind"]

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS agent
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="Dockpilot Agent" \
      org.opencontainers.image.description="Dockpilot Docker host Container Agent" \
      org.opencontainers.image.source="https://github.com/east-true/dockpilot" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0" \
      io.dockpilot.role="agent" \
      io.dockpilot.docker-cli.version="29.7.2" \
      io.dockpilot.compose.version="5.5.0"
COPY --from=rootfs /etc/passwd /etc/group /etc/
COPY --from=rootfs --chown=65532:65532 --chmod=0700 /skel/state /var/lib/dockpilot
COPY --from=rootfs --chown=65532:65532 --chmod=0700 /skel/state /var/lib/dockpilot/docker-config
COPY --from=build --chmod=0555 /out/dockpilot /usr/local/bin/dockpilot
COPY --from=docker-cli --chmod=0555 /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=compose --chmod=0555 /docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --from=licenses /licenses /licenses
COPY LICENSE NOTICE /licenses/Dockpilot/
COPY distribution/IMAGE-LICENSES.md /licenses/README.md
ENV HOME=/var/lib/dockpilot \
    DOCKER_CONFIG=/var/lib/dockpilot/docker-config
USER 65532:65532
VOLUME ["/var/lib/dockpilot"]
ENTRYPOINT ["/usr/local/bin/dockpilot"]
CMD ["agent"]
