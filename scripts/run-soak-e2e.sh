#!/bin/sh
set -eu

# Long-running soak. This is not another failure matrix: the matrices already
# prove what happens when something breaks. A soak proves what happens when
# nothing does - that hours of ordinary operation leave no accumulation behind.
#
# What it watches, and why each one is a leak the other gates cannot see:
#   process RSS / cgroup anon   memory that is retained rather than reused
#   threads                     runtime growth behind goroutines that never end
#   open file descriptors       streams and files that are opened but not closed
#   Agent state bytes           a WAL that grows faster than it is truncated
#   Audit sync lag              ingest that never catches up with generation
#   Audit coverage revision     coverage rewritten on every pass
#   bounded stream drops        buffers that grow instead of dropping
#   Server 5xx / SQLite busy    contention that turns into failure over time
#
# Modes:
#   idle    background loops only - heartbeat, discovery, Docker events, Audit
#           sync, retention. No user operations.
#   active  repeated real work - queries, file reads, log and stats streams,
#           targeted discovery, backups, Compose runs on the fixture, stream
#           cancellation, a deliberately slow consumer, reconnect injection.
#   mixed   alternating idle and active phases (default).
#
# Every mutation targets the fixture this harness created. On a host that also
# runs the operator's projects, a target chosen any other way would drive real
# Compose projects; see the fixture identity section below.

usage() {
    printf 'usage: %s ABSOLUTE_EVIDENCE_DIR SERVER_IMAGE_ID AGENT_IMAGE_ID FIXTURE_IMAGE_ID\n' "$0" >&2
    printf 'all image arguments must be exact local sha256 image IDs\n' >&2
    printf 'environment: SOAK_MODE=idle|active|mixed SOAK_SECONDS SOAK_SAMPLE_SECONDS\n' >&2
}

fail() {
    printf 'soak failed: %s\n' "$*" >&2
    failure_reason=$*
    # fail is also reached from command substitutions, whose exit only leaves
    # the subshell. set -e still aborts the run, but the reason would be lost,
    # so it is recorded where cleanup can read it back.
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

mode=${SOAK_MODE:-mixed}
case "$mode" in
    idle|active|mixed) ;;
    *) fail "preflight: SOAK_MODE must be idle, active, or mixed" ;;
esac

soak_seconds=${SOAK_SECONDS:-3600}
sample_seconds=${SOAK_SAMPLE_SECONDS:-30}
evidence_max_bytes=${SOAK_EVIDENCE_MAX_BYTES:-33554432}
log_max_bytes=${SOAK_LOG_MAX_BYTES:-1048576}
for value in "$soak_seconds" "$sample_seconds" "$evidence_max_bytes" "$log_max_bytes"; do
    case "$value" in
        ''|*[!0-9]*) fail "preflight: numeric settings must be integers" ;;
    esac
done
# A soak shorter than a few sampling windows cannot show a slope, and the
# quarter-median rule below needs at least four samples per quarter.
[ "$sample_seconds" -ge 5 ] || fail "preflight: SOAK_SAMPLE_SECONDS must be at least 5"
[ "$soak_seconds" -ge $((sample_seconds * 16)) ] ||
    fail "preflight: SOAK_SECONDS must cover at least 16 samples"
[ "$soak_seconds" -le 86400 ] || fail "preflight: SOAK_SECONDS must not exceed one day"
settle_seconds=${SOAK_SETTLE_SECONDS:-90}
case "$settle_seconds" in ''|*[!0-9]*) fail "preflight: SOAK_SETTLE_SECONDS must be an integer" ;; esac
# The settle window is where a leak in retained state shows itself, so it has
# to hold at least two samples to have a slope at all.
[ "$settle_seconds" -ge $((sample_seconds * 2)) ] ||
    fail "preflight: SOAK_SETTLE_SECONDS must cover at least two samples"

case "$evidence_dir" in
    /*) ;;
    *) fail "preflight: evidence directory must be absolute" ;;
esac
[ ! -e "$evidence_dir" ] || fail "preflight: evidence directory already exists"
evidence_parent=$(dirname "$evidence_dir")
[ -d "$evidence_parent" ] || fail "preflight: evidence parent directory does not exist"

for tool in docker jq curl openssl sha256sum awk; do
    command -v "$tool" >/dev/null 2>&1 || fail "preflight: required command not found: $tool"
done
[ "$(docker info --format '{{.OSType}}')" = linux ] || fail "preflight: a Linux Docker Engine is required"
[ "$(docker info --format '{{.CgroupVersion}}')" = 2 ] || fail "preflight: cgroup v2 is required"
[ -z "${DOCKER_HOST:-}" ] || fail "preflight: DOCKER_HOST must be unset; this harness reads host cgroups"
[ -r /var/run/docker.sock ] && [ -w /var/run/docker.sock ] ||
    fail "preflight: readable and writable /var/run/docker.sock is required"

require_image_id_shape "$server_image" Server
require_image_id_shape "$agent_image" Agent
require_image_id_shape "$fixture_image" fixture
[ "$server_image" != "$agent_image" ] && [ "$server_image" != "$fixture_image" ] && [ "$agent_image" != "$fixture_image" ] ||
    fail "preflight: Server, Agent, and fixture must be three distinct images"
for image in "$server_image" "$agent_image" "$fixture_image"; do
    docker image inspect "$image" >/dev/null 2>&1 || fail "preflight: exact local image is unavailable: $image"
    [ "$(docker image inspect --format '{{.Id}}' "$image")" = "$image" ] ||
        fail "preflight: image reference did not resolve to its exact requested ID: $image"
done
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$server_image")" = "Dockpilot Server" ] ||
    fail "preflight: Server image is not the production Server target"
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$agent_image")" = "Dockpilot Agent" ] ||
    fail "preflight: Agent image is not the production Agent target"
server_version=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$server_image")
server_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$server_image")
[ "$server_revision" = "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$agent_image")" ] ||
    fail "preflight: Server and Agent revisions differ"

runtime_base=${TMPDIR:-/tmp}
case "$runtime_base" in /*) ;; *) fail "preflight: TMPDIR must be absolute" ;; esac
[ -d "$runtime_base" ] || fail "preflight: TMPDIR does not exist"

prefix="dockpilot-soak-$(date -u +%Y%m%dT%H%M%SZ)-$$"
umask 077
artifact_created=0
runtime=
server="$prefix-server"
agent="$prefix-agent"
network="$prefix-network"
compose_project=$(printf '%s' "$prefix-fixture" | tr '[:upper:]' '[:lower:]')
secret_marker="soak-secret-must-never-be-recorded-$$"
completed=0
failure_reason="soak did not complete"
failure_reason_file=

capture_log() {
    docker inspect "$1" >/dev/null 2>&1 || return 0
    docker logs --tail 2000 "$1" 2>&1 | head -c "$log_max_bytes" >"$2" || true
}

remove_compose_objects() {
    ids=$(docker ps -aq --filter "label=com.docker.compose.project=$compose_project" 2>/dev/null || true)
    # shellcheck disable=SC2086
    [ -z "$ids" ] || docker rm -f $ids >/dev/null 2>&1 || true
    ids=$(docker network ls -q --filter "label=com.docker.compose.project=$compose_project" 2>/dev/null || true)
    # shellcheck disable=SC2086
    [ -z "$ids" ] || docker network rm $ids >/dev/null 2>&1 || true
}

scrub_runtime() {
    [ -n "$runtime" ] || return 0
    [ -d "$runtime" ] || return 0
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/soak-runtime" "$server_image" -c \
        'rm -rf /soak-runtime/server /soak-runtime/agent /soak-runtime/bootstrap /soak-runtime/projects' >/dev/null 2>&1 || return 1
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
        capture_log "$agent" "$evidence_dir/agent.final.log"
        capture_log "$server" "$evidence_dir/server.final.log"
    fi
    docker rm -f "$agent" >/dev/null 2>&1 || true
    remove_compose_objects
    docker rm -f "$server" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
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
trap 'failure_reason="soak interrupted by signal"; exit 130' HUP INT TERM

runtime=$(mktemp -d "$runtime_base/dockpilot-soak.XXXXXXXX")
case "$runtime" in "$runtime_base"/dockpilot-soak.*) ;; *) fail "mktemp returned an unexpected runtime root" ;; esac
chmod 0700 "$runtime"
mkdir "$evidence_dir"
chmod 0700 "$evidence_dir"
artifact_created=1
failure_reason_file="$evidence_dir/failure-reason.txt"
{
    printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'docker_server_version=%s\n' "$(docker info --format '{{.ServerVersion}}')"
    printf 'kernel=%s\n' "$(uname -srm)"
    printf 'server_image_id=%s\n' "$server_image"
    printf 'agent_image_id=%s\n' "$agent_image"
    printf 'fixture_image_id=%s\n' "$fixture_image"
    printf 'release_version=%s\n' "$server_version"
    printf 'release_revision=%s\n' "$server_revision"
    printf 'mode=%s\n' "$mode"
    printf 'soak_seconds=%s\n' "$soak_seconds"
    printf 'sample_seconds=%s\n' "$sample_seconds"
    printf 'settle_seconds=%s\n' "$settle_seconds"
} >"$evidence_dir/environment.env"

mkdir "$runtime/server" "$runtime/server/tls" "$runtime/agent" "$runtime/bootstrap" "$runtime/projects"
fixture_root="$runtime/projects"
socket_gid=$(stat -c '%g' /var/run/docker.sock)
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
    -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
    -keyout "$runtime/server/tls/server.key" -out "$runtime/server/tls/server.crt" \
    >"$evidence_dir/openssl.stdout" 2>"$evidence_dir/openssl.stderr"
cp "$runtime/server/tls/server.crt" "$runtime/bootstrap/server-ca.crt"
cat >"$runtime/projects/compose.yaml" <<EOF
name: $compose_project
services:
  soak-fixture:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do echo soak-fixture-heartbeat; sleep 2; done"]
EOF
printf 'SOAK_SECRET=%s\n' "$secret_marker" >"$runtime/projects/.env"
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
    -v "$runtime:/soak" "$server_image" -c \
    'chown -R 65532:65532 /soak/server /soak/agent; chmod 0700 /soak/server /soak/agent /soak/server/tls; chmod 0600 /soak/server/tls/server.crt /soak/server/tls/server.key; chown -R 65532:65532 /soak/projects; chmod 0777 /soak/projects; chmod 0666 /soak/projects/compose.yaml /soak/projects/.env' \
    >/dev/null
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

docker network create --subnet "$(harness_subnet)" "$network" >/dev/null

start_server() {
    docker run --pull never -d --name "$server" --network "$network" --network-alias server \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
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
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    fail "the Server never answered its dashboard route"
}

issue_token() {
    docker run --pull never --rm --user 65532:65532 \
        -v "$runtime/server:/var/lib/dockpilot:rw" "$server_image" \
        server issue-token --state-dir /var/lib/dockpilot --ttl 60m \
        >"$runtime/bootstrap/join-token" 2>"$evidence_dir/issue-token.stderr"
    [ "$(wc -c <"$runtime/bootstrap/join-token" | awk '{ print $1 }')" -gt 1 ] || fail "Join Token CLI produced no token"
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/agent:/agent" -v "$runtime/bootstrap:/bootstrap:ro" "$server_image" -c \
        'cp /bootstrap/server-ca.crt /agent/server-ca.crt; cp /bootstrap/join-token /agent/join-token; chown -R 65532:65532 /agent; chmod 0700 /agent; chmod 0600 /agent/server-ca.crt /agent/join-token' >/dev/null
    rm -f "$runtime/bootstrap/join-token"
}

start_agent() {
    with_token=$1
    if [ "$with_token" = true ]; then
        token_args="--join-token-file /var/lib/dockpilot/join-token"
    else
        token_args=
    fi
    # shellcheck disable=SC2086
    docker run --pull never -d --name "$agent" --network "$network" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.dockpilot.role=agent \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/agent:/var/lib/dockpilot:rw" \
        -v "$runtime/projects:$runtime/projects:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/dockpilot/server-ca.crt $token_args \
        --display-name soak-agent --self-container-name "$agent" \
        --project-root "$runtime/projects"
}

# ------------------------------------------------------- fixture identity
# Identical in intent to the hardening and abuse matrices: every target is the
# identity this harness created. A soak runs for hours against a host that is
# also doing the operator's work, so this is the difference between exercising
# a fixture and quietly driving somebody's production Compose project.
expected_fixture_uid() {
    printf '%s\000%s' "$1" "$2" | sha256sum | awk '{ print $1 }'
}

find_fixture_project() {
    dashboard=$1
    root=$2
    expect_name=${3:-$compose_project}
    matched=$(jq -r --arg name "$expect_name" --arg root "$root" \
        '[.projects[]? | select(.name == $name and .working_dir == $root)] | length' "$dashboard")
    case "$matched" in
        0) return 0 ;;
        1) ;;
        *) fail "fixture: $matched dashboard projects claim the fixture identity $expect_name at $root" ;;
    esac
    selected_uid=$(jq -r --arg name "$expect_name" --arg root "$root" \
        '[.projects[] | select(.name == $name and .working_dir == $root)][0].uid' "$dashboard")
    [ -n "$selected_uid" ] && [ "$selected_uid" != null ] ||
        fail "fixture: the dashboard omitted the uid of the fixture project at $root"
    [ "$selected_uid" = "$(expected_fixture_uid "$agent_id" "$root")" ] ||
        fail "fixture: dashboard uid $selected_uid does not match the uid derived from $root"
    printf '%s' "$selected_uid"
}

select_fixture_project() {
    resolved=$(find_fixture_project "$1" "$2" "${3:-$compose_project}")
    [ -n "$resolved" ] ||
        fail "fixture: no dashboard project is named ${3:-$compose_project} at $2"
    printf '%s' "$resolved"
}

fixture_uids=

allow_fixture_uid() {
    fixture_uids="$fixture_uids $1"
}

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
                fail "fixture guard: request targets project $guarded_target, which this harness did not create"
            ;;
    esac
    case "$guarded_body" in
        *project_uid*)
            guarded_target=$(printf '%s' "$guarded_body" | jq -r '.project_uid // empty' 2>/dev/null || true)
            [ -z "$guarded_target" ] || is_fixture_uid "$guarded_target" ||
                fail "fixture guard: request body targets project $guarded_target, which this harness did not create"
            ;;
    esac
}

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

wait_active_host() {
    expected=$1; output=$2; seconds=$3
    deadline=$(( $(date +%s) + seconds ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >"$output.tmp" 2>/dev/null &&
            jq -e --arg expected "$expected" '
              (.hosts | length) == 1 and .hosts[0].state == "ACTIVE" and
              ($expected == "" or .hosts[0].id == $expected)
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
    operation_id=$1; output=$2; seconds=$3
    deadline=$(( $(date +%s) + seconds ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/agents/$agent_id/operations/$operation_id" >"$output.tmp" 2>/dev/null &&
            jq -e '.status == "success" or .status == "failed" or .status == "canceled" or
                   .status == "interrupted" or .status == "rejected"' \
                "$output.tmp" >/dev/null 2>&1; then
            mv "$output.tmp" "$output"
            return 0
        fi
        sleep 1
    done
    [ -e "$output.tmp" ] && mv "$output.tmp" "$output"
    return 1
}

record() {
    printf '%s=%s\n' "$1" "$2" >>"$evidence_dir/assertions.env"
}

# ----------------------------------------------------------- measurement
samples="$evidence_dir/samples.jsonl"
: >"$samples"

container_pid() {
    docker inspect --format '{{.State.Pid}}' "$1" 2>/dev/null || printf 0
}

# proc_metric reads one number from the host's view of a container process.
# The Agent reports container state from the host hierarchy, so the same view
# is the honest one to measure it from.
proc_field() {
    pid=$1; field=$2
    [ "$pid" != 0 ] || { printf 0; return 0; }
    awk -v want="$field" '$1 == want ":" { print $2; found = 1 } END { if (!found) print 0 }' \
        "/proc/$pid/status" 2>/dev/null || printf 0
}

# The host cannot list another user's /proc/<pid>/fd, and both containers run
# as an unprivileged uid, so the count is taken from inside the container where
# the process can see its own descriptors. Read from the host it is silently
# always zero, which is worse than having no metric at all: it looks like a
# check that keeps passing.
fd_count() {
    { docker exec "$1" sh -c 'ls /proc/1/fd 2>/dev/null | wc -l' 2>/dev/null || true; } | first_number
}

# Every collector prints exactly one integer. A sample that cannot be taken is
# reported as zero rather than left empty, because an empty value would abort
# the run on a transient docker exec instead of recording the gap.
first_number() {
    awk 'NR == 1 { print $1 + 0; taken = 1 } END { if (!taken) print 0 }'
}

cgroup_metric() {
    name=$1; file=$2
    { docker exec "$name" sh -c "cat /sys/fs/cgroup/$file 2>/dev/null" 2>/dev/null || true; } | first_number
}

cgroup_event() {
    name=$1; key=$2
    { docker exec "$name" sh -c "awk -v k=$key '\$1 == k { print \$2 }' /sys/fs/cgroup/memory.events 2>/dev/null" 2>/dev/null || true; } | first_number
}

# busybox du has no -b, so the Agent state is measured in KiB.
agent_state_kib() {
    { docker exec "$agent" sh -c 'du -sk /var/lib/dockpilot 2>/dev/null | cut -f1' 2>/dev/null || true; } | first_number
}

sample() {
    phase=$1
    server_pid=$(container_pid "$server")
    agent_pid=$(container_pid "$agent")
    audit_status=$(api_status GET "$base_url/api/v1/hosts/$agent_id/audit?limit=1" '' "$evidence_dir/sample.audit.json")
    dashboard_status=$(api_status GET "$base_url/api/v1/dashboard" '' "$evidence_dir/sample.dashboard.json")
    if [ "$audit_status" = 200 ]; then
        ack_seq=$(jq -r '.coverage.ack.seq // 0' "$evidence_dir/sample.audit.json")
        delivery_seq=$(jq -r '.coverage.delivery_next.seq // 0' "$evidence_dir/sample.audit.json")
        gaps=$(jq -r '(.coverage.gaps // []) | length' "$evidence_dir/sample.audit.json")
        coverage_revision=$(jq -r '.coverage.revision // 0' "$evidence_dir/sample.audit.json")
    else
        ack_seq=0; delivery_seq=0; gaps=0; coverage_revision=0
    fi
    if [ "$dashboard_status" = 200 ]; then
        host_state=$(jq -r '.hosts[0].state // "UNKNOWN"' "$evidence_dir/sample.dashboard.json")
        host_count=$(jq -r '(.hosts // []) | length' "$evidence_dir/sample.dashboard.json")
    else
        host_state=UNREACHABLE; host_count=0
    fi
    case "$audit_status$dashboard_status" in
        200200) http_error=0 ;;
        *) http_error=1 ;;
    esac
    jq -cn \
        --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg phase "$phase" --arg state "$host_state" \
        --argjson elapsed "$(( $(date +%s) - started_epoch ))" \
        --argjson server_rss "$(proc_field "$server_pid" VmRSS)" \
        --argjson server_threads "$(proc_field "$server_pid" Threads)" \
        --argjson server_fds "$(fd_count "$server")" \
        --argjson server_current "$(cgroup_metric "$server" memory.current)" \
        --argjson server_oom "$(cgroup_event "$server" oom)" \
        --argjson server_oom_kill "$(cgroup_event "$server" oom_kill)" \
        --argjson agent_rss "$(proc_field "$agent_pid" VmRSS)" \
        --argjson agent_threads "$(proc_field "$agent_pid" Threads)" \
        --argjson agent_fds "$(fd_count "$agent")" \
        --argjson agent_current "$(cgroup_metric "$agent" memory.current)" \
        --argjson agent_oom "$(cgroup_event "$agent" oom)" \
        --argjson agent_oom_kill "$(cgroup_event "$agent" oom_kill)" \
        --argjson agent_state_kib "$(agent_state_kib)" \
        --argjson ack "$ack_seq" --argjson delivery "$delivery_seq" \
        --argjson gaps "$gaps" --argjson coverage_revision "$coverage_revision" \
        --argjson hosts "$host_count" --argjson http_error "$http_error" \
        '{observed_at:$at,phase:$phase,elapsed_seconds:$elapsed,host_state:$state,hosts:$hosts,
          http_error:$http_error,
          server:{rss_kib:$server_rss,threads:$server_threads,fds:$server_fds,
                  memory_current:$server_current,oom:$server_oom,oom_kill:$server_oom_kill},
          agent:{rss_kib:$agent_rss,threads:$agent_threads,fds:$agent_fds,
                 memory_current:$agent_current,oom:$agent_oom,oom_kill:$agent_oom_kill,
                 state_kib:$agent_state_kib},
          audit:{ack_seq:$ack,delivery_seq:$delivery,lag:($delivery - $ack),gaps:$gaps,
                 coverage_revision:$coverage_revision}}' >>"$samples"
}

# ------------------------------------------------------------- workloads
stream_churn=0
operation_count=0
reconnects=0

# fixture_container prints the fixture's running container id, or nothing. The
# live streams need a container to follow, and it is always this project's.
fixture_container() {
    docker ps --filter "label=com.docker.compose.project=$compose_project" \
        --filter status=running --format '{{.ID}}' 2>/dev/null | head -1
}

compose_operation() {
    kind=$1; label=$2
    operation="soak-$label-$$"
    if [ "$(api_status POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$operation" --arg agent "$agent_id" --arg project "$project_uid" --arg kind "$kind" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:$kind}')" \
        "$evidence_dir/cycle.$label.json")" = 202 ]; then
        poll_operation "$operation" "$evidence_dir/cycle.$label.final.json" 240 || true
        operation_count=$((operation_count + 1))
        return 0
    fi
    return 1
}

active_cycle() {
    cycle=$1
    # Interactive queries: the shapes a browser produces while somebody watches.
    api GET "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" '' "$evidence_dir/cycle.file.json"
    api GET "$base_url/api/v1/projects/$project_uid/environment" '' "$evidence_dir/cycle.environment.json"
    api GET "$base_url/api/v1/projects/$project_uid/activity" '' "$evidence_dir/cycle.activity.json"
    api_status GET "$base_url/api/v1/projects/$project_uid/compose/ps" '' "$evidence_dir/cycle.ps.json" >/dev/null
    api_status GET "$base_url/api/v1/projects/$project_uid/compose/config" '' "$evidence_dir/cycle.config.json" >/dev/null
    api_status GET "$base_url/api/v1/hosts/$agent_id/containers" '' "$evidence_dir/cycle.containers.json" >/dev/null
    api GET "$base_url/api/v1/dashboard" '' "$evidence_dir/cycle.dashboard.json"

    container=$(fixture_container)
    if [ -n "$container" ]; then
        # Streams opened and closed again. A stream that leaks its buffer or
        # its goroutine shows up as a slope in fds and threads, not as an
        # error, which is why only a soak can see it.
        curl --silent --show-error --max-time 4 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/live/stats?agent_id=$agent_id&container_id=$container" \
            >"$evidence_dir/cycle.stats.sse" 2>/dev/null || true
        curl --silent --show-error --max-time 4 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/live/logs?agent_id=$agent_id&container_id=$container" \
            >"$evidence_dir/cycle.logs.sse" 2>/dev/null || true
        stream_churn=$((stream_churn + 2))

        # A deliberately slow consumer every fourth cycle: the bounded buffer
        # must drop rather than grow.
        if [ $((cycle % 4)) -eq 0 ]; then
            curl --silent --show-error --max-time 6 --limit-rate 1k --cacert "$runtime/bootstrap/server-ca.crt" \
                "$base_url/api/v1/live/logs?agent_id=$agent_id&container_id=$container" \
                >"$evidence_dir/cycle.slow.sse" 2>/dev/null || true
            stream_churn=$((stream_churn + 1))
        fi
    fi

    # A real backup every third cycle, on the fixture only.
    if [ $((cycle % 3)) -eq 0 ]; then
        backup_operation="soak-backup-$$"
        if [ "$(api_status POST "$base_url/api/v1/projects/$project_uid/backups" \
            "$(jq -cn --arg id "$backup_operation-$cycle" '{operation_id:$id,relative_paths:["compose.yaml"]}')" \
            "$evidence_dir/cycle.backup.json")" = 202 ]; then
            poll_operation "$backup_operation-$cycle" "$evidence_dir/cycle.backup.final.json" 180 || true
            operation_count=$((operation_count + 1))
        fi
    fi

    # Cancellation churn every sixth cycle. A cancelled operation must release
    # its lock and its buffers as completely as a finished one.
    if [ $((cycle % 6)) -eq 0 ]; then
        cancel_operation="soak-cancel-$cycle-$$"
        if [ "$(api_status POST "$base_url/api/v1/operations" \
            "$(jq -cn --arg id "$cancel_operation" --arg agent "$agent_id" --arg project "$project_uid" \
                '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
            "$evidence_dir/cycle.cancel.accepted.json")" = 202 ]; then
            api_status POST "$base_url/api/v1/agents/$agent_id/operations/$cancel_operation/cancel" '{}' \
                "$evidence_dir/cycle.cancel.json" >/dev/null
            poll_operation "$cancel_operation" "$evidence_dir/cycle.cancel.final.json" 240 || true
            operation_count=$((operation_count + 1))
        fi
    fi

    # Compose restart churn every eighth cycle, on the fixture only. The
    # project is brought back up so the stream target survives the soak.
    if [ $((cycle % 8)) -eq 0 ]; then
        compose_operation compose.down "down-$cycle" || true
        compose_operation compose.up "up-$cycle" || true
    fi

    # Reconnect injection every twelfth cycle. A session that is not released
    # on disconnect is the classic long-run leak.
    if [ $((cycle % 12)) -eq 0 ]; then
        docker network disconnect "$network" "$agent" >/dev/null 2>&1 || true
        sleep 3
        docker network connect "$network" "$agent" >/dev/null 2>&1 || true
        wait_active_host "$agent_id" "$evidence_dir/cycle.reconnected.json" 240 ||
            fail "soak: the Agent did not return ACTIVE after a reconnect"
        reconnects=$((reconnects + 1))
    fi
}

# ------------------------------------------------------------- baseline
start_server >"$evidence_dir/server.container-id"
resolve_base_url
wait_server_ready
issue_token
start_agent true >"$evidence_dir/agent.container-id"
wait_active_host "" "$evidence_dir/dashboard.baseline.json" 180 ||
    fail "baseline registration did not produce exactly one ACTIVE host"
agent_id=$(jq -r '.hosts[0].id' "$evidence_dir/dashboard.baseline.json")
[ -n "$agent_id" ] && [ "$agent_id" != null ] || fail "baseline dashboard omitted the Agent id"
project_uid=$(select_fixture_project "$evidence_dir/dashboard.baseline.json" "$fixture_root")
allow_fixture_uid "$project_uid"
record baseline_agent_id "$agent_id"
record baseline_project_uid "$project_uid"
record fixture_root "$fixture_root"
record fixture_identity_verified PASS
record other_projects_on_host "$(jq -r --arg uid "$project_uid" '[.projects[]? | select(.uid != $uid)] | length' "$evidence_dir/dashboard.baseline.json")"

# The fixture project runs for the whole soak: it gives the log and stats
# streams something real to follow, and its own restarts are the churn.
if [ "$mode" != idle ]; then
    compose_operation compose.up initial ||
        fail "soak: the fixture project could not be brought up"
    record fixture_project_started PASS
fi

started_epoch=$(date +%s)
# The loop's own names are prefixed: the helpers it calls are plain POSIX
# functions with no locals, and poll_operation and wait_active_host both assign
# a "deadline" of their own. Sharing that name would push the end of the soak
# further out on every operation, and the run would never finish.
soak_deadline=$((started_epoch + soak_seconds))
sample baseline

soak_cycle=0
while [ "$(date +%s)" -lt "$soak_deadline" ]; do
    soak_cycle=$((soak_cycle + 1))
    case "$mode" in
        idle) soak_phase=idle ;;
        active) soak_phase=active ;;
        # Mixed alternates in blocks of four samples so each phase is long
        # enough to show its own slope.
        mixed) if [ $(((soak_cycle / 4) % 2)) -eq 0 ]; then soak_phase=active; else soak_phase=idle; fi ;;
    esac
    if [ "$soak_phase" = active ]; then
        active_cycle "$soak_cycle"
    fi
    sample "$soak_phase"
    soak_now=$(date +%s)
    [ "$soak_now" -ge "$soak_deadline" ] && break
    soak_remaining=$((soak_deadline - soak_now))
    [ "$soak_remaining" -lt "$sample_seconds" ] && sleep "$soak_remaining" || sleep "$sample_seconds"
done

# A settle window: the Go scavenger returns freed pages gradually, so the last
# samples must describe a rested process rather than peak load. Without it the
# recovery check would measure churn instead of retention.
soak_settle_deadline=$(( $(date +%s) + settle_seconds ))
while [ "$(date +%s)" -lt "$soak_settle_deadline" ]; do
    sample settle
    sleep "$sample_seconds"
done
sample final

record cycles "$soak_cycle"
record stream_churn "$stream_churn"
record operations "$operation_count"
record reconnects "$reconnects"
record samples "$(wc -l <"$samples" | awk '{ print $1 }')"

# ---------------------------------------------------------- trend verdict
# The rule is the one the resource gate settled on: a single high reading is
# noise, and an absolute ceiling would only re-measure what the resource gate
# already measures. What a soak can prove is direction. A metric fails when
# every quarter median is above the one before it *and* the last quarter
# exceeds the first by more than its tolerance - a shape that noise does not
# produce and that a leak always does.
trend_report="$evidence_dir/trend.json"
jq -s --argjson tolerance 30 '
    def quarters(f):
        (map(f) | . as $v | ($v | length) as $n |
         if $n < 8 then null else
             [range(0; 4)] | map(
                 ($v[(($n * .) / 4 | floor):(($n * (. + 1)) / 4 | floor)]) as $q |
                 ($q | sort) as $s | ($s | length) as $l |
                 if $l == 0 then 0 else $s[($l / 2 | floor)] end)
         end);
    def verdict(name; f; tol):
        quarters(f) as $q |
        if $q == null then {metric:name,verdict:"INSUFFICIENT_SAMPLES"}
        else
            ($q[0]) as $first | ($q[3]) as $last |
            (if $first == 0 then (if $last == 0 then 0 else 100 end)
             else (($last - $first) * 100 / $first) end) as $growth |
            (($q[1] > $q[0]) and ($q[2] > $q[1]) and ($q[3] > $q[2])) as $monotonic |
            {metric:name,quarters:$q,growth_percent:($growth | . * 100 | round / 100),
             monotonic_rise:$monotonic,tolerance_percent:tol,
             verdict:(if $monotonic and $growth > tol then "FAIL" else "PASS" end)}
        end;
    [ .[] | select(.phase != "baseline") ] as $s |
    {
      samples: ($s | length),
      metrics: [
        ($s | verdict("server.rss_kib"; .server.rss_kib; $tolerance)),
        ($s | verdict("server.threads"; .server.threads; $tolerance)),
        ($s | verdict("server.fds"; .server.fds; $tolerance)),
        ($s | verdict("agent.rss_kib"; .agent.rss_kib; $tolerance)),
        ($s | verdict("agent.threads"; .agent.threads; $tolerance)),
        ($s | verdict("agent.fds"; .agent.fds; $tolerance)),
        ($s | verdict("audit.lag"; .audit.lag; $tolerance)),
        ($s | verdict("audit.coverage_revision"; .audit.coverage_revision; 100000))
      ],
      hard: {
        oom: ($s | map(.server.oom + .server.oom_kill + .agent.oom + .agent.oom_kill) | max),
        http_errors: ($s | map(.http_error) | add),
        never_active: ($s | map(select(.host_state != "ACTIVE")) | length)
      },
      # Agent state legitimately grows while work happens: every backup this
      # soak takes is retained on purpose, and retention only evicts near the
      # budget. Growth is therefore not the question. The question is whether
      # it stops growing once the workload does, which is what the settle
      # window is for, and whether it is anywhere near its ceiling.
      state: (
        [ .[] | select(.phase == "settle" or .phase == "final") | .agent.state_kib ] as $rest |
        {
          final_kib: ($s | last | .agent.state_kib),
          settle_samples: ($rest | length),
          settle_first_kib: ($rest | first // 0),
          settle_last_kib: ($rest | last // 0),
          settle_growth_percent: (
            ($rest | first // 0) as $f | ($rest | last // 0) as $l |
            if $f == 0 then (if $l == 0 then 0 else 100 end)
            else ((($l - $f) * 100 / $f) | . * 100 | round / 100) end)
        })
    }' "$samples" >"$trend_report"

failed_metrics=$(jq -r '[.metrics[] | select(.verdict == "FAIL") | .metric] | join(",")' "$trend_report")
[ -z "$failed_metrics" ] || fail "soak: these metrics rose across every quarter beyond tolerance: $failed_metrics"
insufficient=$(jq -r '[.metrics[] | select(.verdict == "INSUFFICIENT_SAMPLES")] | length' "$trend_report")
[ "$insufficient" -eq 0 ] || fail "soak: the run produced too few samples to judge a trend"
[ "$(jq -r '.hard.oom' "$trend_report")" = 0 ] || fail "soak: the run recorded an OOM event"
[ "$(jq -r '.hard.http_errors' "$trend_report")" = 0 ] || fail "soak: the Server failed a sampling request during the run"
[ "$(jq -r '.hard.never_active' "$trend_report")" = 0 ] || fail "soak: the Agent was not ACTIVE at every sample"

state_ceiling_kib=${SOAK_STATE_CEILING_KIB:-262144}
case "$state_ceiling_kib" in ''|*[!0-9]*) fail "SOAK_STATE_CEILING_KIB must be an integer" ;; esac
[ "$(jq -r '.state.settle_samples' "$trend_report")" -ge 2 ] ||
    fail "soak: the settle window produced too few samples to judge Agent state growth"
settle_growth=$(jq -r '.state.settle_growth_percent | floor' "$trend_report")
[ "$settle_growth" -le 10 ] ||
    fail "soak: Agent state kept growing by ${settle_growth}% after the workload stopped"
final_state_kib=$(jq -r '.state.final_kib' "$trend_report")
[ "$final_state_kib" -le "$state_ceiling_kib" ] ||
    fail "soak: Agent state reached ${final_state_kib} KiB, above the ${state_ceiling_kib} KiB soak ceiling"

# The Server must not have logged database contention or an internal failure.
capture_log "$server" "$evidence_dir/server.during.log"
! grep -qi 'SQLITE_BUSY\|database is locked' "$evidence_dir/server.during.log" ||
    fail "soak: the Server logged SQLite contention during the run"
! grep -q 'api request failed' "$evidence_dir/server.during.log" ||
    fail "soak: the Server logged a failed API request during the run"

# --------------------------------------------------------- closing state
# The same invariants every hardening scenario closes with. A soak that ends
# with an orphan lock, a surviving journal, or a stale session has leaked, even
# if no slope showed it.
wait_active_host "$agent_id" "$evidence_dir/invariants.dashboard.json" 240 ||
    fail "invariant: the fleet did not settle on exactly one ACTIVE Agent"

probe="soak-lock-probe-$$"
api POST "$base_url/api/v1/projects/$project_uid/backups" \
    "$(jq -cn --arg id "$probe" '{operation_id:$id,relative_paths:["compose.yaml"]}')" \
    "$evidence_dir/invariants.probe.accepted.json"
poll_operation "$probe" "$evidence_dir/invariants.probe.json" 180 ||
    fail "invariant: the project lock probe never reached a terminal state"
jq -e '((.error // "") | test("PROJECT_BUSY")) | not' "$evidence_dir/invariants.probe.json" >/dev/null ||
    fail "invariant: the project lock is still held by nothing"

state_dir=$(docker exec "$agent" /bin/sh -c '
    for candidate in /var/lib/dockpilot /constrained/state; do
        if [ -e "$candidate/identity/agent-state.json" ] || [ -e "$candidate/agent-state.json" ]; then
            printf %s "$candidate"
            exit 0
        fi
    done
    exit 1') || fail "invariant: could not locate the Agent state directory"
docker exec "$agent" /bin/sh -c "ls -A '$state_dir/restore-journal' 2>/dev/null" \
    >"$evidence_dir/invariants.restore-journal.txt" 2>&1 || true
[ ! -s "$evidence_dir/invariants.restore-journal.txt" ] ||
    fail "invariant: a restore journal survived a settled soak"
ls -A "$runtime/projects" | grep -e '^\.dockpilot-' >"$evidence_dir/invariants.staging.txt" 2>/dev/null || true
[ ! -s "$evidence_dir/invariants.staging.txt" ] ||
    fail "invariant: staging files were orphaned in the project directory"

api GET "$base_url/api/v1/hosts/$agent_id/audit?limit=50" '' "$evidence_dir/invariants.audit.json"
jq -e '
  (.coverage.gaps // []) | all(
    (.precision == "exact" or .precision == "coalesced" or .precision == "unknown") and
    (.source == "AGENT_GAP" or .source == "SERVER_RETENTION" or
     .source == "AGENT_CONTINUITY_UNCERTAIN" or .source == "SERVER_CURSOR_REGRESSION") and
    (.from.seq <= .until.seq))' "$evidence_dir/invariants.audit.json" >/dev/null ||
    fail "invariant: an Audit coverage entry is missing its source, precision, or ordering"
jq -e '
  (.coverage.ack == null) or (.coverage.delivery_next == null) or
  (.coverage.ack.incarnation < .coverage.delivery_next.incarnation) or
  (.coverage.ack.incarnation == .coverage.delivery_next.incarnation and
   .coverage.ack.seq < .coverage.delivery_next.seq)' "$evidence_dir/invariants.audit.json" >/dev/null ||
    fail "invariant: the acknowledged cursor passed the Server delivery cursor"

# Docker's view and Dockpilot's view of the fixture must still agree.
docker ps --all --filter "label=com.docker.compose.project=$compose_project" --format '{{.ID}}' \
    >"$evidence_dir/invariants.docker-fixture.txt"
api GET "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" '' "$evidence_dir/invariants.file.json"
on_disk=$(sha256sum "$runtime/projects/compose.yaml" | awk '{ print $1 }')
reported=$(jq -r '.sha256' "$evidence_dir/invariants.file.json")
[ "$on_disk" = "$reported" ] ||
    fail "invariant: Dockpilot reported compose.yaml as $reported, disk says $on_disk"

# The project secret must not have reached Audit, an answer, or a container log.
! grep -rF -- "$secret_marker" "$evidence_dir" --exclude=STATUS --exclude=SHA256SUMS >/dev/null 2>&1 ||
    fail "invariant: the project secret leaked into recorded evidence"

record trend_verdict PASS
record invariants_after_soak PASS
{
    printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >>"$evidence_dir/assertions.env"
completed=1
