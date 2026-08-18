#!/usr/bin/env bash
set -euo pipefail

# Executes ADR A.6-A.9 without shortening durations or relaxing limits.
# Expect roughly 16 hours on one machine. Set PROTOTYPE_IMAGE to reuse a
# prebuilt image; all other controls intentionally have fixed defaults.

repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
image=${PROTOTYPE_IMAGE:-dockpilot-transport-prototype:local}
artifact_root=${1:-"$repo_dir/artifacts/transport-prototype/official"}
mkdir -p "$artifact_root"

docker build -f "$repo_dir/prototype/Dockerfile" -t "$image" "$repo_dir"

control_dir="$artifact_root/control"
mkdir -p "$control_dir"
docker run --rm -v "$control_dir:/control" "$image" cert --output /control --hosts 127.0.0.1,localhost,prototype-server >/dev/null
sha256sum "$control_dir/prototype-cert.pem" >"$control_dir/certificate.sha256"
{
  date --utc --iso-8601=seconds
  uname -a
  docker version --format '{{.Client.Version}}/{{.Server.Version}}'
  docker image inspect --format '{{.Id}}' "$image"
} >"$control_dir/environment.txt"

server_name=""
agent_names=()
network_name=""

cleanup() {
  if [[ -n "$server_name" ]]; then
    docker rm -f "$server_name" >/dev/null 2>&1 || true
  fi
  for name in "${agent_names[@]:-}"; do
    docker rm -f "$name" >/dev/null 2>&1 || true
  done
  if [[ -n "$network_name" ]]; then
    docker network rm "$network_name" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

write_config() {
  local path=$1 scenario=$2 rate=$3 agents=$4 pause=$5
  local stats=6 logs=4
  if [[ $scenario -eq 3 ]]; then
    stats=1
    logs=1
  elif [[ $scenario -eq 4 ]]; then
    stats=0
    logs=0
  fi
  cat >"$path" <<JSON
{
  "scenario": $scenario,
  "time_scale": 1,
  "controlled_harness": true,
  "audit_records_per_second": $rate,
  "audit_payload_bytes": 512,
  "audit_mode": "managed-like",
  "agents": $agents,
  "pause_log_consumer": $pause,
  "echo_payload_bytes": 1024,
  "stats_targets": $stats,
  "log_streams": $logs,
  "log_bytes_per_second": 204800,
  "log_line_bytes": 200
}
JSON
}

run_trial() {
  local candidate=$1 condition=$2 scenario=$3 trial=$4 rate=$5 agents=$6 pause=$7 label=$8 baseline=${9:-}
  local run_dir="$artifact_root/$candidate/$condition/scenario-$scenario/$label/trial-$trial"
  mkdir -p "$run_dir"
  write_config "$run_dir/config.json" "$scenario" "$rate" "$agents" "$pause"

  local suffix="${candidate:0:2}-${condition:0:2}-s${scenario}-t${trial}-${rate}-${label}"
  suffix=${suffix//_/-}
  server_name="dps-${suffix}"
  agent_names=()
  network_name=""
  local server_network endpoint listen
  if [[ "$condition" == "loopback" ]]; then
    server_network=(--network host)
    printf '%s\n' 'loopback without netem' >"$run_dir/network-control.txt"
    endpoint="127.0.0.1:18443"
    listen="127.0.0.1:18443"
  else
    network_name="dpn-${suffix}"
    docker network create "$network_name" >/dev/null
    server_network=(--network "$network_name" --network-alias prototype-server --cap-add NET_ADMIN -e DOCKPILOT_NETEM=1 -e DOCKPILOT_NETEM_PROOF=/out/network-control-server.txt)
    endpoint="prototype-server:8443"
    listen="0.0.0.0:8443"
  fi

  docker run -d --name "$server_name" "${server_network[@]}" \
    --memory 1g --memory-swap 1g --cpus 1 --ulimit nofile=4096:4096 \
    -e GOMAXPROCS=1 -e GOGC=100 -v "$run_dir:/out" -v "$control_dir:/control:ro" "$image" serve \
    --candidate "$candidate" --listen "$listen" --cert /control/prototype-cert.pem \
    --key /control/prototype-key.pem --config /out/config.json --output /out \
    --network "$condition" --trial "$trial" >/dev/null

  local ready=0
  for _ in $(seq 1 60); do
    if docker logs "$server_name" 2>&1 | grep -q '^READY '; then
      ready=1
      break
    fi
    sleep 1
  done
  if [[ $ready -ne 1 ]]; then
    docker logs "$server_name"
    return 1
  fi

  for i in $(seq 1 "$agents"); do
    local padded name network_args
    padded=$(printf '%03d' "$i")
    name="dpa-${suffix}-${padded}"
    agent_names+=("$name")
    if [[ "$condition" == "loopback" ]]; then
      network_args=(--network host)
    else
      network_args=(--network "$network_name" --cap-add NET_ADMIN -e DOCKPILOT_NETEM=1 -e "DOCKPILOT_NETEM_PROOF=/out/network-control-agent-$padded.txt")
    fi
    docker run -d --name "$name" "${network_args[@]}" \
      --memory 512m --memory-swap 512m --cpus 1 --ulimit nofile=4096:4096 \
      -e GOMAXPROCS=1 -e GOGC=100 -v "$run_dir:/out" -v "$control_dir:/control:ro" "$image" agent \
      --candidate "$candidate" --endpoint "$endpoint" --server-name "$([[ "$condition" == "loopback" ]] && echo 127.0.0.1 || echo prototype-server)" \
      --ca /control/prototype-cert.pem --config /out/config.json \
      --raw "/out/agent-${padded}.jsonl" --agent-id "agent-${padded}" >/dev/null
  done

  local server_exit
  server_exit=$(docker wait "$server_name")
  docker logs "$server_name" >"$run_dir/server.log" 2>&1
  if [[ "$server_exit" != "0" ]]; then
    return "$server_exit"
  fi
  for name in "${agent_names[@]}"; do
    local agent_exit
    agent_exit=$(docker wait "$name")
    docker logs "$name" >>"$run_dir/agents.log" 2>&1
    if [[ "$agent_exit" != "0" ]]; then
      return "$agent_exit"
    fi
  done

  local relative_run=${run_dir#"$artifact_root"/}
  local report_args=(report --run "/artifacts/$relative_run" --output "/artifacts/$relative_run" --require-official=true)
  if [[ -n "$baseline" ]]; then
    report_args+=(--baseline "/artifacts/${baseline#"$artifact_root"/}")
  fi
  docker run --rm -v "$artifact_root:/artifacts" "$image" "${report_args[@]}" || true
  cleanup
  server_name=""
  agent_names=()
  network_name=""
}

for candidate in grpc websocket; do
  for condition in loopback netem; do
    for trial in 1 2 3; do
      baseline_dir="$artifact_root/$candidate/$condition/scenario-1/baseline/trial-$trial"
      run_trial "$candidate" "$condition" 1 "$trial" 20 1 false baseline
      run_trial "$candidate" "$condition" 1 "$trial" 20 1 true paused "$baseline_dir"
      for rate in 20 50 100; do
        run_trial "$candidate" "$condition" 2 "$trial" "$rate" 1 true "rate-$rate"
      done
    done
  done
  for trial in 1 2 3; do
    baseline_dir="$artifact_root/$candidate/loopback/scenario-3/baseline-1-agent/trial-$trial"
    run_trial "$candidate" loopback 3 "$trial" 5 1 false baseline-1-agent
    run_trial "$candidate" loopback 3 "$trial" 5 20 false scale "$baseline_dir"
    run_trial "$candidate" loopback 4 "$trial" 0 1 false cancellation
  done
done

docker run --rm -v "$artifact_root:/artifacts" -v "$repo_dir:/repo:ro" "$image" \
  aggregate --root /artifacts --repo /repo
echo "Raw measurements and reports: $artifact_root"
