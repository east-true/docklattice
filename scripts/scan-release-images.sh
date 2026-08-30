#!/bin/sh
set -eu

usage() {
    printf 'usage: %s ASSET_DIR SERVER_IMAGE SERVER_DIGEST AGENT_IMAGE AGENT_DIGEST\n' "$0" >&2
}

fail() {
    printf 'release image scan failed: %s\n' "$*" >&2
    exit 1
}

[ "$#" -eq 5 ] || {
    usage
    exit 2
}

asset_dir=$1
server_image=$2
server_digest=$3
agent_image=$4
agent_digest=$5
repo_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
ignore_file=$repo_dir/distribution/trivyignore.yaml

command -v trivy >/dev/null 2>&1 || fail "trivy is required"
[ -f "$ignore_file" ] || fail "missing release vulnerability policy: $ignore_file"

case "$asset_dir" in
    /*) ;;
    *) asset_dir=$(pwd)/$asset_dir ;;
esac
[ ! -e "$asset_dir" ] || fail "refusing to overwrite $asset_dir"

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

mkdir -p "$asset_dir"

generate_reports() {
    component=$1
    image=$2
    digest=$3
    platform=$4
    architecture=${platform#linux/}
    subject=$image@$digest
    sbom=$asset_dir/docklattice-$component-$architecture.cdx.json
    vulnerabilities=$asset_dir/docklattice-$component-$architecture.vulnerabilities.json

    trivy image \
        --platform "$platform" \
        --scanners vuln \
        --format cyclonedx \
        --output "$sbom" \
        "$subject"

    trivy image \
        --platform "$platform" \
        --scanners vuln \
        --format json \
        --output "$vulnerabilities" \
        "$subject"

}

gate_platform() {
    image=$1
    digest=$2
    platform=$3
    subject=$image@$digest

    # The saved JSON report is complete. This gate fails only for a HIGH or
    # CRITICAL finding for which the upstream distributor has a fix. Unfixed
    # findings remain visible in the retained and published report.
    trivy image \
        --platform "$platform" \
        --scanners vuln \
        --severity HIGH,CRITICAL \
        --ignore-unfixed \
        --ignorefile "$ignore_file" \
        --show-suppressed \
        --exit-code 1 \
        --format table \
        "$subject"
}

for platform in linux/amd64 linux/arm64; do
    generate_reports server "$server_image" "$server_digest" "$platform"
    generate_reports agent "$agent_image" "$agent_digest" "$platform"
done

for platform in linux/amd64 linux/arm64; do
    gate_platform "$server_image" "$server_digest" "$platform"
    gate_platform "$agent_image" "$agent_digest" "$platform"
done
