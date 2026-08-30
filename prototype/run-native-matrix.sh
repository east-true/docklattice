#!/usr/bin/env bash
set -euo pipefail

# Native WSL/Linux runner for Appendix A.6-A.9. It uses user systemd scopes for
# separate cgroup-v2 limits and an unprivileged network namespace for netem.

repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
artifact_root=${1:-"$repo_dir/artifacts/transport-prototype/official"}
go_binary=${GO_BINARY:-$(command -v go 2>/dev/null || true)}
if [[ -z "$go_binary" && -x /home/leo/.local/go/bin/go ]]; then
  go_binary=/home/leo/.local/go/bin/go
fi
if [[ -z "$go_binary" ]]; then
  echo "Go toolchain not found; set GO_BINARY" >&2
  exit 1
fi
mkdir -p "$artifact_root"
artifact_root=$(realpath "$artifact_root")
cd "$repo_dir"

control_dir="$artifact_root/control"
mkdir -p "$control_dir"
binary="$control_dir/transport-prototype"
if [[ ${2:-} != --one && ! -x "$binary" ]]; then
  GOCACHE=${GOCACHE:-/tmp/docklattice-prototype-go-cache} \
  GOMODCACHE=${GOMODCACHE:-/tmp/docklattice-prototype-go-mod} \
  "$go_binary" build -trimpath -o "$binary" "$repo_dir/cmd/transport-prototype"
elif [[ ! -x "$binary" ]]; then
  echo "preserved prototype binary is missing: $binary" >&2
  exit 1
fi
if [[ -s "$control_dir/environment.txt" ]]; then
  expected_binary_sha=$(awk '$2 ~ /transport-prototype/ {print $1; exit}' "$control_dir/environment.txt")
  actual_binary_sha=$(sha256sum "$binary" | awk '{print $1}')
  if [[ -z "$expected_binary_sha" || "$actual_binary_sha" != "$expected_binary_sha" ]]; then
    echo "preserved prototype binary does not match the official control hash" >&2
    exit 1
  fi
fi

if [[ ${2:-} != --one && ( ! -s "$control_dir/prototype-cert.pem" || ! -s "$control_dir/prototype-key.pem" ) ]]; then
  "$binary" cert --output "$control_dir" --hosts 127.0.0.1,localhost,prototype-server >/dev/null
elif [[ ! -s "$control_dir/prototype-cert.pem" || ! -s "$control_dir/prototype-key.pem" ]]; then
  echo "shared prototype certificate is missing" >&2
  exit 1
fi
sha256sum "$control_dir/prototype-cert.pem" >"$control_dir/certificate.sha256"
if [[ ${2:-} != --one && ! -s "$control_dir/environment.txt" ]]; then
  {
    date --utc --iso-8601=seconds
    uname -a
    "$go_binary" version
    systemd-run --version | head -n 1
    tc -Version
    sha256sum "$binary"
  } >"$control_dir/environment.txt"
fi

children=()
cleanup() {
  for pid in "${children[@]:-}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
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

scope_run() {
  local role=$1 unit=$2 log=$3
  shift 3
  local memory=512M
  if [[ "$role" == server ]]; then
    memory=1G
  fi
  systemd-run --user --scope --unit "$unit" \
    -p "MemoryMax=$memory" -p MemorySwapMax=0 -p CPUQuota=100% \
    taskset --cpu-list 0 prlimit --nofile=4096:4096 -- env GOMAXPROCS=1 GOGC=100 "$@" >"$log" 2>&1 &
  children+=("$!")
  SCOPE_PID=$!
}

run_trial_inner() {
  local candidate=$1 condition=$2 scenario=$3 trial=$4 rate=$5 agents=$6 pause=$7 label=$8 baseline=${9:-}
  local run_dir="$artifact_root/$candidate/$condition/scenario-$scenario/$label/trial-$trial"
  mkdir -p "$run_dir"
  write_config "$run_dir/config.json" "$scenario" "$rate" "$agents" "$pause"

  local suffix="${candidate:0:2}-${condition:0:2}-s${scenario}-t${trial}-${rate}-${label}"
  suffix=${suffix//_/-}
  suffix=${suffix//[^a-zA-Z0-9-]/-}
  local server_log="$run_dir/server.log"
  scope_run server "dp-server-$suffix.scope" "$server_log" \
    "$binary" serve --candidate "$candidate" --listen 127.0.0.1:18443 \
    --cert "$control_dir/prototype-cert.pem" --key "$control_dir/prototype-key.pem" \
    --config "$run_dir/config.json" --output "$run_dir" \
    --network "$condition" --trial "$trial"
  local server_pid=$SCOPE_PID

  local ready=0
  for _ in $(seq 1 100); do
    if grep -q '^READY ' "$server_log" 2>/dev/null; then
      ready=1
      break
    fi
    if ! kill -0 "$server_pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  if [[ $ready -ne 1 ]]; then
    cat "$server_log" >&2 || true
    return 1
  fi

  local agent_pids=()
  for i in $(seq 1 "$agents"); do
    local padded agent_log
    padded=$(printf '%03d' "$i")
    agent_log="$run_dir/agent-${padded}.log"
    scope_run agent "dp-agent-$suffix-$padded.scope" "$agent_log" \
      "$binary" agent --candidate "$candidate" --endpoint 127.0.0.1:18443 \
      --ca "$control_dir/prototype-cert.pem" --config "$run_dir/config.json" \
      --raw "$run_dir/agent-${padded}.jsonl" --agent-id "agent-${padded}"
    agent_pids+=("$SCOPE_PID")
  done

  local server_status=0
  wait "$server_pid" || server_status=$?
  local agent_status=0
  for pid in "${agent_pids[@]}"; do
    wait "$pid" || agent_status=$?
  done
  children=()
  if [[ $server_status -ne 0 || $agent_status -ne 0 ]]; then
    return 1
  fi

  local report_args=(report --run "$run_dir" --output "$run_dir" --require-official=true)
  if [[ -n "$baseline" ]]; then
    report_args+=(--baseline "$baseline")
  fi
  "$binary" "${report_args[@]}" || true
}

run_trial() {
  local candidate=$1 condition=$2 scenario=$3 trial=$4 rate=$5 agents=$6 pause=$7 label=$8 baseline=${9:-}
  local run_dir="$artifact_root/$candidate/$condition/scenario-$scenario/$label/trial-$trial"
  if [[ -s "$run_dir/acceptance.json" ]]; then
    echo "SKIP completed trial: $candidate/$condition/scenario-$scenario/$label/trial-$trial"
    return
  fi
  if [[ "$condition" == netem && ${DOCKLATTICE_IN_NETNS:-0} != 1 ]]; then
    unshare --user --map-root-user --net env DOCKLATTICE_IN_NETNS=1 \
      "$0" "$artifact_root" --one "$candidate" "$condition" "$scenario" "$trial" "$rate" "$agents" "$pause" "$label" "$baseline"
    return
  fi
  if [[ "$condition" == netem ]]; then
    ip link set lo up
    # 10ms per direction gives an approximately 20ms RTT. Loss is per packet.
    tc qdisc replace dev lo root netem delay 10ms loss 1%
  fi
  local run_dir="$artifact_root/$candidate/$condition/scenario-$scenario/$label/trial-$trial"
  mkdir -p "$run_dir"
  if [[ "$condition" == netem ]]; then
    tc qdisc show dev lo >"$run_dir/network-control.txt"
  else
    printf '%s\n' 'loopback without netem' >"$run_dir/network-control.txt"
  fi
  run_trial_inner "$candidate" "$condition" "$scenario" "$trial" "$rate" "$agents" "$pause" "$label" "$baseline"
}

if [[ ${2:-} == --one ]]; then
  shift 2
  run_trial "$@"
  exit
fi

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

"$binary" aggregate --root "$artifact_root" --repo "$repo_dir"
echo "Raw measurements and reports: $artifact_root"
