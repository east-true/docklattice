#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
official_dir="$repo_root/artifacts/transport-prototype/official"
release_dir="$repo_root/artifacts/transport-prototype/release"
asset_name="transport-prototype-official-2026-08-15.tar.gz"
asset_path="$release_dir/$asset_name"
checksum_path="$official_dir/RELEASE-ASSET.sha256"

if [[ ! -s "$official_dir/COMPLETION.md" || ! -s "$official_dir/final-report.json" ]]; then
  echo "official evidence is incomplete: completion marker or final report missing" >&2
  exit 1
fi

mkdir -p "$release_dir"

# Fixed metadata and gzip's timestamp-free mode make identical input produce
# an identical archive. The checksum manifest is excluded to avoid recursion.
tar \
  --sort=name \
  --mtime='2026-08-15 00:00:00Z' \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  --exclude='official/RELEASE-ASSET.sha256' \
  -C "$repo_root/artifacts/transport-prototype" \
  -cf - official | gzip -n > "$asset_path"

(
  cd "$release_dir"
  sha256sum "$asset_name"
) > "$checksum_path"

echo "created $asset_path"
echo "recorded $checksum_path"
