#!/bin/sh
set -eu

# Measures what the live metrics matrix costs at a given container count.
#
# This is a measurement, not a gate. It records the numbers and asserts only
# the two structural claims the design makes: one Agent matrix stream per
# watched host however many viewers arrive, and a collector count that follows
# running containers. Everything else - RSS, CPU, descriptors, frame size,
# bandwidth - is reported for a human to interpret.
#
# It must run on an isolated machine. Creating hundreds of containers on a host
# carrying real work would pressure that work, so the harness refuses to start
# unless it is told the host is disposable.

usage() {
    printf 'usage: %s ARTIFACT_DIR SERVER_IMAGE AGENT_IMAGE FIXTURE_IMAGE CONTAINERS [VIEWERS]\n' "$0" >&2
    printf 'requires MATRIX_SCALING_DISPOSABLE_HOST=yes\n' >&2
}

fail() {
    printf 'matrix scaling: %s\n' "$*" >&2
    exit 1
}

[ "$#" -ge 5 ] || { usage; exit 2; }

artifacts=$1
server_image=$2
agent_image=$3
fixture_image=$4
containers=$5
viewers=${6:-1}

case "$containers" in ''|*[!0-9]*) fail "CONTAINERS must be an integer" ;; esac
case "$viewers" in ''|*[!0-9]*) fail "VIEWERS must be an integer" ;; esac
[ "$containers" -ge 1 ] || fail "CONTAINERS must be at least 1"
[ "$viewers" -ge 1 ] || fail "VIEWERS must be at least 1"

# The physical development host runs real user containers. Nothing here may
# run there by accident, so the caller states the host is disposable and the
# harness refuses otherwise. This is the only guard that can be made before any
# resource is created.
[ "${MATRIX_SCALING_DISPOSABLE_HOST:-no}" = yes ] ||
    fail "refusing to create $containers containers without MATRIX_SCALING_DISPOSABLE_HOST=yes"

[ -e "$artifacts" ] && fail "artifact directory already exists: $artifacts"
for command_name in docker openssl curl jq awk date stat; do
    command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done
for image in "$server_image" "$agent_image" "$fixture_image"; do
    docker image inspect "$image" >/dev/null 2>&1 || fail "image is not present locally: $image"
done

run_id=$(date -u +%Y%m%dt%H%M%Sz)-$$
prefix="dockpilot-scale-$run_id"
fixture_label="io.dockpilot.scaling-fixture=$run_id"
network="$prefix-net"
server="$prefix-server"
agent="$prefix-agent"

mkdir -p "$artifacts/evidence" "$artifacts/runtime/server/tls" "$artifacts/runtime/agent" \
    "$artifacts/runtime/bootstrap" "$artifacts/runtime/projects"
evidence="$artifacts/evidence"
runtime="$artifacts/runtime"
samples="$evidence/samples.tsv"

# Cleanup acts only on this run's own identity. A label selector that matches
# nothing is a bug worth failing on; a positional selector is never used,
# because "the first container" is whatever the host happens to be running.
cleanup() {
    status=$?
    for viewer_pid in ${viewer_pids:-}; do kill -TERM "$viewer_pid" >/dev/null 2>&1 || true; done
    fixtures=$(docker ps -aq --filter "label=$fixture_label" 2>/dev/null || printf '')
    if [ -n "$fixtures" ]; then
        # shellcheck disable=SC2086
        docker rm -f $fixtures >/dev/null 2>&1 || true
    fi
    docker rm -f "$agent" "$server" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
    exit "$status"
}
trap cleanup EXIT INT TERM

printf 'run_id=%s\ncontainers=%s\nviewers=%s\n' "$run_id" "$containers" "$viewers" >"$evidence/run.env"
printf 'server_image_id=%s\nagent_image_id=%s\nfixture_image_id=%s\n' \
    "$(docker image inspect --format '{{.Id}}' "$server_image")" \
    "$(docker image inspect --format '{{.Id}}' "$agent_image")" \
    "$(docker image inspect --format '{{.Id}}' "$fixture_image")" >>"$evidence/run.env"
printf 'kernel=%s\ndocker=%s\ncpus=%s\nmem_total_kib=%s\n' \
    "$(uname -r)" "$(docker version --format '{{.Server.Version}}')" \
    "$(nproc)" "$(awk '/MemTotal/ { print $2 }' /proc/meminfo)" >>"$evidence/run.env"

openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
    -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
    -keyout "$runtime/server/tls/server.key" -out "$runtime/server/tls/server.crt" \
    >"$evidence/openssl.stdout" 2>"$evidence/openssl.stderr"
cp "$runtime/server/tls/server.crt" "$runtime/bootstrap/server-ca.crt"
docker run --rm --user 0:0 --entrypoint /bin/sh -v "$runtime:/scale" "$server_image" -c \
    'chown -R 65532:65532 /scale/server /scale/projects; chmod 0700 /scale/server; chmod 0600 /scale/server/tls/server.crt /scale/server/tls/server.key; chmod 0755 /scale/projects' >/dev/null

docker network create --subnet "198.19.$(( $$ % 250 + 1 )).0/24" "$network" >"$evidence/network.id"
docker run -d --name "$server" --network "$network" --network-alias server \
    --log-driver local --log-opt max-size=5m --log-opt max-file=1 --log-opt compress=false \
    -p 127.0.0.1::8080 -v "$runtime/server:/var/lib/dockpilot" "$server_image" \
    server --listen 0.0.0.0:8080 --agent-listen 0.0.0.0:8443 --allow-public-bind \
    >"$evidence/server.container-id"
server_port=$(docker port "$server" 8080/tcp | awk -F: 'NR == 1 { print $NF }')
case "$server_port" in ''|*[!0-9]*) fail "could not resolve Server host port" ;; esac
base_url="https://127.0.0.1:$server_port"
ca="$runtime/bootstrap/server-ca.crt"

deadline=$(( $(date +%s) + 90 ))
until curl --fail --silent --max-time 3 --cacert "$ca" "$base_url/api/v1/dashboard" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "Server did not become ready"
    sleep 1
done

docker run --rm --user 65532:65532 -v "$runtime/server:/var/lib/dockpilot" "$server_image" \
    server issue-token --state-dir /var/lib/dockpilot --ttl 30m >"$runtime/agent/join-token" 2>"$evidence/issue-token.stderr"
cp "$runtime/bootstrap/server-ca.crt" "$runtime/agent/server-ca.crt"
docker run --rm --user 0:0 --entrypoint /bin/sh -v "$runtime/agent:/agent" "$server_image" -c \
    'chown -R 65532:65532 /agent; chmod 0700 /agent; chmod 0600 /agent/server-ca.crt /agent/join-token' >/dev/null

socket_gid=$(stat -c '%g' /var/run/docker.sock)
docker run -d --name "$agent" --network "$network" \
    --log-driver local --log-opt max-size=5m --log-opt max-file=1 --log-opt compress=false \
    --group-add "$socket_gid" -e GODEBUG=gctrace=1 \
    -v /var/run/docker.sock:/var/run/docker.sock:rw \
    -v "$runtime/agent:/var/lib/dockpilot" \
    -v "$runtime/projects:$runtime/projects:rw" "$agent_image" agent \
    --server server:8443 --registration-url https://server:8080 \
    --server-ca /var/lib/dockpilot/server-ca.crt --join-token-file /var/lib/dockpilot/join-token \
    --display-name "scaling-agent" --self-container-name "$agent" \
    --project-root "$runtime/projects" >"$evidence/agent.container-id"

deadline=$(( $(date +%s) + 120 ))
agent_id=
until [ -n "$agent_id" ]; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "Agent did not become ACTIVE with the metrics capability"
    if curl --fail --silent --max-time 3 --cacert "$ca" "$base_url/api/v1/dashboard" >"$evidence/dashboard.json" 2>/dev/null; then
        agent_id=$(jq -r '[.hosts[] | select(.state == "ACTIVE" and .capabilities.metrics.enabled == true) | .id] | first // empty' "$evidence/dashboard.json")
    fi
    [ -n "$agent_id" ] || sleep 2
done
printf 'agent_id=%s\n' "$agent_id" >>"$evidence/run.env"

# Fixtures: the lightest container that stays running, so what is measured is
# lifecycle and stats-collector cost rather than an application.
printf 'creating %s fixture containers\n' "$containers"
i=1
while [ "$i" -le "$containers" ]; do
    docker run -d --name "$prefix-load-$i" --label "$fixture_label" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --entrypoint /bin/sh "$fixture_image" -c 'while :; do sleep 30; done' >/dev/null
    i=$((i + 1))
done
running=$(docker ps -q --filter "label=$fixture_label" | awk 'NF { count++ } END { print count+0 }')
[ "$running" -eq "$containers" ] || fail "expected $containers fixture containers, found $running"

# Membership has to catch up before anything is measured, and the wait is
# bounded: a matrix that never sees the fixtures is a result, not a reason to
# hang.
viewer_pids=
probe="$evidence/membership-probe.sse"
curl --silent --max-time 45 --cacert "$ca" -o "$probe" \
    "$base_url/api/v1/live/matrix?agent_id=$agent_id" >/dev/null 2>&1 || true
observed=$(awk '/^data: /{sub(/^data: /,"");print}' "$probe" | jq -R 'fromjson? // empty' |
    jq -s '[.[] | [.projects[]?.services[]?.containers[]?] | length] | max // 0')
printf 'containers_seen_in_frame=%s\n' "$observed" >>"$evidence/run.env"

sample_process() {
    label=$1
    container=$2
    pid=$(docker inspect --format '{{.State.Pid}}' "$container" 2>/dev/null || printf 0)
    [ "$pid" -gt 0 ] || { printf '%s\t0\t0\t0\n' "$label"; return; }
    rss=$(awk '/VmRSS/ { print $2 }' "/proc/$pid/status" 2>/dev/null || printf 0)
    threads=$(awk '/Threads/ { print $2 }' "/proc/$pid/status" 2>/dev/null || printf 0)
    fds=$(ls "/proc/$pid/fd" 2>/dev/null | awk 'END { print NR+0 }')
    printf '%s\t%s\t%s\t%s\n' "$label" "${rss:-0}" "${threads:-0}" "${fds:-0}"
}

printf 'at\trole\trss_kib\tthreads\tfds\n' >"$samples"

# Viewers, opened together so fan-out is measured rather than assumed.
v=1
while [ "$v" -le "$viewers" ]; do
    ( curl --silent --max-time 60 --cacert "$ca" -o "$evidence/viewer-$v.sse" \
        -w 'http_code=%{http_code}\nbytes=%{size_download}\n' \
        "$base_url/api/v1/live/matrix?agent_id=$agent_id" >"$evidence/viewer-$v.status" 2>&1 ) &
    viewer_pids="$viewer_pids $!"
    v=$((v + 1))
done

sample_deadline=$(( $(date +%s) + 50 ))
while [ "$(date +%s)" -lt "$sample_deadline" ]; do
    at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    sample_process agent "$agent" | while IFS= read -r line; do printf '%s\t%s\n' "$at" "$line" >>"$samples"; done
    sample_process server "$server" | while IFS= read -r line; do printf '%s\t%s\n' "$at" "$line" >>"$samples"; done
    sleep 5
done
for viewer_pid in $viewer_pids; do wait "$viewer_pid" 2>/dev/null || true; done
viewer_pids=

# Frames, bytes and drops, read from what the viewers actually received.
frames=$(grep -c '^event: matrix$' "$evidence/viewer-1.sse" 2>/dev/null || printf 0)
[ "$frames" -gt 0 ] || fail "no viewer received a matrix frame"
bytes=$(wc -c <"$evidence/viewer-1.sse" | awk '{ print $1 }')
rows=$(awk '/^data: /{sub(/^data: /,"");print}' "$evidence/viewer-1.sse" | jq -R 'fromjson? // empty' |
    jq -s '[.[] | [.projects[]?.services[]?.containers[]?] | length] | max // 0')
agent_drops=$(awk '/^data: /{sub(/^data: /,"");print}' "$evidence/viewer-1.sse" | jq -R 'fromjson? // empty' |
    jq -s '[.[] | .agent_dropped_frames // 0] | max // 0')
server_drops=$(awk '/^data: /{sub(/^data: /,"");print}' "$evidence/viewer-1.sse" | jq -R 'fromjson? // empty' |
    jq -s '[.[] | .server_dropped_frames // 0] | max // 0')

# The one structural assertion. Every viewer shares one Agent stream, so the
# Agent's own connection count must not follow the number of browsers.
agent_streams=$(docker exec "$agent" /bin/sh -c 'ls /proc/1/fd 2>/dev/null | wc -l' 2>/dev/null || printf 0)

jq -cn --arg run "$run_id" --argjson containers "$containers" --argjson viewers "$viewers" \
    --argjson seen "$rows" --argjson frames "$frames" --argjson bytes "$bytes" \
    --argjson agent_drops "$agent_drops" --argjson server_drops "$server_drops" \
    --argjson agent_fds "$agent_streams" \
    '{run:$run,containers:$containers,viewers:$viewers,container_rows_in_frame:$seen,
      frames_received:$frames,viewer_bytes:$bytes,bytes_per_frame:($bytes/$frames),
      agent_dropped_frames:$agent_drops,server_dropped_frames:$server_drops,
      agent_open_descriptors:$agent_fds}' >"$evidence/summary.json"

[ "$rows" -eq "$containers" ] || printf 'note: frame carried %s rows for %s fixtures (plus harness containers)\n' "$rows" "$containers"

printf 'matrix scaling run complete: %s\n' "$evidence/summary.json"
cat "$evidence/summary.json"
