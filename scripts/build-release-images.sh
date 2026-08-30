#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
    printf 'usage: %s VERSION REVISION SOURCE_DATE_EPOCH\n' "$0" >&2
    exit 2
fi

version=$1
revision=$2
source_date_epoch=$3
case "$version" in
    ''|*[!A-Za-z0-9._+-]*)
        printf 'VERSION must use release-safe characters\n' >&2
        exit 2
        ;;
esac
case "$revision" in
    ''|*[!0-9a-f]*)
        printf 'REVISION must be a full lowercase Git object ID\n' >&2
        exit 2
        ;;
esac
if [ "${#revision}" -ne 40 ]; then
    printf 'REVISION must be a full 40-character Git object ID\n' >&2
    exit 2
fi
case "$source_date_epoch" in
    ''|*[!0-9]*)
        printf 'SOURCE_DATE_EPOCH must be a non-negative integer\n' >&2
        exit 2
        ;;
esac

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"
./scripts/verify-distribution.sh
command -v docker >/dev/null 2>&1 || {
    printf 'docker is required\n' >&2
    exit 1
}
docker buildx version >/dev/null

mkdir -p dist
for target in server agent; do
    output="dist/docklattice-${target}-${version}.oci.tar"
    if [ -e "$output" ] || [ -e "$output.sha256" ]; then
        printf 'refusing to overwrite existing release output: %s\n' "$output" >&2
        exit 1
    fi
    docker buildx build \
        --platform linux/amd64,linux/arm64 \
        --target "$target" \
        --build-arg "VERSION=$version" \
        --build-arg "REVISION=$revision" \
        --build-arg "SOURCE_DATE_EPOCH=$source_date_epoch" \
        --provenance=false \
        --output "type=oci,dest=$output,rewrite-timestamp=true" \
        .
    checksum=$(sha256sum "$output")
    printf '%s\n' "$checksum" >"$output.sha256"
    printf '%s\n' "$checksum"
done
