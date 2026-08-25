#!/bin/sh
set -eu

usage() {
    printf '%s\n' \
        "usage: $0 ASSET_DIR VERSION REVISION SOURCE_DATE_EPOCH SERVER_IMAGE SERVER_DIGEST AGENT_IMAGE AGENT_DIGEST" >&2
}

fail() {
    printf 'release asset preparation failed: %s\n' "$*" >&2
    exit 1
}

[ "$#" -eq 8 ] || {
    usage
    exit 2
}

asset_dir=$1
version=$2
revision=$3
source_date_epoch=$4
server_image=$5
server_digest=$6
agent_image=$7
agent_digest=$8

repo_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
case "$asset_dir" in
    /*) ;;
    *) asset_dir=$(pwd)/$asset_dir ;;
esac

[ -d "$asset_dir" ] || fail "asset directory does not exist: $asset_dir"
command -v jq >/dev/null 2>&1 || fail "jq is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

case "$version" in
    ''|*[!0-9A-Za-z.+-]*) fail "VERSION contains unsupported characters" ;;
esac
case "$revision" in
    ''|*[!0-9a-f]*) fail "REVISION must be lowercase hexadecimal" ;;
esac
[ "${#revision}" -eq 40 ] || fail "REVISION must be a full Git object ID"
case "$source_date_epoch" in
    ''|*[!0-9]*) fail "SOURCE_DATE_EPOCH must be a non-negative integer" ;;
esac

validate_image() {
    image=$1
    digest=$2

    case "$image" in
        ghcr.io/*) ;;
        *) fail "Image must be a fully qualified GHCR name: $image" ;;
    esac
    case "$image" in
        *@*|*:*) fail "Image name must not include a tag or digest: $image" ;;
    esac
    case "$digest" in
        sha256:*) hex=${digest#sha256:} ;;
        *) fail "Image digest must begin with sha256:" ;;
    esac
    case "$hex" in
        ''|*[!0-9a-f]*) fail "Image digest must be lowercase hexadecimal" ;;
    esac
    [ "${#hex}" -eq 64 ] || fail "Image digest must contain 64 hexadecimal characters"
}

validate_image "$server_image" "$server_digest"
validate_image "$agent_image" "$agent_digest"

for component in server agent; do
    for architecture in amd64 arm64; do
        [ -s "$asset_dir/dockpilot-$component-$architecture.cdx.json" ] ||
            fail "missing $component/$architecture SBOM"
        [ -s "$asset_dir/dockpilot-$component-$architecture.vulnerabilities.json" ] ||
            fail "missing $component/$architecture vulnerability report"
    done
done

licenses_dir=$asset_dir/licenses
licenses_archive=$asset_dir/dockpilot-$version-go-licenses.tar.gz
manifest=$asset_dir/release-images.json
checksums=$asset_dir/SHA256SUMS
scan_policy=$asset_dir/trivyignore.yaml

for output in "$licenses_dir" "$licenses_archive" "$manifest" "$checksums" "$scan_policy"; do
    [ ! -e "$output" ] || fail "refusing to overwrite $output"
done

"$repo_dir/scripts/generate-license-inventory.sh" "$licenses_dir"
cp "$repo_dir/distribution/trivyignore.yaml" "$scan_policy"

tar \
    --sort=name \
    --mtime="@$source_date_epoch" \
    --owner=0 \
    --group=0 \
    --numeric-owner \
    -czf "$licenses_archive" \
    -C "$asset_dir" \
    licenses

jq -n \
    --arg version "$version" \
    --arg revision "$revision" \
    --arg server_image "$server_image" \
    --arg server_digest "$server_digest" \
    --arg agent_image "$agent_image" \
    --arg agent_digest "$agent_digest" \
    '{
        schema_version: 1,
        version: $version,
        revision: $revision,
        platforms: ["linux/amd64", "linux/arm64"],
        images: {
            server: {
                name: $server_image,
                digest: $server_digest,
                reference: ($server_image + "@" + $server_digest)
            },
            agent: {
                name: $agent_image,
                digest: $agent_digest,
                reference: ($agent_image + "@" + $agent_digest)
            }
        }
    }' >"$manifest"

(
    cd "$asset_dir"
    sha256sum \
        ./*.json \
        ./*.tar.gz \
        ./*.yaml >"$checksums"
)

rm -rf "$licenses_dir"
printf 'release assets prepared in %s\n' "$asset_dir"
