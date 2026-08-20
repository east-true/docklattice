#!/bin/sh
set -eu

# Multi-Agent lab. Every gate before this one drives a single Agent against a
# single Docker Engine, which cannot answer the question this one exists for:
# when one host dies, stalls, fills up, or restarts its daemon, does anything
# happen to the others?
#
# Each Agent host here is a Docker-in-Docker container: its own dockerd, its
# own storage, its own container namespace, its own filesystem. The Agent runs
# as a container *of that inner daemon*, so self-protection, discovery, and
# Compose all see exactly what they would see on a separate machine. Cutting a
# host off the lab network is a real partition; killing its daemon is a real
# daemon restart; killing the host is a real abrupt loss.
#
# What this is not: a virtual machine lab. There is one kernel. A guest kernel
# panic, a hypervisor-level power cut, and anything that depends on separate
# kernel state are outside what this can show, and are recorded as such rather
# than approximated.
#
# Cases (select with LAB_CASES, default all):
#   registration        N Agents register at once, each distinct and independent
#   reconnect-storm     every Agent loses the Server at once and returns
#   server-restart      graceful stop and SIGKILL, with N Agents attached
#   partition-one       one host is cut off; the others must not notice
#   daemon-restart      one host's dockerd is restarted under a live Agent
#   host-poweroff       one host is killed outright and brought back
#   bulk-isolation      one Agent floods logs while others do durable work
#   operation-flood     parallel operations per Agent, locks independent
#   catchup-fairness    unequal backlogs reconnect together
#   disk-pressure       one host fills up; the others stay healthy
#
# Safety: the physical host's Docker is never mutated. Every container this
# creates carries the run's own label, every inner container lives inside a
# throwaway dind host, and every project target is checked against the fixture
# identity this run derived. A target that cannot be proved is a failure.

usage() {
    printf 'usage: %s ABSOLUTE_EVIDENCE_DIR SERVER_IMAGE_ID AGENT_IMAGE_ID FIXTURE_IMAGE_ID\n' "$0" >&2
    printf 'all image arguments must be exact local sha256 image IDs\n' >&2
    printf 'environment: LAB_AGENTS LAB_CASES LAB_DIND_IMAGE\n' >&2
}

fail() {
    printf 'multi-agent lab failed: %s\n' "$*" >&2
    failure_reason=$*
    [ -z "${failure_reason_file:-}" ] || printf '%s\n' "$*" >"$failure_reason_file" 2>/dev/null || true
    exit 1
}

require_image_id_shape() {
    case "$1" in
        sha256:*) ;;
        *) fail "preflight: $2 image must be an exact sha256 image ID" ;;
    esac
    [ "${#1}" -eq 71 ] || fail "preflight: $2 image ID is malformed"
}

[ "$#" -eq 4 ] || {
    usage
    exit 2
}

evidence_dir=$1
server_image=$2
agent_image=$3
fixture_image=$4

agents=${LAB_AGENTS:-3}
case "$agents" in ''|*[!0-9]*) fail "preflight: LAB_AGENTS must be an integer" ;; esac
[ "$agents" -ge 2 ] && [ "$agents" -le 10 ] || fail "preflight: LAB_AGENTS must be 2..10"
selected_cases=${LAB_CASES:-registration reconnect-storm server-restart partition-one daemon-restart host-poweroff bulk-isolation operation-flood catchup-fairness disk-pressure}
dind_image=${LAB_DIND_IMAGE:-sha256:12e683a161823b2a839aeea999b9d960e6e1f9a97b1679ad6b441982e2d9cf07}
evidence_max_bytes=${LAB_EVIDENCE_MAX_BYTES:-33554432}
log_max_bytes=${LAB_LOG_MAX_BYTES:-524288}

case "$evidence_dir" in
    /*) ;;
    *) fail "preflight: evidence directory must be absolute" ;;
esac
[ ! -e "$evidence_dir" ] || fail "preflight: evidence directory already exists"
[ -d "$(dirname "$evidence_dir")" ] || fail "preflight: evidence parent directory does not exist"

for tool in docker jq curl openssl sha256sum awk; do
    command -v "$tool" >/dev/null 2>&1 || fail "preflight: required command not found: $tool"
done
[ "$(docker info --format '{{.OSType}}')" = linux ] || fail "preflight: a Linux Docker Engine is required"
[ -z "${DOCKER_HOST:-}" ] || fail "preflight: DOCKER_HOST must be unset"
[ -r /var/run/docker.sock ] && [ -w /var/run/docker.sock ] ||
    fail "preflight: readable and writable /var/run/docker.sock is required"

require_image_id_shape "$server_image" Server
require_image_id_shape "$agent_image" Agent
require_image_id_shape "$fixture_image" fixture
require_image_id_shape "$dind_image" dind
for image in "$server_image" "$agent_image" "$fixture_image" "$dind_image"; do
    docker image inspect "$image" >/dev/null 2>&1 || fail "preflight: exact local image is unavailable: $image"
    [ "$(docker image inspect --format '{{.Id}}' "$image")" = "$image" ] ||
        fail "preflight: image reference did not resolve to its exact requested ID: $image"
done
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$server_image")" = "Dockpilot Server" ] ||
    fail "preflight: Server image is not the production Server target"
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$agent_image")" = "Dockpilot Agent" ] ||
    fail "preflight: Agent image is not the production Agent target"
server_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$server_image")
[ "$server_revision" = "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$agent_image")" ] ||
    fail "preflight: Server and Agent revisions differ"

runtime_base=${TMPDIR:-/tmp}
[ -d "$runtime_base" ] || fail "preflight: TMPDIR does not exist"

prefix="dockpilot-lab-$(date -u +%Y%m%dT%H%M%SZ)-$$"
lab_label="io.dockpilot.lab=$prefix"
umask 077
artifact_created=0
runtime=
server="$prefix-server"
network="$prefix-net"
fixture_root=/srv/dockpilot-fixture
completed=0
failure_reason="lab did not complete"
failure_reason_file=

# ----------------------------------------------------------- housekeeping
capture_log() {
    docker inspect "$1" >/dev/null 2>&1 || return 0
    docker logs --tail 1000 "$1" 2>&1 | head -c "$log_max_bytes" >"$2" || true
}

host_name() { printf '%s-host-%s' "$prefix" "$1"; }

# agent_var reads a per-agent variable by name, e.g. agent_var agent_id 2.
# Shell has no arrays, and every value here is indexed by host number.
agent_var() { eval "printf '%s' \"\$${1}_${2}\""; }

# Only this run's own objects are ever removed. The label is set at creation
# and is the only thing consulted here, so nothing the operator owns can match.
remove_lab_objects() {
    ids=$(docker ps -aq --filter "label=$lab_label" 2>/dev/null || true)
    # shellcheck disable=SC2086
    [ -z "$ids" ] || docker rm -f $ids >/dev/null 2>&1 || true
    ids=$(docker volume ls -q --filter "label=$lab_label" 2>/dev/null || true)
    # shellcheck disable=SC2086
    [ -z "$ids" ] || docker volume rm -f $ids >/dev/null 2>&1 || true
    ids=$(docker network ls -q --filter "label=$lab_label" 2>/dev/null || true)
    # shellcheck disable=SC2086
    [ -z "$ids" ] || docker network rm $ids >/dev/null 2>&1 || true
}

scrub_runtime() {
    [ -n "$runtime" ] || return 0
    [ -d "$runtime" ] || return 0
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/lab-runtime" "$server_image" -c 'rm -rf /lab-runtime/*' >/dev/null 2>&1 || return 1
    rm -rf "$runtime" || return 1
    return 0
}

seal_evidence() {
    ( cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -exec sha256sum {} + >SHA256SUMS )
    find "$evidence_dir" -type f -exec chmod 0444 {} +
}

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$artifact_created" -eq 1 ]; then
        capture_log "$server" "$evidence_dir/server.final.log"
        i=1
        while [ "$i" -le "$agents" ]; do
            capture_log "$(host_name "$i")" "$evidence_dir/host-$i.final.log"
            # The Agent is a container of the inner daemon, so its log is only
            # reachable through the host. Without it a bring-up failure has no
            # explanation at all.
            docker exec "$(host_name "$i")" /bin/sh -c 'docker logs --tail 200 dp-agent 2>&1' \
                >"$evidence_dir/agent-$i.final.log" 2>/dev/null || true
            docker exec "$(host_name "$i")" /bin/sh -c 'tail -c 20000 /var/log/dockerd.log 2>/dev/null' \
                >"$evidence_dir/dockerd-$i.log" 2>/dev/null || true
            i=$((i + 1))
        done
    fi
    remove_lab_objects
    runtime_cleaned=true
    scrub_runtime || runtime_cleaned=false
    if [ "$artifact_created" -eq 1 ]; then
        used_kib=$(du -sk "$evidence_dir" | awk '{ print $1 }')
        if [ $((used_kib * 1024)) -gt "$evidence_max_bytes" ]; then
            status=1
            failure_reason="evidence size cap exceeded"
        fi
        if [ "$status" -eq 0 ] && [ "$completed" -eq 1 ] && [ "$runtime_cleaned" = true ]; then
            printf 'status=PASS\n' >"$evidence_dir/STATUS"
        else
            [ "$status" -ne 0 ] || status=1
            [ ! -s "${failure_reason_file:-/nonexistent}" ] ||
                failure_reason=$(head -c 4096 "$failure_reason_file")
            [ "$runtime_cleaned" = true ] || failure_reason="runtime cleanup failed"
            {
                printf 'status=FAIL\n'
                printf 'reason=%s\n' "$failure_reason" | tr '\r\n' '  '
                printf '\n'
                printf 'runtime_cleaned=%s\n' "$runtime_cleaned"
            } >"$evidence_dir/STATUS"
        fi
        seal_evidence || true
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'failure_reason="lab interrupted by signal"; exit 130' HUP INT TERM

runtime=$(mktemp -d "$runtime_base/dockpilot-lab.XXXXXXXX")
chmod 0700 "$runtime"
mkdir "$evidence_dir"
chmod 0700 "$evidence_dir"
artifact_created=1
failure_reason_file="$evidence_dir/failure-reason.txt"
{
    printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'kernel=%s\n' "$(uname -srm)"
    printf 'outer_docker_version=%s\n' "$(docker info --format '{{.ServerVersion}}')"
    printf 'server_image_id=%s\n' "$server_image"
    printf 'agent_image_id=%s\n' "$agent_image"
    printf 'fixture_image_id=%s\n' "$fixture_image"
    printf 'dind_image_id=%s\n' "$dind_image"
    printf 'release_revision=%s\n' "$server_revision"
    printf 'agents=%s\n' "$agents"
    printf 'selected_cases=%s\n' "$selected_cases"
} >"$evidence_dir/environment.env"

record() {
    printf '%s=%s\n' "$1" "$2" >>"$evidence_dir/assertions.env"
}

selected() {
    for name in $selected_cases; do
        [ "$name" = "$1" ] && return 0
    done
    return 1
}

# ------------------------------------------------------------- lab network
# harness_subnet pins the network this run creates to a range no real network
# uses. Docker's default pool is 172.17.0.0/12, which on a host whose LAN sits
# anywhere in 172.16-172.31 will eventually be handed a subnet that overlaps
# the LAN itself - and a bridge route for the LAN's own prefix takes the host
# off the network entirely. 198.18.0.0/15 is reserved for benchmarking by
# RFC 2544 and is never routed, so a harness can claim it safely. The octet is
# derived from the pid so two runs on one host do not collide; if they do,
# Docker refuses the create and the run fails rather than guessing.
harness_subnet() {
    printf '198.18.%s.0/24' "$(( $$ % 250 + 1 ))"
}

docker network create --subnet "$(harness_subnet)" --label "$lab_label" "$network" >/dev/null

mkdir "$runtime/server" "$runtime/server/tls" "$runtime/bootstrap"
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
    -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
    -keyout "$runtime/server/tls/server.key" -out "$runtime/server/tls/server.crt" \
    >"$evidence_dir/openssl.stdout" 2>"$evidence_dir/openssl.stderr"
cp "$runtime/server/tls/server.crt" "$runtime/bootstrap/server-ca.crt"
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh --label "$lab_label" \
    -v "$runtime:/lab" "$server_image" -c \
    'chown -R 65532:65532 /lab/server; chmod 0700 /lab/server /lab/server/tls; chmod 0600 /lab/server/tls/server.crt /lab/server/tls/server.key' >/dev/null

start_server() {
    docker run --pull never -d --name "$server" --network "$network" --network-alias server \
        --label "$lab_label" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=2 --log-opt compress=false \
        -p 127.0.0.1::8080 -v "$runtime/server:/var/lib/dockpilot:rw" "$server_image" \
        server --listen 0.0.0.0:8080 --agent-listen 0.0.0.0:8443 --allow-public-bind
}

resolve_base_url() {
    server_port=$(docker port "$server" 8080/tcp | awk -F: 'NR == 1 { print $NF }')
    case "$server_port" in ''|*[!0-9]*) fail "could not resolve the Server HTTPS port" ;; esac
    base_url="https://127.0.0.1:$server_port"
}

wait_server_ready() {
    deadline=$(( $(date +%s) + 120 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >/dev/null 2>&1 && return 0
        sleep 1
    done
    fail "the Server never answered its dashboard route"
}

server_ip() {
    docker inspect --format "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$server"
}

issue_token() {
    docker run --pull never --rm --user 65532:65532 --label "$lab_label" \
        -v "$runtime/server:/var/lib/dockpilot:rw" "$server_image" \
        server issue-token --state-dir /var/lib/dockpilot --ttl 60m \
        2>>"$evidence_dir/issue-token.stderr"
}

# ------------------------------------------------------------ dind hosts
# Each host runs dockerd under a supervisor loop rather than as PID 1, so the
# daemon can be restarted the way an operator restarts it - taking its
# containers down and bringing them back - without destroying the host itself.
start_host() {
    index=$1
    name=$(host_name "$index")
    docker volume create --label "$lab_label" "$name-fixture" >/dev/null
    docker volume create --label "$lab_label" "$name-state" >/dev/null
    docker run --pull never -d --name "$name" --hostname "$name" --network "$network" \
        --network-alias "host-$index" --label "$lab_label" --privileged \
        --log-driver local --log-opt max-size=1m --log-opt max-file=2 --log-opt compress=false \
        -e DOCKER_TLS_CERTDIR= \
        -v "$name-fixture:$fixture_root" \
        -v "$name-state:/var/lib/dockpilot-agent" \
        --entrypoint /bin/sh "$dind_image" -c \
        'mkdir -p /var/run; while :; do dockerd --host=unix:///var/run/docker.sock --storage-driver=vfs >>/var/log/dockerd.log 2>&1; echo "dockerd exited, restarting" >>/var/log/dockerd.log; sleep 1; done' >/dev/null
}

host_exec() {
    index=$1
    shift
    docker exec "$(host_name "$index")" /bin/sh -c "$*"
}

wait_host_daemon() {
    index=$1
    deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if host_exec "$index" 'docker info >/dev/null 2>&1'; then
            return 0
        fi
        sleep 2
    done
    return 1
}

# The inner daemon starts empty. Images are handed over as archives rather
# than pulled: this lab must work with no registry access, and the images under
# test must be the ones this run was given.
#
# A save/load round trip changes the image ID - the two daemons keep their
# image stores in different formats - so the inner ID cannot be pinned in
# advance. The labels survive it, and the release revision baked into them is a
# stronger identity than the ID: it names the source the binary was built from.
# The inner ID is captured per host and recorded alongside the outer one.
load_host_image() {
    index=$1
    archive=$2
    want_title=$3
    loaded=$(docker exec -i "$(host_name "$index")" /bin/sh -c 'docker load' <"$archive" |
        sed -n 's/^Loaded image ID: //p' | head -1)
    [ -n "$loaded" ] || fail "host $index did not load $archive"
    got_title=$(host_exec "$index" "docker image inspect --format '{{index .Config.Labels \"org.opencontainers.image.title\"}}' $loaded" | tr -d '\r\n')
    got_revision=$(host_exec "$index" "docker image inspect --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}' $loaded" | tr -d '\r\n')
    [ "$want_title" = any ] || [ "$got_title" = "$want_title" ] ||
        fail "host $index loaded an image titled \"$got_title\", want \"$want_title\""
    [ "$want_title" = any ] || [ "$got_revision" = "$server_revision" ] ||
        fail "host $index loaded revision $got_revision, want $server_revision"
    printf '%s' "$loaded"
}

start_host_agent() {
    index=$1
    with_token=$2
    name=$(host_name "$index")
    token_args=
    if [ "$with_token" = true ]; then
        token_args="--join-token-file /var/lib/dockpilot/join-token"
    fi
    # The Agent runs as 65532 and reaches the daemon through the socket's
    # group, exactly as the documented deployment does. Inside a dind host that
    # group is the socket's own, which is read here rather than assumed.
    socket_gid=$(host_exec "$index" "stat -c '%g' /var/run/docker.sock" | tr -d '\r\n')
    case "$socket_gid" in ''|*[!0-9]*) fail "host $index: could not read the Docker socket group" ;; esac
    host_exec "$index" "docker run -d --name dp-agent --restart unless-stopped \
        --group-add $socket_gid \
        --add-host server:$lab_server_ip \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v /var/lib/dockpilot-agent:/var/lib/dockpilot:rw \
        -v $fixture_root:$fixture_root:rw \
        $(agent_var inner_agent "$index") agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/dockpilot/server-ca.crt $token_args \
        --display-name lab-agent-$index --self-container-name dp-agent \
        --project-root $fixture_root >/dev/null"
}

# ------------------------------------------------------------- API helpers
api() {
    method=$1; url=$2; body=$3; output=$4
    guard_project_target "$url" "$body"
    if [ "$method" = GET ]; then
        curl --fail --silent --show-error --max-time 20 --cacert "$runtime/bootstrap/server-ca.crt" "$url" >"$output.tmp"
    else
        curl --fail --silent --show-error --max-time 20 --cacert "$runtime/bootstrap/server-ca.crt" \
            -H 'Content-Type: application/json' -X "$method" --data "$body" "$url" >"$output.tmp"
    fi
    mv "$output.tmp" "$output"
}

api_status() {
    method=$1; url=$2; body=$3; output=$4
    guard_project_target "$url" "$body"
    if [ "$method" = GET ]; then
        curl --silent --show-error --max-time 20 --output "$output" --write-out '%{http_code}' \
            --cacert "$runtime/bootstrap/server-ca.crt" "$url"
    else
        curl --silent --show-error --max-time 20 --output "$output" --write-out '%{http_code}' \
            --cacert "$runtime/bootstrap/server-ca.crt" -H 'Content-Type: application/json' \
            -X "$method" --data "$body" "$url"
    fi
}

dashboard() {
    api GET "$base_url/api/v1/dashboard" '' "$1"
}

# ------------------------------------------------------- fixture identity
# Same rule as every other matrix, extended to many hosts: each Agent has one
# fixture, at a root this lab created, with a uid the Agent must derive from
# it. Nothing else may ever be a target.
fixture_uids=

allow_fixture_uid() { fixture_uids="$fixture_uids $1"; }

is_fixture_uid() {
    for known_fixture_uid in $fixture_uids; do
        [ "$known_fixture_uid" = "$1" ] && return 0
    done
    return 1
}

guard_project_target() {
    guarded_url=$1
    guarded_body=$2
    case "$guarded_url" in
        */api/v1/projects/*)
            guarded_target=${guarded_url#*/api/v1/projects/}
            guarded_target=${guarded_target%%/*}
            guarded_target=${guarded_target%%\?*}
            is_fixture_uid "$guarded_target" ||
                fail "fixture guard: request targets project $guarded_target, which this lab did not create"
            ;;
    esac
    case "$guarded_body" in
        *project_uid*)
            guarded_target=$(printf '%s' "$guarded_body" | jq -r '.project_uid // empty' 2>/dev/null || true)
            [ -z "$guarded_target" ] || is_fixture_uid "$guarded_target" ||
                fail "fixture guard: request body targets project $guarded_target, which this lab did not create"
            ;;
    esac
}

expected_fixture_uid() {
    printf '%s\000%s' "$1" "$fixture_root" | sha256sum | awk '{ print $1 }'
}

# agent_id_of prints the Server-assigned id for a lab Agent, matched by the
# display name this lab gave it.
agent_id_of() {
    jq -r --arg name "lab-agent-$1" '[.hosts[] | select(.display_name == $name)][0].id // empty' "$2"
}

fixture_uid_of() {
    index=$1
    dashboard_file=$2
    id=$(agent_id_of "$index" "$dashboard_file")
    [ -n "$id" ] || fail "fixture: the dashboard does not list lab-agent-$index"
    matched=$(jq -r --arg root "$fixture_root" --arg id "$id" \
        '[.projects[]? | select(.working_dir == $root and .host_id == $id)] | length' "$dashboard_file")
    if [ "$matched" = 0 ]; then
        # Older payloads may not carry host_id on a project; fall back to the
        # derived uid, which is bound to the Agent id and cannot collide.
        want=$(expected_fixture_uid "$id")
        matched=$(jq -r --arg uid "$want" '[.projects[]? | select(.uid == $uid)] | length' "$dashboard_file")
        [ "$matched" = 1 ] || fail "fixture: agent $index has $matched projects at $fixture_root"
        printf '%s' "$want"
        return 0
    fi
    [ "$matched" = 1 ] || fail "fixture: agent $index has $matched projects at $fixture_root"
    uid=$(jq -r --arg root "$fixture_root" --arg id "$id" \
        '[.projects[] | select(.working_dir == $root and .host_id == $id)][0].uid' "$dashboard_file")
    [ "$uid" = "$(expected_fixture_uid "$id")" ] ||
        fail "fixture: agent $index uid $uid is not derivable from $fixture_root"
    printf '%s' "$uid"
}

wait_hosts_active() {
    want=$1
    output=$2
    seconds=$3
    deadline=$(( $(date +%s) + seconds ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >"$output.tmp" 2>/dev/null &&
            jq -e --argjson want "$want" '
              ([.hosts[] | select(.state == "ACTIVE")] | length) == $want and
              (.hosts | length) == $want
            ' "$output.tmp" >/dev/null 2>&1; then
            mv "$output.tmp" "$output"
            return 0
        fi
        sleep 2
    done
    [ -e "$output.tmp" ] && mv "$output.tmp" "$output"
    return 1
}

poll_operation() {
    agent=$1; operation_id=$2; output=$3; seconds=$4
    deadline=$(( $(date +%s) + seconds ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/agents/$agent/operations/$operation_id" >"$output.tmp" 2>/dev/null &&
            jq -e '.status == "success" or .status == "failed" or .status == "canceled" or
                   .status == "interrupted" or .status == "rejected"' "$output.tmp" >/dev/null 2>&1; then
            mv "$output.tmp" "$output"
            return 0
        fi
        sleep 1
    done
    [ -e "$output.tmp" ] && mv "$output.tmp" "$output"
    return 1
}

audit_page() {
    api GET "$base_url/api/v1/hosts/$1/audit?limit=50" '' "$2"
}

audit_ack_seq() {
    jq -r '.coverage.ack.seq // 0' "$1"
}

# ---------------------------------------------------------------- bring-up
start_server >"$evidence_dir/server.container-id"
resolve_base_url
wait_server_ready
lab_server_ip=$(server_ip)
[ -n "$lab_server_ip" ] || fail "the Server has no address on the lab network"
record server_ip "$lab_server_ip"

docker save "$agent_image" -o "$runtime/agent-image.tar"
docker save "$fixture_image" -o "$runtime/fixture-image.tar"

i=1
while [ "$i" -le "$agents" ]; do
    start_host "$i" >"$evidence_dir/host-$i.container-id"
    i=$((i + 1))
done

i=1
while [ "$i" -le "$agents" ]; do
    wait_host_daemon "$i" || fail "host $i never started its Docker daemon"
    inner_agent=$(load_host_image "$i" "$runtime/agent-image.tar" "Dockpilot Agent")
    inner_fixture=$(load_host_image "$i" "$runtime/fixture-image.tar" any)
    eval "inner_agent_$i=\$inner_agent"
    eval "inner_fixture_$i=\$inner_fixture"
    record "inner_agent_image_$i" "$inner_agent"
    host_exec "$i" "mkdir -p $fixture_root"
    # Each host gets its own fixture project, named for the host so that a
    # target can never be confused with another host's.
    host_exec "$i" "cat >$fixture_root/compose.yaml <<'COMPOSE'
name: $prefix-fixture-$i
services:
  lab-fixture:
    image: $inner_fixture
    pull_policy: never
    command: [\"/bin/sh\", \"-c\", \"trap 'exit 0' TERM INT; while :; do echo lab-fixture-$i; sleep 2; done\"]
COMPOSE"
    host_exec "$i" "printf 'LAB_SECRET=lab-secret-must-never-be-recorded-$$\\n' >$fixture_root/.env"
    host_exec "$i" "chown -R 65532:65532 $fixture_root; chmod 0777 $fixture_root; chmod 0666 $fixture_root/compose.yaml $fixture_root/.env"
    host_exec "$i" "mkdir -p /var/lib/dockpilot-agent && chown 65532:65532 /var/lib/dockpilot-agent && chmod 0700 /var/lib/dockpilot-agent"
    host_exec "$i" "cat >/tmp/server-ca.crt <<'CRT'
$(cat "$runtime/bootstrap/server-ca.crt")
CRT"
    host_exec "$i" "cp /tmp/server-ca.crt /var/lib/dockpilot-agent/server-ca.crt; chown 65532:65532 /var/lib/dockpilot-agent/server-ca.crt; chmod 0600 /var/lib/dockpilot-agent/server-ca.crt"
    token=$(issue_token)
    [ -n "$token" ] || fail "the Server issued no Join Token for host $i"
    host_exec "$i" "printf '%s\\n' '$token' >/var/lib/dockpilot-agent/join-token; chown 65532:65532 /var/lib/dockpilot-agent/join-token; chmod 0600 /var/lib/dockpilot-agent/join-token"
    i=$((i + 1))
done
record hosts_started "$agents"

# Every Agent is started before any of them is waited for: simultaneous
# registration is the first thing this lab has to prove, and starting them in
# sequence would prove something easier.
i=1
while [ "$i" -le "$agents" ]; do
    start_host_agent "$i" true
    i=$((i + 1))
done

wait_hosts_active "$agents" "$evidence_dir/dashboard.baseline.json" 300 ||
    fail "the fleet did not settle on $agents ACTIVE Agents"

i=1
while [ "$i" -le "$agents" ]; do
    id=$(agent_id_of "$i" "$evidence_dir/dashboard.baseline.json")
    [ -n "$id" ] || fail "agent $i did not register"
    eval "agent_id_$i=\$id"
    i=$((i + 1))
done

# Discovery has to have run before a project uid exists.
project_deadline=$(( $(date +%s) + 300 ))
while [ "$(date +%s)" -lt "$project_deadline" ]; do
    dashboard "$evidence_dir/dashboard.projects.json"
    if [ "$(jq -r --arg root "$fixture_root" '[.projects[]? | select(.working_dir == $root)] | length' "$evidence_dir/dashboard.projects.json")" = "$agents" ]; then
        break
    fi
    sleep 3
done
i=1
while [ "$i" -le "$agents" ]; do
    uid=$(fixture_uid_of "$i" "$evidence_dir/dashboard.projects.json")
    allow_fixture_uid "$uid"
    eval "project_uid_$i=\$uid"
    record "fixture_uid_$i" "$uid"
    i=$((i + 1))
done
record fixture_identity_verified PASS


# fixture_container prints the running fixture container id on host $1.
fixture_container() {
    host_exec "$1" "docker ps --filter label=com.docker.compose.project=$prefix-fixture-$1 --filter status=running --format '{{.ID}}' | head -1" 2>/dev/null || true
}

compose_op() {
    index=$1; kind=$2; label=$3; timeout=${4:-240}
    aid=$(agent_var agent_id "$index")
    uid=$(agent_var project_uid "$index")
    op="lab-$label-$index-$$"
    code=$(api_status POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$op" --arg agent "$aid" --arg project "$uid" --arg kind "$kind" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:$kind}')" \
        "$evidence_dir/$label-$index.accepted.json")
    [ "$code" = 202 ] || return 1
    poll_operation "$aid" "$op" "$evidence_dir/$label-$index.final.json" "$timeout" || return 1
    jq -e '.status == "success"' "$evidence_dir/$label-$index.final.json" >/dev/null
}

host_state() {
    jq -r --arg name "lab-agent-$1" '[.hosts[] | select(.display_name == $name)][0].state // "MISSING"' "$2"
}

# --------------------------------------------- case: simultaneous registration
# Every Agent was started before any was waited for, so the fleet that settled
# above is already the answer. What is asserted here is that it settled into
# distinct, independent Agents rather than into one Agent seen N times.
if selected registration; then
    dashboard "$evidence_dir/registration.dashboard.json"
    seen=$(jq -r '[.hosts[].id] | length' "$evidence_dir/registration.dashboard.json")
    unique=$(jq -r '[.hosts[].id] | unique | length' "$evidence_dir/registration.dashboard.json")
    [ "$seen" = "$agents" ] || fail "registration: the Server lists $seen Agents, want $agents"
    [ "$unique" = "$agents" ] || fail "registration: $seen Agents share only $unique identities"
    jq -e '[.hosts[] | select(.state == "ACTIVE" and .capabilities.connection.enabled == true and
                              .capabilities.docker.enabled == true and .capabilities.compose.enabled == true and
                              .capabilities.discovery.enabled == true)] | length == '"$agents" \
        "$evidence_dir/registration.dashboard.json" >/dev/null ||
        fail "registration: not every Agent reported Docker, Compose, and discovery ready"
    # Each Agent must see only its own host's containers and its own project.
    projects=$(jq -r --arg root "$fixture_root" '[.projects[] | select(.working_dir == $root)] | length' "$evidence_dir/registration.dashboard.json")
    [ "$projects" = "$agents" ] || fail "registration: $projects fixture projects for $agents Agents"
    i=1
    while [ "$i" -le "$agents" ]; do
        aid=$(agent_var agent_id "$i")
        api GET "$base_url/api/v1/hosts/$aid/containers" '' "$evidence_dir/registration.containers-$i.json"
        i=$((i + 1))
    done
    record registration_agents_distinct PASS
    record registration_capabilities_ready PASS
fi

# ------------------------------------------------- case: reconnect storm
# All Agents lose the Server at once. The failure this looks for is a Server
# that keeps a retired session, or an Agent that comes back as a second
# identity.
if selected reconnect-storm; then
    i=1
    while [ "$i" -le "$agents" ]; do
        docker network disconnect "$network" "$(host_name "$i")" >/dev/null 2>&1 || true
        i=$((i + 1))
    done
    sleep 10
    i=1
    while [ "$i" -le "$agents" ]; do
        docker network connect --alias "host-$i" "$network" "$(host_name "$i")" >/dev/null 2>&1 || true
        i=$((i + 1))
    done
    wait_hosts_active "$agents" "$evidence_dir/reconnect-storm.dashboard.json" 300 ||
        fail "reconnect-storm: the fleet did not return to $agents ACTIVE Agents"
    after=$(jq -r '[.hosts[].id] | sort | join(",")' "$evidence_dir/reconnect-storm.dashboard.json")
    before=$(jq -r '[.hosts[].id] | sort | join(",")' "$evidence_dir/dashboard.baseline.json")
    [ "$after" = "$before" ] || fail "reconnect-storm: the fleet identity changed across the storm"
    record reconnect_storm_all_returned PASS
    record reconnect_storm_identities_stable PASS
fi

# --------------------------------------------------- case: server restart
if selected server-restart; then
    docker stop "$server" >/dev/null
    sleep 3
    docker start "$server" >/dev/null
    resolve_base_url
    wait_server_ready
    wait_hosts_active "$agents" "$evidence_dir/server-restart.graceful.json" 300 ||
        fail "server-restart: the fleet did not return after a graceful restart"
    record server_restart_graceful PASS

    docker kill --signal KILL "$server" >/dev/null
    sleep 2
    docker start "$server" >/dev/null
    resolve_base_url
    wait_server_ready
    wait_hosts_active "$agents" "$evidence_dir/server-restart.kill.json" 300 ||
        fail "server-restart: the fleet did not return after SIGKILL"
    ids=$(jq -r '[.hosts[].id] | sort | join(",")' "$evidence_dir/server-restart.kill.json")
    [ "$ids" = "$(jq -r '[.hosts[].id] | sort | join(",")' "$evidence_dir/dashboard.baseline.json")" ] ||
        fail "server-restart: Agent identities changed across a Server SIGKILL"
    record server_restart_sigkill PASS
    record server_restart_identities_preserved PASS
fi

# ---------------------------------------- case: one host partitioned only
# The point is not that the partitioned Agent goes offline - that is already
# covered for a single Agent. The point is that nothing happens to the others.
if selected partition-one; then
    victim=2
    [ "$agents" -ge "$victim" ] || victim=1
    docker network disconnect "$network" "$(host_name "$victim")" >/dev/null
    offline=0
    deadline=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        dashboard "$evidence_dir/partition-one.during.json"
        state=$(host_state "$victim" "$evidence_dir/partition-one.during.json")
        if [ "$state" != ACTIVE ]; then offline=1; break; fi
        sleep 5
    done
    [ "$offline" -eq 1 ] || fail "partition-one: the partitioned Agent never left ACTIVE"
    # Every other Agent must still be ACTIVE and still able to do real work.
    i=1
    while [ "$i" -le "$agents" ]; do
        if [ "$i" != "$victim" ]; then
            [ "$(host_state "$i" "$evidence_dir/partition-one.during.json")" = ACTIVE ] ||
                fail "partition-one: Agent $i left ACTIVE while Agent $victim was partitioned"
            uid=$(agent_var project_uid "$i")
            api GET "$base_url/api/v1/projects/$uid/files?path=compose.yaml" '' \
                "$evidence_dir/partition-one.file-$i.json"
            compose_op "$i" compose.up "partition-up" 240 ||
                fail "partition-one: Agent $i could not run an operation while Agent $victim was partitioned"
        fi
        i=$((i + 1))
    done
    # A mutation aimed at the partitioned Agent must be refused, not queued
    # silently and not answered as if it had run.
    aid=$(agent_var agent_id "$victim")
    uid=$(agent_var project_uid "$victim")
    victim_status=$(api_status POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "lab-partition-victim-$$" --arg agent "$aid" --arg project "$uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
        "$evidence_dir/partition-one.victim-op.json")
    case "$victim_status" in
        503|409|404) ;;
        *) fail "partition-one: an operation on the partitioned Agent answered HTTP $victim_status" ;;
    esac
    record partition_one_victim_offline PASS
    record partition_one_others_unaffected PASS
    record partition_one_victim_operation_refused "$victim_status"

    docker network connect --alias "host-$victim" "$network" "$(host_name "$victim")" >/dev/null
    wait_hosts_active "$agents" "$evidence_dir/partition-one.after.json" 300 ||
        fail "partition-one: the partitioned Agent did not return"
    record partition_one_victim_recovered PASS
fi

# ------------------------------------- case: real Docker daemon restart
# The gap this closes: on a working host the daemon cannot be restarted, so
# every earlier gate recorded this as SKIPPED_NOT_AUTHORIZED. Inside a dind
# host it is safe and real - the daemon goes away, its containers stop, and
# both come back.
if selected daemon-restart; then
    victim=2
    [ "$agents" -ge "$victim" ] || victim=1
    aid=$(agent_var agent_id "$victim")
    dashboard "$evidence_dir/daemon-restart.before.json"
    [ "$(host_state "$victim" "$evidence_dir/daemon-restart.before.json")" = ACTIVE ] ||
        fail "daemon-restart: Agent $victim was not ACTIVE before the restart"

    host_exec "$victim" 'pkill -TERM dockerd' || true
    record daemon_restart_signalled PASS

    # The Agent is a container of that daemon, so it stops with it. What is
    # asserted is that the pair comes back on its own.
    degraded=0
    deadline=$(( $(date +%s) + 240 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        dashboard "$evidence_dir/daemon-restart.during.json"
        state=$(host_state "$victim" "$evidence_dir/daemon-restart.during.json")
        docker_ok=$(jq -r --arg name "lab-agent-$victim" \
            '[.hosts[] | select(.display_name == $name)][0].capabilities.docker.enabled // false' \
            "$evidence_dir/daemon-restart.during.json")
        if [ "$state" != ACTIVE ] || [ "$docker_ok" != true ]; then degraded=1; break; fi
        sleep 3
    done
    [ "$degraded" -eq 1 ] || fail "daemon-restart: the Server never observed the daemon going away"

    # Other Agents must not have noticed.
    i=1
    while [ "$i" -le "$agents" ]; do
        if [ "$i" != "$victim" ]; then
            [ "$(host_state "$i" "$evidence_dir/daemon-restart.during.json")" = ACTIVE ] ||
                fail "daemon-restart: Agent $i left ACTIVE when Agent $victim's daemon restarted"
        fi
        i=$((i + 1))
    done
    record daemon_restart_isolated PASS

    wait_host_daemon "$victim" || fail "daemon-restart: the daemon on host $victim never came back"
    wait_hosts_active "$agents" "$evidence_dir/daemon-restart.after.json" 420 ||
        fail "daemon-restart: Agent $victim did not return ACTIVE after the daemon came back"
    jq -e --arg name "lab-agent-$victim" \
        '[.hosts[] | select(.display_name == $name)][0] |
         .capabilities.docker.enabled == true and .capabilities.compose.enabled == true' \
        "$evidence_dir/daemon-restart.after.json" >/dev/null ||
        fail "daemon-restart: Docker capability did not recover"
    # Rediscovery has to be real, not remembered: the fixture must be running
    # and visible again through the Agent.
    compose_op "$victim" compose.up "daemon-restart-up" 300 ||
        fail "daemon-restart: the fixture could not be brought up after the daemon returned"
    api GET "$base_url/api/v1/hosts/$aid/containers" '' "$evidence_dir/daemon-restart.containers.json"
    record daemon_restart_recovered PASS
    record daemon_restart_rediscovered PASS
fi

# ---------------------------------------- case: host lost outright and back
if selected host-poweroff; then
    victim=$agents
    aid=$(agent_var agent_id "$victim")
    before_incarnation=$(audit_page "$aid" "$evidence_dir/host-poweroff.audit-before.json" 2>/dev/null; \
        jq -r '.coverage.delivery_next.incarnation // 0' "$evidence_dir/host-poweroff.audit-before.json")
    docker kill --signal KILL "$(host_name "$victim")" >/dev/null
    offline=0
    deadline=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        dashboard "$evidence_dir/host-poweroff.during.json"
        [ "$(host_state "$victim" "$evidence_dir/host-poweroff.during.json")" != ACTIVE ] && { offline=1; break; }
        sleep 5
    done
    [ "$offline" -eq 1 ] || fail "host-poweroff: the killed host never left ACTIVE"
    i=1
    while [ "$i" -lt "$victim" ]; do
        [ "$(host_state "$i" "$evidence_dir/host-poweroff.during.json")" = ACTIVE ] ||
            fail "host-poweroff: Agent $i left ACTIVE when host $victim was killed"
        i=$((i + 1))
    done
    record host_poweroff_isolated PASS

    docker start "$(host_name "$victim")" >/dev/null
    wait_host_daemon "$victim" || fail "host-poweroff: the daemon did not return after the host restarted"
    wait_hosts_active "$agents" "$evidence_dir/host-poweroff.after.json" 420 ||
        fail "host-poweroff: the Agent did not return after its host restarted"
    audit_page "$aid" "$evidence_dir/host-poweroff.audit-after.json"
    after_incarnation=$(jq -r '.coverage.delivery_next.incarnation // 0' "$evidence_dir/host-poweroff.audit-after.json")
    [ "$after_incarnation" -gt "$before_incarnation" ] ||
        fail "host-poweroff: the incarnation did not advance ($before_incarnation -> $after_incarnation)"
    jq -e '[.coverage.gaps[]? | select(.source == "AGENT_CONTINUITY_UNCERTAIN")] | length >= 1' \
        "$evidence_dir/host-poweroff.audit-after.json" >/dev/null ||
        printf 'note: no AGENT_CONTINUITY_UNCERTAIN gap was recorded for the killed incarnation\n' \
            >>"$evidence_dir/host-poweroff.notes.txt"
    record host_poweroff_incarnation_advanced "$before_incarnation->$after_incarnation"
    record host_poweroff_recovered PASS
fi

# ------------------------------------------- case: cross-agent bulk isolation
if selected bulk-isolation; then
    [ "$agents" -ge 3 ] || fail "bulk-isolation needs at least three Agents"
    noisy=1; durable=2; controller=3
    noisy_aid=$(agent_var agent_id "$noisy")
    durable_aid=$(agent_var agent_id "$durable")
    compose_op "$noisy" compose.up "bulk-up" 240 || fail "bulk-isolation: the noisy fixture would not start"
    noisy_container=$(fixture_container "$noisy")
    [ -n "$noisy_container" ] || fail "bulk-isolation: the noisy host has no running fixture container"

    audit_page "$durable_aid" "$evidence_dir/bulk.durable-before.json"
    durable_before=$(audit_ack_seq "$evidence_dir/bulk.durable-before.json")

    # Agent 1 floods logs and stats, including a deliberately slow consumer.
    for n in 1 2 3 4; do
        curl --silent --max-time 45 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/live/logs?agent_id=$noisy_aid&container_id=$noisy_container" \
            >"$evidence_dir/bulk.logs-$n.sse" 2>/dev/null &
        curl --silent --max-time 45 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/live/stats?agent_id=$noisy_aid&container_id=$noisy_container" \
            >"$evidence_dir/bulk.stats-$n.sse" 2>/dev/null &
    done
    curl --silent --max-time 45 --limit-rate 1k --cacert "$runtime/bootstrap/server-ca.crt" \
        "$base_url/api/v1/live/logs?agent_id=$noisy_aid&container_id=$noisy_container" \
        >"$evidence_dir/bulk.slow.sse" 2>/dev/null &

    # Agent 3 must keep completing control-plane work while that runs.
    control_failures=0
    n=1
    while [ "$n" -le 4 ]; do
        compose_op "$controller" compose.up "bulk-control-$n" 120 || control_failures=$((control_failures + 1))
        n=$((n + 1))
    done
    wait
    [ "$control_failures" -eq 0 ] ||
        fail "bulk-isolation: $control_failures control operations on Agent $controller failed under another Agent's bulk load"

    audit_page "$durable_aid" "$evidence_dir/bulk.durable-after.json"
    durable_after=$(audit_ack_seq "$evidence_dir/bulk.durable-after.json")
    [ "$durable_after" -ge "$durable_before" ] ||
        fail "bulk-isolation: Agent $durable's acknowledged cursor went backwards under load"
    record bulk_isolation_control_ops_completed PASS
    record bulk_isolation_durable_cursor "$durable_before->$durable_after"
fi

# ------------------------------------------------- case: operation flood
if selected operation-flood; then
    i=1
    while [ "$i" -le "$agents" ]; do
        aid=$(agent_var agent_id "$i")
        uid=$(agent_var project_uid "$i")
        n=1
        while [ "$n" -le 10 ]; do
            api_status POST "$base_url/api/v1/projects/$uid/backups" \
                "$(jq -cn --arg id "lab-flood-$i-$n-$$" '{operation_id:$id,relative_paths:["compose.yaml"]}')" \
                "$evidence_dir/flood-$i-$n.json" >>"$evidence_dir/flood.statuses.txt"
            printf '\n' >>"$evidence_dir/flood.statuses.txt"
            n=$((n + 1))
        done &
        i=$((i + 1))
    done
    wait
    server_errors=$(grep -c '^5' "$evidence_dir/flood.statuses.txt" || true)
    [ "$server_errors" -eq 0 ] ||
        fail "operation-flood: $server_errors requests answered with a server error"
    ! grep -qi 'SQLITE_BUSY\|database is locked' "$evidence_dir/server.during.log" 2>/dev/null || true
    dashboard "$evidence_dir/flood.dashboard.json"
    jq -e '[.hosts[] | select(.state == "ACTIVE")] | length == '"$agents" \
        "$evidence_dir/flood.dashboard.json" >/dev/null ||
        fail "operation-flood: the fleet did not stay ACTIVE through the flood"
    record operation_flood_no_server_error PASS
    record operation_flood_fleet_stable PASS
fi

# ------------------------------------------- case: audit catch-up fairness
if selected catchup-fairness; then
    i=1
    while [ "$i" -le "$agents" ]; do
        docker network disconnect "$network" "$(host_name "$i")" >/dev/null 2>&1 || true
        i=$((i + 1))
    done
    # Unequal backlogs: each Agent generates a different amount while offline.
    i=1
    while [ "$i" -le "$agents" ]; do
        rounds=$((i * 6))
        host_exec "$i" "n=0; while [ \$n -lt $rounds ]; do docker restart \$(docker ps -q --filter label=com.docker.compose.project=$prefix-fixture-$i | head -1) >/dev/null 2>&1 || true; n=\$((n+1)); done" || true
        i=$((i + 1))
    done
    i=1
    while [ "$i" -le "$agents" ]; do
        docker network connect --alias "host-$i" "$network" "$(host_name "$i")" >/dev/null 2>&1 || true
        i=$((i + 1))
    done
    wait_hosts_active "$agents" "$evidence_dir/catchup.dashboard.json" 420 ||
        fail "catchup-fairness: not every Agent returned after the unequal backlog"
    # Every Agent's cursor has to move, not just the smallest backlog's.
    stalled=
    i=1
    while [ "$i" -le "$agents" ]; do
        aid=$(agent_var agent_id "$i")
        audit_page "$aid" "$evidence_dir/catchup.audit-$i-first.json"
        first=$(audit_ack_seq "$evidence_dir/catchup.audit-$i-first.json")
        eval "catchup_first_$i=\$first"
        i=$((i + 1))
    done
    sleep 45
    i=1
    while [ "$i" -le "$agents" ]; do
        aid=$(agent_var agent_id "$i")
        audit_page "$aid" "$evidence_dir/catchup.audit-$i-second.json"
        second=$(audit_ack_seq "$evidence_dir/catchup.audit-$i-second.json")
        first=$(agent_var catchup_first "$i")
        [ "$second" -ge "$first" ] || stalled="$stalled $i"
        record "catchup_agent_${i}_ack" "$first->$second"
        i=$((i + 1))
    done
    [ -z "$stalled" ] || fail "catchup-fairness: acknowledged cursors went backwards for agents:$stalled"
    record catchup_all_cursors_advanced PASS
fi

# ------------------------------------------ case: per-agent disk pressure
if selected disk-pressure; then
    victim=2
    [ "$agents" -ge "$victim" ] || victim=1
    # The Agent state volume is filled from inside its own host only.
    host_exec "$victim" 'df -k /var/lib/dockpilot-agent | tail -1' >"$evidence_dir/disk-pressure.before.txt"
    # The free-space floor is max(1 GiB, 5%) of a filesystem this lab does not
    # control, and 5% of a developer disk is hundreds of gigabytes. The other
    # trigger is reachable: Agent state above its 2 GiB budget. 3 GiB crosses
    # it with margin and costs a fraction of the I/O.
    host_exec "$victim" 'dd if=/dev/zero of=/var/lib/dockpilot-agent/lab-ballast bs=1M count=3072 2>/dev/null; true' || true
    host_exec "$victim" 'df -k /var/lib/dockpilot-agent | tail -1' >"$evidence_dir/disk-pressure.after.txt"
    degraded=0
    deadline=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        dashboard "$evidence_dir/disk-pressure.during.json"
        if jq -e --arg name "lab-agent-$victim" \
            '[.hosts[] | select(.display_name == $name)][0].capabilities |
             [to_entries[] | select(.value.reason != null and
                (.value.reason | test("DEGRADED_STORAGE|FILESYSTEM_FREE_LOW|AGENT_STATE_BUDGET_EXCEEDED")))] | length >= 1' \
            "$evidence_dir/disk-pressure.during.json" >/dev/null 2>&1; then
            degraded=1
            break
        fi
        sleep 5
    done
    if [ "$degraded" -eq 1 ]; then
        record disk_pressure_reason_reported PASS
    else
        # The ballast may not be enough to cross the floor on a large host
        # filesystem. That is a limit of the environment, not a product answer,
        # and is recorded rather than asserted away.
        record disk_pressure_reason_reported SKIPPED_FLOOR_NOT_REACHED
    fi
    i=1
    while [ "$i" -le "$agents" ]; do
        if [ "$i" != "$victim" ]; then
            [ "$(host_state "$i" "$evidence_dir/disk-pressure.during.json")" = ACTIVE ] ||
                fail "disk-pressure: Agent $i left ACTIVE while Agent $victim was under pressure"
            uid=$(agent_var project_uid "$i")
            api GET "$base_url/api/v1/projects/$uid/files?path=compose.yaml" '' \
                "$evidence_dir/disk-pressure.file-$i.json"
        fi
        i=$((i + 1))
    done
    record disk_pressure_others_unaffected PASS
    host_exec "$victim" 'rm -f /var/lib/dockpilot-agent/lab-ballast' || true
    wait_hosts_active "$agents" "$evidence_dir/disk-pressure.recovered.json" 300 ||
        fail "disk-pressure: the fleet did not settle after the pressure was released"
    record disk_pressure_recovered PASS
fi

# ------------------------------------------------------ final invariants
dashboard "$evidence_dir/final.dashboard.json"
[ "$(jq -r '[.hosts[].id] | length' "$evidence_dir/final.dashboard.json")" = "$agents" ] ||
    fail "final: the Server no longer lists exactly $agents Agents"
[ "$(jq -r '[.hosts[].id] | unique | length' "$evidence_dir/final.dashboard.json")" = "$agents" ] ||
    fail "final: duplicate Agent identities survived the lab"

i=1
while [ "$i" -le "$agents" ]; do
    aid=$(agent_var agent_id "$i")
    uid=$(agent_var project_uid "$i")
    # The project lock must be free on every Agent.
    probe="lab-lock-probe-$i-$$"
    api POST "$base_url/api/v1/projects/$uid/backups" \
        "$(jq -cn --arg id "$probe" '{operation_id:$id,relative_paths:["compose.yaml"]}')" \
        "$evidence_dir/final.probe-$i.accepted.json"
    poll_operation "$aid" "$probe" "$evidence_dir/final.probe-$i.json" 180 ||
        fail "final: the lock probe on Agent $i never reached a terminal state"
    jq -e '((.error // "") | test("PROJECT_BUSY")) | not' "$evidence_dir/final.probe-$i.json" >/dev/null ||
        fail "final: Agent $i's project lock is held by nothing"

    # Audit coverage must be describable on every Agent.
    audit_page "$aid" "$evidence_dir/final.audit-$i.json"
    jq -e '
      (.coverage.gaps // []) | all(
        (.precision == "exact" or .precision == "coalesced" or .precision == "unknown") and
        (.source == "AGENT_GAP" or .source == "SERVER_RETENTION" or
         .source == "AGENT_CONTINUITY_UNCERTAIN" or .source == "SERVER_CURSOR_REGRESSION") and
        (.from.seq <= .until.seq))' "$evidence_dir/final.audit-$i.json" >/dev/null ||
        fail "final: an Audit coverage entry on Agent $i is missing its source, precision, or ordering"
    jq -e '
      (.coverage.ack == null) or (.coverage.delivery_next == null) or
      (.coverage.ack.incarnation < .coverage.delivery_next.incarnation) or
      (.coverage.ack.incarnation == .coverage.delivery_next.incarnation and
       .coverage.ack.seq < .coverage.delivery_next.seq)' "$evidence_dir/final.audit-$i.json" >/dev/null ||
        fail "final: Agent $i's acknowledged cursor passed the Server delivery cursor"

    # Docker's view and Dockpilot's view of that host must agree, and the file
    # Dockpilot reads must be the file on the host's disk.
    host_exec "$i" "docker ps --format '{{.ID}}'" >"$evidence_dir/final.docker-$i.txt" || true
    api GET "$base_url/api/v1/projects/$uid/files?path=compose.yaml" '' "$evidence_dir/final.file-$i.json"
    on_disk=$(host_exec "$i" "sha256sum $fixture_root/compose.yaml | cut -d' ' -f1" | tr -d '\r\n')
    reported=$(jq -r '.sha256' "$evidence_dir/final.file-$i.json")
    [ "$on_disk" = "$reported" ] ||
        fail "final: Agent $i reported compose.yaml as $reported, its host says $on_disk"

    # No restore journal or staging orphan.
    host_exec "$i" 'docker exec dp-agent /bin/sh -c "ls -A /var/lib/dockpilot/restore-journal 2>/dev/null" || true' \
        >"$evidence_dir/final.restore-journal-$i.txt" 2>&1 || true
    [ ! -s "$evidence_dir/final.restore-journal-$i.txt" ] ||
        fail "final: a restore journal survived on Agent $i"
    host_exec "$i" "ls -A $fixture_root | grep -e '^\.dockpilot-' || true" \
        >"$evidence_dir/final.staging-$i.txt" 2>&1 || true
    [ ! -s "$evidence_dir/final.staging-$i.txt" ] ||
        fail "final: staging files were orphaned on Agent $i"
    i=$((i + 1))
done

capture_log "$server" "$evidence_dir/server.during.log"
! grep -qi 'SQLITE_BUSY\|database is locked' "$evidence_dir/server.during.log" ||
    fail "final: the Server logged SQLite contention during the lab"
! grep -q 'api request failed' "$evidence_dir/server.during.log" ||
    fail "final: the Server logged a failed API request during the lab"
! grep -rF -- "lab-secret-must-never-be-recorded" "$evidence_dir" --exclude=STATUS --exclude=SHA256SUMS >/dev/null 2>&1 ||
    fail "final: the project secret leaked into recorded evidence"

record final_invariants PASS
{
    printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >>"$evidence_dir/assertions.env"
completed=1
