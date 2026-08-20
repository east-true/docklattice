#!/bin/sh
set -eu

usage() {
    printf 'usage: %s ABSOLUTE_EVIDENCE_DIR SERVER_IMAGE_ID AGENT_IMAGE_ID FIXTURE_IMAGE_ID\n' "$0" >&2
    printf 'all image arguments must be exact local sha256 image IDs\n' >&2
}

fail() {
    printf 'clean-host install E2E failed: %s\n' "$*" >&2
    failure_reason=$*
    exit 1
}

[ "$#" -eq 4 ] || {
    usage
    exit 2
}

evidence_dir=$1
server_image=$2
agent_image=$3
fixture_image=$4
evidence_max_bytes=${CLEAN_HOST_EVIDENCE_MAX_BYTES:-16777216}
log_max_bytes=${CLEAN_HOST_LOG_MAX_BYTES:-1048576}

case "$evidence_dir" in
    /*) ;;
    *) fail "preflight: evidence directory must be absolute" ;;
esac
case "$evidence_dir" in
    *:*|*'
'*) fail "preflight: evidence directory cannot contain colon or newline" ;;
esac
[ ! -e "$evidence_dir" ] || fail "preflight: refusing to overwrite evidence directory: $evidence_dir"
evidence_parent=${evidence_dir%/*}
[ -n "$evidence_parent" ] || evidence_parent=/
[ -d "$evidence_parent" ] || fail "preflight: evidence parent does not exist: $evidence_parent"

require_image_id_shape() {
    image=$1
    label=$2
    case "$image" in
        sha256:*) digest=${image#sha256:} ;;
        *) fail "preflight: $label must be an exact sha256 image ID" ;;
    esac
    case "$digest" in
        ''|*[!0-9a-f]*) fail "preflight: $label image ID is not lowercase hexadecimal" ;;
    esac
    [ "${#digest}" -eq 64 ] || fail "preflight: $label image ID must contain 64 digest characters"
}

require_image_id_shape "$server_image" Server
require_image_id_shape "$agent_image" Agent
require_image_id_shape "$fixture_image" fixture
[ "$server_image" != "$agent_image" ] && [ "$server_image" != "$fixture_image" ] && [ "$agent_image" != "$fixture_image" ] ||
    fail "preflight: Server, Agent, and fixture image IDs must be distinct"

case "$evidence_max_bytes" in
    ''|*[!0-9]*) fail "preflight: CLEAN_HOST_EVIDENCE_MAX_BYTES must be an integer" ;;
esac
case "$log_max_bytes" in
    ''|*[!0-9]*) fail "preflight: CLEAN_HOST_LOG_MAX_BYTES must be an integer" ;;
esac
[ "$evidence_max_bytes" -ge 4194304 ] && [ "$evidence_max_bytes" -le 67108864 ] ||
    fail "preflight: evidence cap must be between 4 MiB and 64 MiB"
[ "$log_max_bytes" -ge 65536 ] && [ "$log_max_bytes" -le 4194304 ] ||
    fail "preflight: log cap must be between 64 KiB and 4 MiB"

# Docker is intentionally the first external prerequisite. In particular, its
# absence must fail before an evidence directory or runtime secret is created.
command -v docker >/dev/null 2>&1 || fail "preflight: docker is required"
for command_name in openssl curl jq awk grep date du df sha256sum stat mktemp head wc find chmod uname cat cp mv rm rmdir mkdir sleep tr; do
    command -v "$command_name" >/dev/null 2>&1 || fail "preflight: required command not found: $command_name"
done
[ -z "${DOCKER_HOST:-}" ] || fail "preflight: DOCKER_HOST is not supported; use the local default Engine socket"
docker info >/dev/null 2>&1 || fail "preflight: Docker daemon is unavailable or permission is denied"
[ "$(docker info --format '{{.OSType}}')" = linux ] || fail "preflight: a Linux Docker Engine is required"
[ "$(docker info --format '{{.CgroupVersion}}')" = 2 ] || fail "preflight: Docker must use cgroup v2"
[ -r /sys/fs/cgroup/cgroup.controllers ] || fail "preflight: host cgroup v2 is not readable"
[ -S /var/run/docker.sock ] && [ -r /var/run/docker.sock ] && [ -w /var/run/docker.sock ] ||
    fail "preflight: readable and writable /var/run/docker.sock is required"

for image in "$server_image" "$agent_image" "$fixture_image"; do
    docker image inspect "$image" >/dev/null 2>&1 || fail "preflight: exact local image is unavailable: $image"
    [ "$(docker image inspect --format '{{.Id}}' "$image")" = "$image" ] ||
        fail "preflight: image reference did not resolve to its exact requested ID: $image"
done
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$server_image")" = "Dockpilot Server" ] ||
    fail "preflight: Server image is not the production Server target"
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$agent_image")" = "Dockpilot Agent" ] ||
    fail "preflight: Agent image is not the production Agent target"
[ "$(docker image inspect --format '{{index .Config.Labels "io.dockpilot.role"}}' "$agent_image")" = agent ] ||
    fail "preflight: Agent image lacks io.dockpilot.role=agent"
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.licenses"}}' "$server_image")" = Apache-2.0 ] ||
    fail "preflight: Server production license label is missing"
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.licenses"}}' "$agent_image")" = Apache-2.0 ] ||
    fail "preflight: Agent production license label is missing"
server_version=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$server_image")
agent_version=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$agent_image")
server_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$server_image")
agent_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$agent_image")
[ "$server_version" = "$agent_version" ] && [ -n "$server_version" ] && [ "$server_version" != dev ] && [ "$server_version" != '<no value>' ] ||
    fail "preflight: Server and Agent must carry the same non-dev release version"
[ "$server_revision" = "$agent_revision" ] || fail "preflight: Server and Agent revisions differ"
case "$server_revision" in ''|*[!0-9a-f]*) fail "preflight: production revision must be lowercase hexadecimal" ;; esac
[ "${#server_revision}" -eq 40 ] || fail "preflight: production revision must be a full 40-character Git object ID"
compose_version=$(docker image inspect --format '{{index .Config.Labels "io.dockpilot.compose.version"}}' "$agent_image")
[ -n "$compose_version" ] && [ "$compose_version" != '<no value>' ] || fail "preflight: Agent Compose version label is missing"

available_kib=$(df -Pk "$evidence_parent" | awk 'NR == 2 { print $4 }')
[ "$available_kib" -ge $((evidence_max_bytes / 1024 + 65536)) ] ||
    fail "preflight: evidence filesystem needs the cap plus 64 MiB free"

prefix="dockpilot-clean-host-$(date -u +%Y%m%dT%H%M%SZ)-$$"
probe="$prefix-probe"
docker run --pull never -d --name "$probe" --memory 32m --memory-swap 32m \
    --entrypoint /bin/sh "$server_image" -c 'sleep 30' >/dev/null ||
    fail "preflight: cannot start a local cgroup probe from the exact Server image"
probe_pid=$(docker inspect --format '{{.State.Pid}}' "$probe")
probe_cgroup=$(awk -F: '$1 == "0" { print $3; exit }' "/proc/$probe_pid/cgroup" 2>/dev/null || true)
if [ -z "$probe_cgroup" ] || [ ! -r "/sys/fs/cgroup$probe_cgroup/memory.max" ] ||
    [ "$(cat "/sys/fs/cgroup$probe_cgroup/memory.max" 2>/dev/null || true)" != 33554432 ]; then
    docker rm -f "$probe" >/dev/null 2>&1 || true
    fail "preflight: Docker container cgroup is not locally observable with the requested limit"
fi
docker rm -f "$probe" >/dev/null
docker run --pull never --rm --entrypoint /bin/sh "$fixture_image" -c 'exit 0' >/dev/null ||
    fail "preflight: exact fixture image must provide a working /bin/sh"

runtime_base=${TMPDIR:-/tmp}
case "$runtime_base" in /*) ;; *) fail "preflight: TMPDIR must be absolute" ;; esac
case "$runtime_base" in *:*|*'
'*) fail "preflight: TMPDIR cannot contain colon or newline" ;; esac
[ "$runtime_base" != / ] || fail "preflight: TMPDIR cannot be the filesystem root"
[ -d "$runtime_base" ] || fail "preflight: TMPDIR does not exist"

umask 077
artifact_created=0
runtime=
server=
agent=
network=
# Compose normalizes project names to lower case, so the label filters used to
# assert and to clean up must be built from the normalized form.
compose_project=$(printf '%s' "$prefix-fixture" | tr '[:upper:]' '[:lower:]')
completed=0
failure_reason="harness did not complete"
not_clean=0

capture_log() {
    container=$1
    output=$2
    [ -n "$container" ] || return 0
    docker inspect "$container" >/dev/null 2>&1 || return 0
    docker logs --tail 2000 "$container" 2>&1 | head -c "$log_max_bytes" >"$output" || true
}

remove_compose_objects() {
    ids=$(docker ps -aq --filter "label=com.docker.compose.project=$compose_project" 2>/dev/null || true)
    if [ -n "$ids" ]; then
        # Docker IDs contain no whitespace and are bounded to this unique label.
        # shellcheck disable=SC2086
        docker rm -f $ids >/dev/null 2>&1 || true
    fi
    ids=$(docker network ls -q --filter "label=com.docker.compose.project=$compose_project" 2>/dev/null || true)
    if [ -n "$ids" ]; then
        # shellcheck disable=SC2086
        docker network rm $ids >/dev/null 2>&1 || true
    fi
}

scrub_runtime() {
    [ -n "${runtime:-}" ] && [ -d "$runtime" ] || return 0
    case "$runtime" in "$runtime_base"/dockpilot-clean-host.*) ;; *) return 1 ;; esac
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/dockpilot-clean-host-runtime" "$server_image" \
        -c 'rm -rf /dockpilot-clean-host-runtime/server /dockpilot-clean-host-runtime/agent /dockpilot-clean-host-runtime/bootstrap /dockpilot-clean-host-runtime/projects' \
        >/dev/null 2>&1 || return 1
    rmdir "$runtime"
}

seal_evidence() {
    find "$evidence_dir" -type f -exec chmod 0444 {} \;
    find "$evidence_dir" -type d -exec chmod 0555 {} \;
}

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$artifact_created" -eq 1 ]; then
        capture_log "$agent" "$evidence_dir/agent.final.log"
        capture_log "$server" "$evidence_dir/server.final.log"
        used_kib=$(du -sk "$evidence_dir" | awk '{ print $1 }')
        if [ $((used_kib * 1024)) -gt "$evidence_max_bytes" ]; then
            status=1
            failure_reason="evidence size cap exceeded during final log capture"
        fi
    fi
    [ -z "$agent" ] || docker rm -f "$agent" >/dev/null 2>&1 || true
    remove_compose_objects
    [ -z "$server" ] || docker rm -f "$server" >/dev/null 2>&1 || true
    [ -z "$network" ] || docker network rm "$network" >/dev/null 2>&1 || true
    runtime_cleaned=true
    scrub_runtime || runtime_cleaned=false
    if [ "$artifact_created" -eq 1 ]; then
        if [ "$not_clean" -eq 1 ]; then
            # This gate is defined for a fresh Docker host. On a host that
            # already manages other Compose projects the assertion it makes is
            # not true and must not be relaxed to make the run pass, so the
            # outcome is recorded as neither PASS nor a product failure.
            {
                printf 'status=SKIPPED_NOT_CLEAN\n'
                printf 'reason=%s\n' "$failure_reason" | tr '\r\n' '  '
                printf '\n'
                printf 'runtime_cleaned=%s\n' "$runtime_cleaned"
            } >"$evidence_dir/STATUS"
        elif [ "$status" -eq 0 ] && [ "$completed" -eq 1 ] && [ "$runtime_cleaned" = true ]; then
            printf 'status=PASS\n' >"$evidence_dir/STATUS"
        else
            [ "$status" -ne 0 ] || status=1
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
trap 'failure_reason="harness interrupted by signal"; exit 130' HUP INT TERM

runtime=$(mktemp -d "$runtime_base/dockpilot-clean-host.XXXXXXXX")
case "$runtime" in "$runtime_base"/dockpilot-clean-host.*) ;; *) fail "mktemp returned an unexpected runtime root" ;; esac
chmod 0700 "$runtime"
mkdir "$evidence_dir"
artifact_created=1
{
    printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'kernel=%s\n' "$(uname -srvm)"
    printf 'docker_server_version=%s\n' "$(docker info --format '{{.ServerVersion}}')"
    printf 'docker_cgroup_driver=%s\n' "$(docker info --format '{{.CgroupDriver}}')"
    printf 'docker_cgroup_version=2\n'
    printf 'server_image_id=%s\nagent_image_id=%s\nfixture_image_id=%s\n' "$server_image" "$agent_image" "$fixture_image"
    printf 'evidence_max_bytes=%s\nlog_max_bytes=%s\n' "$evidence_max_bytes" "$log_max_bytes"
} >"$evidence_dir/environment.env"

check_evidence_cap() {
    used_kib=$(du -sk "$evidence_dir" | awk '{ print $1 }')
    [ $((used_kib * 1024)) -le "$evidence_max_bytes" ] || fail "evidence size cap exceeded"
}

mkdir "$runtime/server" "$runtime/server/tls" "$runtime/agent" "$runtime/bootstrap" "$runtime/projects"
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
    -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
    -keyout "$runtime/server/tls/server.key" -out "$runtime/server/tls/server.crt" \
    >"$evidence_dir/openssl.stdout" 2>"$evidence_dir/openssl.stderr"
cp "$runtime/server/tls/server.crt" "$runtime/bootstrap/server-ca.crt"
cat >"$runtime/projects/compose.yaml" <<EOF
name: $compose_project
services:
  clean-host-fixture:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 60; done"]
EOF
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
    -v "$runtime:/clean-host" "$server_image" -c \
    'chown -R 65532:65532 /clean-host/server /clean-host/agent; chmod 0700 /clean-host/server /clean-host/agent; chmod 0700 /clean-host/server/tls; chmod 0600 /clean-host/server/tls/server.crt /clean-host/server/tls/server.key; chmod 0755 /clean-host/projects; chmod 0644 /clean-host/projects/compose.yaml' \
    >/dev/null
# The state roots are 0700 and owned by 65532, so an unprivileged operator on
# the host cannot traverse them. Read the ownership and mode back through the
# same root helper that set them instead of stat(1) on the host.
state_modes=$(docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
    -v "$runtime:/clean-host" "$server_image" -c \
    'stat -c "%u:%g:%a" /clean-host/server /clean-host/agent /clean-host/server/tls/server.key')
[ "$(printf '%s\n' "$state_modes" | sed -n 1p)" = 65532:65532:700 ] || fail "Server state ownership or mode is incorrect"
[ "$(printf '%s\n' "$state_modes" | sed -n 2p)" = 65532:65532:700 ] || fail "Agent state ownership or mode is incorrect"
[ "$(printf '%s\n' "$state_modes" | sed -n 3p)" = 65532:65532:600 ] || fail "TLS key ownership or mode is incorrect"

network="$prefix-network"
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

docker network create --subnet "$(harness_subnet)" "$network" >"$evidence_dir/network.id"
server="$prefix-server"
docker run --pull never -d --name "$server" --network "$network" --network-alias server \
    --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
    -p 127.0.0.1::8080 -v "$runtime/server:/var/lib/dockpilot:rw" "$server_image" \
    server --listen 0.0.0.0:8080 --agent-listen 0.0.0.0:8443 --allow-public-bind \
    >"$evidence_dir/server.container-id"
server_port=$(docker port "$server" 8080/tcp | awk -F: 'NR == 1 { print $NF }')
case "$server_port" in ''|*[!0-9]*) fail "could not resolve the Server HTTPS port" ;; esac
base_url="https://127.0.0.1:$server_port"
ready=0
deadline=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
    if curl --fail --silent --show-error --max-time 3 --cacert "$runtime/bootstrap/server-ca.crt" \
        "$base_url/api/v1/dashboard" >"$evidence_dir/server-ready.tmp" 2>/dev/null; then
        mv "$evidence_dir/server-ready.tmp" "$evidence_dir/server-ready.json"
        ready=1
        break
    fi
    sleep 1
done
[ "$ready" -eq 1 ] || fail "Server did not become HTTPS-ready"

docker run --pull never --rm --user 65532:65532 \
    -v "$runtime/server:/var/lib/dockpilot:rw" "$server_image" \
    server issue-token --state-dir /var/lib/dockpilot --ttl 15m \
    >"$runtime/bootstrap/join-token" 2>"$evidence_dir/issue-token.stderr"
[ "$(wc -l <"$runtime/bootstrap/join-token" | awk '{ print $1 }')" -eq 1 ] || fail "Join Token CLI did not emit exactly one line"
token_size=$(wc -c <"$runtime/bootstrap/join-token" | awk '{ print $1 }')
[ "$token_size" -gt 1 ] && [ "$token_size" -le 4096 ] || fail "Join Token CLI output size is invalid"
# /agent is already 0700 and owned by 65532, so the bootstrap material has to
# be placed and tightened by the same root helper rather than copied in from an
# unprivileged host shell.
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
    -v "$runtime/agent:/agent" -v "$runtime/bootstrap:/bootstrap:ro" "$server_image" -c \
    'cp /bootstrap/server-ca.crt /agent/server-ca.crt; cp /bootstrap/join-token /agent/join-token; chown -R 65532:65532 /agent; chmod 0700 /agent; chmod 0600 /agent/server-ca.crt /agent/join-token' >/dev/null

socket_gid=$(stat -c '%g' /var/run/docker.sock)
agent="$prefix-agent"
start_agent() {
    with_token=$1
    if [ "$with_token" = true ]; then
        token_args="--join-token-file /var/lib/dockpilot/join-token"
    else
        token_args=
    fi
    # token_args is a fixed harness-controlled option, never user input.
    # shellcheck disable=SC2086
    docker run --pull never -d --name "$agent" --network "$network" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.dockpilot.role=agent \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/agent:/var/lib/dockpilot:rw" \
        -v "$runtime/projects:$runtime/projects:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/dockpilot/server-ca.crt $token_args \
        --display-name clean-host-agent --self-container-name "$agent" \
        --project-root "$runtime/projects"
}
start_agent true >"$evidence_dir/agent.initial.container-id"

wait_dashboard() {
    expected_agent=$1
    output=$2
    ready=0
    deadline=$(( $(date +%s) + 90 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >"$output.tmp" 2>/dev/null &&
            jq -e --arg root "$runtime/projects" --arg expected "$expected_agent" \
                --arg name "$compose_project" '
              (.hosts | length) == 1 and
              (.projects | length) == 1 and
              .hosts[0].state == "ACTIVE" and
              .hosts[0].display_name == "clean-host-agent" and
              .hosts[0].capabilities.connection.enabled == true and
              .hosts[0].capabilities.docker.enabled == true and
              .hosts[0].capabilities.compose.enabled == true and
              .hosts[0].capabilities.discovery.enabled == true and
              ($expected == "" or .hosts[0].id == $expected) and
              .projects[0].name == $name and
              .projects[0].working_dir == $root and
              .projects[0].present == true and .projects[0].stale == false and
              .projects[0].collision == false and .projects[0].read_only == false and
              .projects[0].compose_executable == true and .projects[0].filesystem_writable == true
            ' "$output.tmp" >/dev/null 2>&1; then
            mv "$output.tmp" "$output"
            ready=1
            break
        fi
        sleep 1
    done
    [ "$ready" -eq 1 ]
}

if ! wait_dashboard "" "$evidence_dir/dashboard.initial.json"; then
    # Distinguish a host that is simply not clean from a product failure. The
    # last dashboard the wait saw is the evidence for that distinction.
    other=$(jq -r --arg name "$compose_project" \
        '[(.projects // [])[] | select(.name != $name)] | length' \
        "$evidence_dir/dashboard.initial.json.tmp" 2>/dev/null || printf 0)
    case "$other" in
        ''|*[!0-9]*) other=0 ;;
    esac
    if [ "$other" -gt 0 ]; then
        not_clean=1
        fail "the host already manages $other Compose projects besides the fixture; this gate requires a clean host"
    fi
    fail "Agent registration or exact project discovery assertion failed"
fi
agent_id=$(jq -r '.hosts[0].id' "$evidence_dir/dashboard.initial.json")
project_uid=$(jq -r '.projects[0].uid' "$evidence_dir/dashboard.initial.json")
[ -n "$agent_id" ] && [ "$agent_id" != null ] && [ -n "$project_uid" ] && [ "$project_uid" != null ] ||
    fail "dashboard omitted Agent or project identity"
# The Agent derives a project UID as sha256(agent_id || NUL || working dir), so
# the harness can prove the project it is about to drive is the one it created.
[ "$project_uid" = "$(printf '%s\000%s' "$agent_id" "$runtime/projects" | sha256sum | awk '{ print $1 }')" ] ||
    fail "the discovered project uid does not match the uid derived from the fixture root"
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh -v "$runtime/agent:/agent" "$server_image" \
    -c 'rm -f /agent/join-token' >/dev/null
rm -f "$runtime/bootstrap/join-token"
agent_token_left=$(docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
    -v "$runtime/agent:/agent" "$server_image" -c '[ -e /agent/join-token ] && echo present || echo absent')
[ "$agent_token_left" = absent ] && [ ! -e "$runtime/bootstrap/join-token" ] || fail "Join Token cleanup failed"

api_json() {
    method=$1
    url=$2
    body=$3
    output=$4
    if [ "$method" = GET ]; then
        curl --fail --silent --show-error --max-time 10 --cacert "$runtime/bootstrap/server-ca.crt" "$url" >"$output.tmp"
    else
        curl --fail --silent --show-error --max-time 10 --cacert "$runtime/bootstrap/server-ca.crt" \
            -H 'Content-Type: application/json' -X "$method" --data "$body" "$url" >"$output.tmp"
    fi
    [ "$(wc -c <"$output.tmp" | awk '{ print $1 }')" -le 1048576 ] || fail "API response exceeded 1 MiB"
    mv "$output.tmp" "$output"
}

poll_operation_success() {
    operation_id=$1
    output=$2
    deadline=$(( $(date +%s) + 90 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/agents/$agent_id/operations/$operation_id" >"$output.tmp" 2>/dev/null &&
            [ "$(wc -c <"$output.tmp" | awk '{ print $1 }')" -le 1048576 ]; then
            mv "$output.tmp" "$output"
            status=$(jq -r '.status // empty' "$output")
            case "$status" in
                success) jq -e --arg id "$operation_id" '.operation_id == $id and .revision > 0 and .error == null' "$output" >/dev/null; return ;;
                failed|canceled|interrupted|rejected) fail "operation $operation_id reached terminal status $status" ;;
            esac
        fi
        sleep 1
    done
    fail "operation $operation_id did not reach success before its deadline"
}

compose_operation="clean-host-compose-up-$$"
compose_body=$(jq -cn --arg id "$compose_operation" --arg agent "$agent_id" --arg project "$project_uid" \
    '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')
api_json POST "$base_url/api/v1/operations" "$compose_body" "$evidence_dir/compose.accepted.json"
jq -e --arg id "$compose_operation" '.operation_id == $id and (.status == "requested" or .status == "dispatched" or .status == "running" or .status == "success") and .revision > 0' \
    "$evidence_dir/compose.accepted.json" >/dev/null || fail "Compose operation acceptance response is not exact"
poll_operation_success "$compose_operation" "$evidence_dir/compose.final.json"
docker ps -a --no-trunc --filter "label=com.docker.compose.project=$compose_project" \
    --format '{{.ID}}\t{{.Image}}\t{{.State}}\t{{.Status}}\t{{.Names}}\t{{.Labels}}' \
    >"$evidence_dir/compose.containers.tsv"
fixture_ids=$(docker ps -q --filter "label=com.docker.compose.project=$compose_project" --filter 'label=com.docker.compose.service=clean-host-fixture')
[ "$(printf '%s\n' "$fixture_ids" | awk 'NF { count++ } END { print count+0 }')" -eq 1 ] || fail "Compose operation did not create exactly one running fixture"
[ "$(docker inspect --format '{{.Image}}' "$fixture_ids")" = "$fixture_image" ] || fail "Compose used an image other than the exact fixture image"
[ "$(docker inspect --format '{{.State.Running}}' "$fixture_ids")" = true ] || fail "Compose fixture is not running"

backup_operation="clean-host-backup-$$"
backup_body=$(jq -cn --arg id "$backup_operation" '{operation_id:$id,relative_paths:["compose.yaml"]}')
api_json POST "$base_url/api/v1/projects/$project_uid/backups" "$backup_body" "$evidence_dir/backup.accepted.json"
jq -e --arg id "$backup_operation" '.operation_id == $id and .revision > 0' "$evidence_dir/backup.accepted.json" >/dev/null ||
    fail "backup.create acceptance response is not exact"
poll_operation_success "$backup_operation" "$evidence_dir/backup.final.json"
api_json GET "$base_url/api/v1/projects/$project_uid/backups" '' "$evidence_dir/backups.json"
jq -e --arg project "$project_uid" '
  length == 1 and .[0].project_uid == $project and .[0].trigger == "manual" and
  (.[0].backup_id | type == "string" and length > 0) and .[0].file_count == 1 and
  .[0].size_bytes > 0 and (.[0].manifest_sha256 | test("^[0-9a-f]{64}$"))
' "$evidence_dir/backups.json" >/dev/null || fail "backup create/list exact assertions failed"

capture_log "$agent" "$evidence_dir/agent.before-restart.log"
docker rm -f "$agent" >/dev/null
start_agent false >"$evidence_dir/agent.restarted.container-id"
wait_dashboard "$agent_id" "$evidence_dir/dashboard.reconnected.json" ||
    fail "Agent did not reconnect with the same durable identity and project"
[ "$(jq -r '.hosts[0].id' "$evidence_dir/dashboard.reconnected.json")" = "$agent_id" ] || fail "Agent identity changed across restart"
api_json GET "$base_url/api/v1/projects/$project_uid/backups" '' "$evidence_dir/backups.after-restart.json"
jq -e --arg project "$project_uid" 'length == 1 and .[0].project_uid == $project and .[0].trigger == "manual"' \
    "$evidence_dir/backups.after-restart.json" >/dev/null || fail "backup metadata was not available after Agent reconnect"

check_evidence_cap
{
    printf 'server_image_id=%s\n' "$server_image"
    printf 'agent_image_id=%s\n' "$agent_image"
    printf 'fixture_image_id=%s\n' "$fixture_image"
    printf 'release_version=%s\n' "$server_version"
    printf 'release_revision=%s\n' "$server_revision"
    printf 'compose_version=%s\n' "$compose_version"
    printf 'agent_id=%s\n' "$agent_id"
    printf 'project_uid=%s\n' "$project_uid"
    printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'registration=PASS\nproject_discovery=PASS\nlive_dashboard=PASS\ncompose_operation=PASS\nbackup_create_list=PASS\nidentity_reconnect=PASS\n'
    printf 'network_downloads=FORBIDDEN\nimage_builds=FORBIDDEN\nimage_pushes=FORBIDDEN\n'
} >"$evidence_dir/assertions.env"
sha256sum "$evidence_dir"/*.json "$evidence_dir"/*.env >"$evidence_dir/SHA256SUMS"
check_evidence_cap
completed=1
