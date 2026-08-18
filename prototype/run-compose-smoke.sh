#!/usr/bin/env bash
set -euo pipefail

# A.5 reality check. The transport decision never depends on this smoke result.
repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output=${1:-"$repo_dir/artifacts/transport-prototype/official/compose-smoke"}
docker_bin=${DOCKER_BIN:-docker}
duration=${COMPOSE_SMOKE_DURATION:-120s}
if [[ ! $duration =~ ^([1-9][0-9]*)s$ ]]; then
  echo "COMPOSE_SMOKE_DURATION must be a positive whole number of seconds (for example 120s)" >&2
  exit 2
fi
duration_seconds=${BASH_REMATCH[1]}
work=$(mktemp -d)
project="dockpilot-compose-smoke-$UID-$$"
image="dockpilot-compose-smoke:$UID-$$"
image_created=false
cleanup() {
  COMPOSE_SMOKE_IMAGE=$image "$docker_bin" compose \
    --project-name "$project" \
    --file "$repo_dir/prototype/compose-smoke/compose.yaml" \
    down --remove-orphans >/dev/null 2>&1 || true
  if [[ $image_created == true ]]; then
    "$docker_bin" image rm "$image" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT INT TERM
mkdir -p "$output"

CGO_ENABLED=0 go build -trimpath -o "$work/emitter" "$repo_dir/prototype/compose-smoke/emitter.go"
tar --owner=0 --group=0 -C "$work" -cf "$work/rootfs.tar" emitter
image_id=$(
  "$docker_bin" image import \
    --change 'ENTRYPOINT ["/emitter"]' \
    "$work/rootfs.tar" "$image"
)
image_created=true

started_ns=$(date +%s%N)
set +e
COMPOSE_SMOKE_DURATION=$duration COMPOSE_SMOKE_IMAGE=$image "$docker_bin" compose \
  --project-name "$project" \
  --file "$repo_dir/prototype/compose-smoke/compose.yaml" \
  up --abort-on-container-exit --exit-code-from operation-output \
  --force-recreate --no-color >"$output/compose.log" 2>&1
status=$?
set -e
finished_ns=$(date +%s%N)
COMPOSE_SMOKE_IMAGE=$image "$docker_bin" compose \
  --project-name "$project" \
  --file "$repo_dir/prototype/compose-smoke/compose.yaml" \
  down --remove-orphans >>"$output/compose.log" 2>&1 || true

expected_lines=$((50 * duration_seconds))
observed_lines=$(grep -c 'compose-smoke line=' "$output/compose.log" || true)
elapsed_ms=$(((finished_ns - started_ns) / 1000000))
complete=false
if [[ $status -eq 0 && $observed_lines -eq $expected_lines ]] && grep -q 'COMPOSE_SMOKE_COMPLETE' "$output/compose.log"; then
  complete=true
fi
docker_version=$($docker_bin version --format '{{.Client.Version}}/{{.Server.Version}}')
compose_version=$($docker_bin compose version --short)
cat >"$output/summary.json" <<JSON
{
  "command": "docker compose up",
  "docker_version": "$docker_version",
  "compose_version": "$compose_version",
  "image_id": "$image_id",
  "requested_duration": "$duration",
  "elapsed_ms": $elapsed_ms,
  "expected_lines": $expected_lines,
  "observed_lines": $observed_lines,
  "exit_status": $status,
  "complete": $complete,
  "acceptance_input": false
}
JSON
[[ $complete == true ]]
