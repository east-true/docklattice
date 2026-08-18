#!/bin/sh
set -eu

# Phase 9 release license material.
#
# Enumerates every third-party Go module that is actually linked into the
# release binary (./cmd/dockpilot), copies each module's license and notice
# texts out of the module cache, and writes a checksummed inventory. Output
# goes to dist/, which is a release output directory and is not committed:
# license texts are generated from the pinned go.sum versions at release time
# rather than vendored into the repository.
#
# The Container Agent image's separately bundled programs (Docker CLI, Docker
# Compose, Alpine base) are covered by distribution/IMAGE-LICENSES.md and the
# /licenses tree inside the image, not by this script.

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

command -v go >/dev/null 2>&1 || {
    printf 'license inventory failed: go toolchain is required\n' >&2
    exit 1
}

output=${1:-dist/licenses}
case "$output" in /*) ;; *) output="$repo_dir/$output" ;; esac
[ ! -e "$output" ] || {
    printf 'license inventory failed: refusing to overwrite %s\n' "$output" >&2
    exit 1
}

module=$(go list -m)
modcache=$(go env GOMODCACHE)
[ -d "$modcache" ] || {
    printf 'license inventory failed: module cache %s is missing\n' "$modcache" >&2
    exit 1
}

mkdir -p "$output"
inventory="$output/INVENTORY.tsv"
printf 'module\tversion\tfile\tsha256\n' >"$inventory"

# Modules linked into the release binary, excluding the standard library and
# this module itself.
modules=$(go list -deps -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./cmd/dockpilot |
    awk 'NF == 2' | sort -u | grep -v "^$module ")

missing=0
count=0
while read -r path version; do
    [ -n "$path" ] || continue
    escaped=$(printf '%s' "$path" | sed 's/\([A-Z]\)/!\1/g' | tr 'A-Z' 'a-z')
    dir="$modcache/$escaped@$version"
    if [ ! -d "$dir" ]; then
        printf 'license inventory: module source not in cache: %s@%s\n' "$path" "$version" >&2
        missing=$((missing + 1))
        continue
    fi
    found=0
    for name in LICENSE LICENSE.txt LICENSE.md LICENCE COPYING NOTICE NOTICE.txt; do
        [ -f "$dir/$name" ] || continue
        target="$output/$(printf '%s' "$path" | tr '/' '_')@$version.$name"
        cp "$dir/$name" "$target"
        chmod 0644 "$target"
        digest=$(sha256sum "$target" | cut -d' ' -f1)
        printf '%s\t%s\t%s\t%s\n' "$path" "$version" "$name" "$digest" >>"$inventory"
        found=1
    done
    if [ "$found" -eq 0 ]; then
        printf 'license inventory: no license file found for %s@%s\n' "$path" "$version" >&2
        missing=$((missing + 1))
        continue
    fi
    count=$((count + 1))
done <<EOF
$modules
EOF

if [ "$missing" -ne 0 ]; then
    printf 'license inventory failed: %d module(s) without recoverable license text\n' "$missing" >&2
    exit 1
fi

sha256sum "$inventory" | cut -d' ' -f1 >"$inventory.sha256"
printf 'license inventory: %d modules, %d files, %s\n' \
    "$count" "$(($(wc -l <"$inventory") - 1))" "$output"
