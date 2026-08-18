#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
dockerfile="$repo_dir/Dockerfile"
lock="$repo_dir/distribution/versions.env"

# This file contains only repository-controlled KEY=VALUE entries.
# shellcheck disable=SC1090
. "$lock"

module_go_version=$(awk '$1 == "go" { print $2; exit }' "$repo_dir/go.mod")
if [ "$module_go_version" != "$GO_VERSION" ]; then
    printf 'distribution verification failed: go.mod requires %s, lock declares %s\n' \
        "$module_go_version" "$GO_VERSION" >&2
    exit 1
fi

require_literal() {
    literal=$1
    file=$2
    if ! grep -F -- "$literal" "$file" >/dev/null; then
        printf 'distribution verification failed: %s lacks %s\n' "$file" "$literal" >&2
        exit 1
    fi
}

require_literal "docker/dockerfile:${DOCKERFILE_FRONTEND_VERSION}@sha256:${DOCKERFILE_FRONTEND_SHA256}" "$dockerfile"
require_literal "${GO_BUILDER_IMAGE}@sha256:${GO_BUILDER_INDEX_SHA256}" "$dockerfile"
require_literal "alpine:${ALPINE_VERSION}@sha256:${ALPINE_INDEX_SHA256}" "$dockerfile"
require_literal "${DOCKER_CLI_IMAGE}@sha256:${DOCKER_CLI_IMAGE_INDEX_SHA256}" "$dockerfile"
require_literal "releases/download/v${COMPOSE_VERSION}/docker-compose-linux-x86_64" "$dockerfile"
require_literal "releases/download/v${COMPOSE_VERSION}/docker-compose-linux-aarch64" "$dockerfile"
require_literal "--checksum=sha256:${COMPOSE_LINUX_AMD64_SHA256}" "$dockerfile"
require_literal "--checksum=sha256:${COMPOSE_LINUX_ARM64_SHA256}" "$dockerfile"
require_literal "docker/cli/v${DOCKER_CLI_VERSION}/LICENSE" "$dockerfile"
require_literal "docker/cli/v${DOCKER_CLI_VERSION}/NOTICE" "$dockerfile"
require_literal "--checksum=sha256:${DOCKER_CLI_LICENSE_SHA256}" "$dockerfile"
require_literal "--checksum=sha256:${DOCKER_CLI_NOTICE_SHA256}" "$dockerfile"
require_literal "docker/compose/v${COMPOSE_VERSION}/LICENSE" "$dockerfile"
require_literal "docker/compose/v${COMPOSE_VERSION}/NOTICE" "$dockerfile"
require_literal "--checksum=sha256:${COMPOSE_LICENSE_SHA256}" "$dockerfile"
require_literal "--checksum=sha256:${COMPOSE_NOTICE_SHA256}" "$dockerfile"

if ! awk '
    $1 == "FROM" {
        image = $2
        # Skip any leading FROM flags such as --platform=$BUILDPLATFORM.
        for (i = 2; i <= NF && image ~ /^--/; i++) {
            image = $(i + 1)
        }
        if (image == "scratch" || image ~ /^compose-/ || image ~ /@sha256:[0-9a-f]{64}$/) {
            next
        }
        exit 1
    }
' "$dockerfile"; then
    printf 'distribution verification failed: an external FROM is not digest-pinned\n' >&2
    exit 1
fi

# The release images must stay byte-reproducible and cross-buildable without
# target-architecture emulation. EXPOSE makes the config digest vary between
# identical builds, because BuildKit renders its history entry from a parser
# node that embeds a heap address; a RUN in a runtime stage would force the
# multi-platform build to execute target binaries.
if grep -qE '^[[:space:]]*EXPOSE' "$dockerfile"; then
    printf 'distribution verification failed: EXPOSE makes the image config digest non-reproducible\n' >&2
    exit 1
fi
if ! awk '
    $1 == "FROM" { stage = ""; for (i = 1; i < NF; i++) if ($i == "AS") stage = $(i + 1) }
    $1 == "RUN" && (stage == "server" || stage == "agent") { exit 1 }
' "$dockerfile"; then
    printf 'distribution verification failed: a runtime stage runs a command and would need target emulation\n' >&2
    exit 1
fi

require_literal 'USER 65532:65532' "$dockerfile"
require_literal 'COPY --from=rootfs /etc/passwd /etc/group /etc/' "$dockerfile"
require_literal 'COPY --from=licenses /licenses /licenses' "$dockerfile"
require_literal "io.dockpilot.docker-cli.version=\"${DOCKER_CLI_VERSION}\"" "$dockerfile"
require_literal "io.dockpilot.compose.version=\"${COMPOSE_VERSION}\"" "$dockerfile"
require_literal "BundledComposeVersion: \"${COMPOSE_VERSION}\"" "$repo_dir/cmd/dockpilot/factories.go"

printf 'distribution metadata is internally consistent\n'
