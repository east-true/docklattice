#!/bin/sh
set -eu

# Aggressive v1 hardening matrix. Every case drives a real Server container, a
# real Agent container, and a real Docker workload into a failure the product
# claims to survive, then asserts the claim rather than the absence of a crash.
#
# Cases and the contract each one checks:
#   agent-sigkill      11.5   unclean shutdown advances the incarnation and
#                             leaves AUDIT_CONTINUITY_UNCERTAIN
#   operation-interrupt 11.5  an operation still running when the Agent is killed
#                             comes back terminal and admits partial effects
#   server-sigkill     6.4    a killed Server keeps its identity and archive and
#                             the canonical cursor never regresses
#   network-partition  7      a severed control path ends the session and the
#                             Agent returns with no Join Token
#   compose-interrupt  9.6    cancel terminates the Compose process group and
#                             leaves no orphan child
#   concurrent-edit    10.6   a stale expected_sha256 is refused with 409 and the
#                             current content
#   disk-pressure      14.3   DEGRADED_STORAGE is reported through capability
#                             reason while allowed reads keep working
#   audit-gap          11.6   a WAL loss is surfaced as a coverage gap with its
#                             precision, not silently dropped
#   concurrent-operations 10.6 two writes racing on one project leave exactly one
#                             applied file, never a blend
#   docker-daemon-restart      the Engine returning brings the Agent back; opt-in
#                             only, because it stops every container on the host
#   db-restore         6.4    a Server database restored to an older state is
#                             not allowed to pull the ACK cursor backwards
#
# Select cases with HARDENING_CASES (default: all).

usage() {
    printf 'usage: %s ABSOLUTE_EVIDENCE_DIR SERVER_IMAGE_ID AGENT_IMAGE_ID FIXTURE_IMAGE_ID\n' "$0" >&2
    printf 'all image arguments must be exact local sha256 image IDs\n' >&2
}

fail() {
    printf 'hardening matrix failed: %s\n' "$*" >&2
    failure_reason=$*
    # fail is also reached from command substitutions, whose exit only leaves
    # the subshell. set -e still aborts the run, but the reason would be lost,
    # so it is recorded where cleanup can read it back.
    [ -z "${failure_reason_file:-}" ] || printf '%s\n' "$*" >"$failure_reason_file" 2>/dev/null || true
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
evidence_max_bytes=${HARDENING_EVIDENCE_MAX_BYTES:-16777216}
log_max_bytes=${HARDENING_LOG_MAX_BYTES:-1048576}
selected_cases=${HARDENING_CASES:-agent-sigkill operation-interrupt server-sigkill network-partition compose-interrupt concurrent-edit disk-pressure audit-gap db-restore concurrent-operations docker-daemon-restart}

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

case "$evidence_max_bytes" in ''|*[!0-9]*) fail "preflight: HARDENING_EVIDENCE_MAX_BYTES must be an integer" ;; esac
case "$log_max_bytes" in ''|*[!0-9]*) fail "preflight: HARDENING_LOG_MAX_BYTES must be an integer" ;; esac
[ "$evidence_max_bytes" -ge 4194304 ] && [ "$evidence_max_bytes" -le 67108864 ] ||
    fail "preflight: evidence cap must be between 4 MiB and 64 MiB"
[ "$log_max_bytes" -ge 65536 ] && [ "$log_max_bytes" -le 4194304 ] ||
    fail "preflight: log cap must be between 64 KiB and 4 MiB"

command -v docker >/dev/null 2>&1 || fail "preflight: docker is required"
for command_name in openssl curl jq awk date du df mktemp head wc find chmod stat cat mv rm rmdir mkdir sleep tr sha256sum; do
    command -v "$command_name" >/dev/null 2>&1 || fail "preflight: required command not found: $command_name"
done
[ -z "${DOCKER_HOST:-}" ] || fail "preflight: DOCKER_HOST is not supported; use the local default Engine socket"
docker info >/dev/null 2>&1 || fail "preflight: Docker daemon is unavailable or permission is denied"
[ "$(docker info --format '{{.OSType}}')" = linux ] || fail "preflight: a Linux Docker Engine is required"
[ -S /var/run/docker.sock ] && [ -r /var/run/docker.sock ] && [ -w /var/run/docker.sock ] ||
    fail "preflight: readable and writable /var/run/docker.sock is required"

for image in "$server_image" "$agent_image" "$fixture_image"; do
    docker image inspect "$image" >/dev/null 2>&1 || fail "preflight: exact local image is unavailable: $image"
    [ "$(docker image inspect --format '{{.Id}}' "$image")" = "$image" ] ||
        fail "preflight: image reference did not resolve to its exact requested ID: $image"
done
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$server_image")" = "DockLattice Server" ] ||
    fail "preflight: Server image is not the production Server target"
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$agent_image")" = "DockLattice Agent" ] ||
    fail "preflight: Agent image is not the production Agent target"
server_version=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$server_image")
agent_version=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$agent_image")
server_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$server_image")
agent_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$agent_image")
[ "$server_version" = "$agent_version" ] && [ -n "$server_version" ] && [ "$server_version" != dev ] && [ "$server_version" != '<no value>' ] ||
    fail "preflight: Server and Agent must carry the same non-dev release version"
[ "$server_revision" = "$agent_revision" ] || fail "preflight: Server and Agent revisions differ"
[ "${#server_revision}" -eq 40 ] || fail "preflight: production revision must be a full 40-character Git object ID"

available_kib=$(df -Pk "$evidence_parent" | awk 'NR == 2 { print $4 }')
[ "$available_kib" -ge $((evidence_max_bytes / 1024 + 65536)) ] ||
    fail "preflight: evidence filesystem needs the cap plus 64 MiB free"

runtime_base=${TMPDIR:-/tmp}
case "$runtime_base" in /*) ;; *) fail "preflight: TMPDIR must be absolute" ;; esac
[ -d "$runtime_base" ] || fail "preflight: TMPDIR does not exist"

prefix="docklattice-hardening-$(date -u +%Y%m%dT%H%M%SZ)-$$"
umask 077
artifact_created=0
runtime=
server="$prefix-server"
agent="$prefix-agent"
network="$prefix-network"
compose_project=$(printf '%s' "$prefix-fixture" | tr '[:upper:]' '[:lower:]')
secret_marker="hardening-secret-must-never-be-recorded-$$"
completed=0
failure_reason="harness did not complete"
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
    [ -n "${runtime:-}" ] && [ -d "$runtime" ] || return 0
    case "$runtime" in "$runtime_base"/docklattice-hardening.*) ;; *) return 1 ;; esac
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/hardening-runtime" "$server_image" \
        -c 'rm -rf /hardening-runtime/server /hardening-runtime/agent /hardening-runtime/bootstrap /hardening-runtime/projects /hardening-runtime/observed-operations' \
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
    docker rm -f "$agent" >/dev/null 2>&1 || true
    remove_compose_objects
    docker rm -f "$server" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
    runtime_cleaned=true
    scrub_runtime || runtime_cleaned=false
    if [ "$artifact_created" -eq 1 ]; then
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
trap 'failure_reason="harness interrupted by signal"; exit 130' HUP INT TERM

runtime=$(mktemp -d "$runtime_base/docklattice-hardening.XXXXXXXX")
case "$runtime" in "$runtime_base"/docklattice-hardening.*) ;; *) fail "mktemp returned an unexpected runtime root" ;; esac
chmod 0700 "$runtime"
mkdir "$evidence_dir"
chmod 0700 "$evidence_dir"
artifact_created=1
failure_reason_file="$evidence_dir/failure-reason.txt"
{
    printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'docker_server_version=%s\n' "$(docker info --format '{{.ServerVersion}}')"
    printf 'server_image_id=%s\n' "$server_image"
    printf 'agent_image_id=%s\n' "$agent_image"
    printf 'fixture_image_id=%s\n' "$fixture_image"
    printf 'release_version=%s\n' "$server_version"
    printf 'release_revision=%s\n' "$server_revision"
    printf 'selected_cases=%s\n' "$selected_cases"
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
  hardening-fixture:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 60; done"]
EOF
# A marked secret lives in the project so that every scenario's closing
# invariant can prove no failure path moved it into Audit, an Operation result,
# or a container log.
printf 'HARDENING_SECRET=%s\n' "$secret_marker" >"$runtime/projects/.env"
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
    -v "$runtime:/hardening" "$server_image" -c \
    'chown -R 65532:65532 /hardening/server /hardening/agent; chmod 0700 /hardening/server /hardening/agent /hardening/server/tls; chmod 0600 /hardening/server/tls/server.crt /hardening/server/tls/server.key; chown -R 65532:65532 /hardening/projects; chmod 0777 /hardening/projects; chmod 0666 /hardening/projects/compose.yaml /hardening/projects/.env' \
    >/dev/null

server_state_sh() {
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/server:/state" "$server_image" -c "$1"
}
agent_state_sh() {
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/agent:/state" "$server_image" -c "$1"
}
read_identity_field() {
    server_state_sh "cat /state/identity/server-identity.json" | jq -r ".$1"
}

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

start_server() {
    docker run --pull never -d --name "$server" --network "$network" --network-alias server \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        -p 127.0.0.1::8080 -v "$runtime/server:/var/lib/docklattice:rw" "$server_image" \
        server --listen 0.0.0.0:8080 --agent-listen 0.0.0.0:8443 --allow-public-bind
}

resolve_base_url() {
    server_port=$(docker port "$server" 8080/tcp | awk -F: 'NR == 1 { print $NF }')
    case "$server_port" in ''|*[!0-9]*) fail "could not resolve the Server HTTPS port" ;; esac
    base_url="https://127.0.0.1:$server_port"
}

wait_server_ready() {
    deadline=$(( $(date +%s) + 90 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        curl --fail --silent --show-error --max-time 3 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >/dev/null 2>&1 && return 0
        sleep 1
    done
    fail "Server did not become HTTPS-ready"
}

# fixture_uids is the set of project identities this harness created. Nothing
# else may ever be the target of a request. allow_fixture_uid is the only way
# into that set, and every entry is a UID this harness derived itself.
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

# guard_project_target sits in the one place every request passes through, so
# a call site cannot forget it and a new one cannot be added without it. It
# refuses both a project-scoped URL and an operation body that names a project
# this harness does not own.
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
        curl --fail --silent --show-error --max-time 15 --cacert "$runtime/bootstrap/server-ca.crt" "$url" >"$output.tmp"
    else
        curl --fail --silent --show-error --max-time 15 --cacert "$runtime/bootstrap/server-ca.crt" \
            -H 'Content-Type: application/json' -X "$method" --data "$body" "$url" >"$output.tmp"
    fi
    [ "$(wc -c <"$output.tmp" | awk '{ print $1 }')" -le 1048576 ] || fail "API response exceeded 1 MiB"
    mv "$output.tmp" "$output"
}

# api_status writes the numeric HTTP status to stdout and the body to a file.
api_status() {
    method=$1; url=$2; body=$3; output=$4
    guard_project_target "$url" "$body"
    if [ "$method" = GET ]; then
        curl --silent --show-error --max-time 15 --output "$output" --write-out '%{http_code}' \
            --cacert "$runtime/bootstrap/server-ca.crt" "$url"
    else
        curl --silent --show-error --max-time 15 --output "$output" --write-out '%{http_code}' \
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

issue_token() {
    docker run --pull never --rm --user 65532:65532 \
        -v "$runtime/server:/var/lib/docklattice:rw" "$server_image" \
        server issue-token --state-dir /var/lib/docklattice --ttl 15m \
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
        token_args="--join-token-file /var/lib/docklattice/join-token"
    else
        token_args=
    fi
    # shellcheck disable=SC2086
    docker run --pull never -d --name "$agent" --network "$network" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.docklattice.role=agent \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/agent:/var/lib/docklattice:rw" \
        -v "$runtime/projects:$runtime/projects:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/docklattice/server-ca.crt $token_args \
        --display-name hardening-agent --self-container-name "$agent" \
        --project-root "$runtime/projects"
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

selected() {
    for name in $selected_cases; do
        [ "$name" = "$1" ] && return 0
    done
    return 1
}

record() {
    printf '%s=%s\n' "$1" "$2" >>"$evidence_dir/assertions.env"
}

# ------------------------------------------------------- fixture identity
# A dashboard on a working host also lists the operator's own Compose
# projects. Choosing a target by list position would point this matrix's
# writes, backups, and Compose runs at somebody else's files, so every
# target is resolved from the identity this harness created and nothing else.
# The Agent derives a project UID as sha256(agent_id || NUL || canonical
# working directory), so the harness can compute the UID it expects instead of
# trusting the answer it is checking.
expected_fixture_uid() {
    printf '%s\000%s' "$1" "$2" | sha256sum | awk '{ print $1 }'
}

# find_fixture_project prints the UID of the one project carrying the fixture
# name at the fixture root given in $2, or nothing when the dashboard does not
# list it. It never falls back to a list position: more than one match, or a
# UID that does not match the derivation above, is a failure rather than a
# guess. Only genuine absence yields an empty answer.
find_fixture_project() {
    dashboard=$1
    root=$2
    # Some cases create a second root under a name of their own; both are still
    # identities this harness made.
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

# select_fixture_project is the same lookup where absence is also a failure.
select_fixture_project() {
    resolved=$(find_fixture_project "$1" "$2" "${3:-$compose_project}")
    [ -n "$resolved" ] ||
        fail "fixture: no dashboard project is named ${3:-$compose_project} at $2"
    printf '%s' "$resolved"
}


# ------------------------------------------------------------------ baseline
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
agent_state_sh 'rm -f /state/join-token' >/dev/null
identity_baseline=$(read_identity_field server_identity_id)
generation_baseline=$(read_identity_field archive_generation)
record baseline_agent_id "$agent_id"
record baseline_project_uid "$project_uid"
record fixture_root "$fixture_root"
record fixture_identity_verified PASS
record other_projects_on_host "$(jq -r --arg uid "$project_uid" '[.projects[]? | select(.uid != $uid)] | length' "$evidence_dir/dashboard.baseline.json")"

# The database snapshot for the restore case is taken here, while the Server is
# stopped, so that later cases can advance the canonical Audit past it and the
# restore is genuinely backwards rather than a no-op.
#
# It is taken only once Audit coverage is established and acknowledged, and that
# is the difference between this case testing one thing or another. A snapshot
# taken before the Agent's first Audit sync restores to a database with no
# coverage row at all, and the Server then establishes coverage fresh at
# wherever the Agent resumes - a real path, but the easy one. A snapshot taken
# after coverage exists restores an archive that believes it acknowledged far
# less than it had, while the Agent resumes from the acknowledgement this Server
# issued and has now forgotten. That is the case 6.4 is about, and waiting for
# the precondition is what makes it happen every run instead of some runs.
if selected db-restore; then
    # Coverage is established by the Agent's first Audit sync, and that needs
    # something to have happened. When this case runs inside the full matrix the
    # earlier cases supply it; run on its own it would wait for events nobody is
    # producing. These containers are created by this harness, carry its own
    # label, and remove themselves.
    activity=1
    while [ "$activity" -le 6 ]; do
        docker run --pull never --rm --name "$prefix-audit-activity-$activity" \
            --label io.docklattice.role=hardening-fixture --entrypoint /bin/sh \
            "$fixture_image" -c 'exit 0' >/dev/null 2>&1 || true
        activity=$((activity + 1))
    done
    snapshot_ready=0
    deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        # The request is made inline rather than through audit_page, which is
        # defined further down the file and does not exist yet at this point.
        if curl --fail --silent --show-error --max-time 10 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/hosts/$agent_id/audit?limit=1" \
            >"$evidence_dir/db-restore.snapshot-precondition.json" 2>/dev/null &&
            jq -e '.coverage.established == true and .coverage.ack != null and (.coverage.ack.seq // 0) > 0' \
                "$evidence_dir/db-restore.snapshot-precondition.json" >/dev/null 2>&1; then
            snapshot_ready=1
            break
        fi
        sleep 2
    done
    [ "$snapshot_ready" -eq 1 ] ||
        fail "db-restore: Audit coverage was never established, so a snapshot would not exercise a backwards restore"
    record db_restore_snapshot_ack "$(jq -r '"\(.coverage.ack.incarnation),\(.coverage.ack.seq)"' \
        "$evidence_dir/db-restore.snapshot-precondition.json")"
    docker stop "$server" >/dev/null
    # The helper runs as root, so the copy has to be handed back to the Server's UID.
    server_state_sh 'cp /state/server.db /state/server.db.snapshot && chown 65532:65532 /state/server.db.snapshot && chmod 0600 /state/server.db.snapshot' >/dev/null
    docker start "$server" >/dev/null
    resolve_base_url
    wait_server_ready
    wait_active_host "$agent_id" "$evidence_dir/dashboard.after-snapshot.json" 300 ||
        fail "baseline: the Agent did not return after the snapshot restart"
fi

# ------------------------------------------------------------- invariants
# Every case closes with the same question: is the product's state still
# describable? A case that leaves an orphan project lock, a live restore
# journal, a stray staging file, a nonterminal Operation, an undescribed Audit
# range, a stale session, or a leaked secret has not passed merely because its
# own assertion held. 13 of the ADR's post-failure invariants are observable
# from outside the Agent, and those are the ones asserted here.
observed_operations="$runtime/observed-operations"
: >"$observed_operations"

track_operation() { printf '%s\n' "$1" >>"$observed_operations"; }

agent_state_dir() {
    docker exec "$agent" /bin/sh -c '
        for candidate in /var/lib/docklattice /constrained/state; do
            if [ -e "$candidate/identity/agent-state.json" ] || [ -e "$candidate/agent-state.json" ]; then
                printf %s "$candidate"
                exit 0
            fi
        done
        exit 1'
}

check_invariants() {
    scenario=$1
    prefix="$evidence_dir/invariants.$scenario"

    # 1. Exactly one ACTIVE Agent: no retired or duplicated session survived.
    wait_active_host "$agent_id" "$prefix.dashboard.json" 240 ||
        fail "$scenario: invariant: the fleet did not settle on exactly one ACTIVE Agent"

    # 2. Every Operation this run created is terminal, and terminal exactly once.
    #    An Operation the Server can no longer describe must be refused by name
    #    rather than answered with a guess, and only a deliberate history
    #    rollback may produce that refusal.
    while read -r tracked; do
        [ -n "$tracked" ] || continue
        operation_status=$(api_status GET "$base_url/api/v1/agents/$agent_id/operations/$tracked" '' "$prefix.operation.json")
        case "$operation_status" in
            200)
                jq -e '.status == "success" or .status == "failed" or .status == "canceled" or
                       .status == "interrupted" or .status == "rejected"' "$prefix.operation.json" >/dev/null ||
                    fail "$scenario: invariant: operation $tracked is still nonterminal"
                jq -e '.revision > 0' "$prefix.operation.json" >/dev/null ||
                    fail "$scenario: invariant: operation $tracked has no durable revision"
                ;;
            *)
                fail "$scenario: invariant: operation $tracked answered HTTP $operation_status"
                ;;
        esac
    done <"$observed_operations"

    # 3. The project lock is free. A lock-taking Operation that is refused for
    #    any other reason still proves the lock was acquired; only PROJECT_BUSY
    #    proves an orphan.
    probe="hardening-lock-probe-$scenario-$$"
    api POST "$base_url/api/v1/projects/$project_uid/backups" \
        "$(jq -cn --arg id "$probe" '{operation_id:$id,relative_paths:["compose.yaml"]}')" \
        "$prefix.probe.accepted.json"
    track_operation "$probe"
    poll_operation "$probe" "$prefix.probe.json" 180 ||
        fail "$scenario: invariant: the project lock probe never reached a terminal state"
    jq -e '((.error // "") | test("PROJECT_BUSY")) | not' "$prefix.probe.json" >/dev/null ||
        fail "$scenario: invariant: the project lock is still held by nothing"

    # 4. No restore journal and no staging orphan survives a closed scenario.
    state_dir=$(agent_state_dir) ||
        fail "$scenario: invariant: could not locate the Agent state directory"
    docker exec "$agent" /bin/sh -c "ls -A '$state_dir/restore-journal' 2>/dev/null" \
        >"$prefix.restore-journal.txt" 2>&1 || true
    [ ! -s "$prefix.restore-journal.txt" ] ||
        fail "$scenario: invariant: a restore journal survived a settled scenario"
    ls -A "$runtime/projects" | grep -e '^\.docklattice-' >"$prefix.staging.txt" 2>/dev/null || true
    [ ! -s "$prefix.staging.txt" ] ||
        fail "$scenario: invariant: staging files were orphaned in the project directory"

    # 5/6. Audit coverage is describable: every gap names its source, precision,
    #      and an ordered range, and the acknowledged cursor never exceeds what
    #      the Server says it has.
    audit_page "$prefix.audit.json"
    jq -e '
      (.coverage.gaps // []) | all(
        (.precision == "exact" or .precision == "coalesced" or .precision == "unknown") and
        (.source == "AGENT_GAP" or .source == "SERVER_RETENTION" or
         .source == "AGENT_CONTINUITY_UNCERTAIN" or .source == "SERVER_CURSOR_REGRESSION") and
        (.from.seq <= .until.seq))' "$prefix.audit.json" >/dev/null ||
        fail "$scenario: invariant: an Audit coverage entry is missing its source, precision, or ordering"
    jq -e '
      (.coverage.ack == null) or (.coverage.delivery_next == null) or
      (.coverage.ack.incarnation < .coverage.delivery_next.incarnation) or
      (.coverage.ack.incarnation == .coverage.delivery_next.incarnation and
       .coverage.ack.seq < .coverage.delivery_next.seq)' "$prefix.audit.json" >/dev/null ||
        fail "$scenario: invariant: the acknowledged cursor passed the Server delivery cursor"

    # 14. Docker's own answer and DockLattice's view agree about this run's containers.
    docker ps --all --filter "label=io.docklattice.role=agent" --format '{{.ID}}' >"$prefix.docker-agent.txt"
    api GET "$base_url/api/v1/hosts/$agent_id/containers" '' "$prefix.containers.json"
    while read -r container; do
        [ -n "$container" ] || continue
        jq -e --arg id "$container" 'any(.[]; .id | startswith($id))' "$prefix.containers.json" >/dev/null ||
            fail "$scenario: invariant: container $container exists in Docker but not in the DockLattice view"
    done <"$prefix.docker-agent.txt"

    # 15. The Compose file DockLattice reads is the file on disk.
    api GET "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" '' "$prefix.file.json"
    on_disk=$(sha256sum "$runtime/projects/compose.yaml" | awk '{ print $1 }')
    reported=$(jq -r '.sha256' "$prefix.file.json")
    [ "$on_disk" = "$reported" ] ||
        fail "$scenario: invariant: DockLattice reported compose.yaml as $reported, disk says $on_disk"

    # 16. The project secret never reached Audit, an Operation result, or a log.
    if grep -q -e "$secret_marker" "$prefix.audit.json" "$prefix.probe.json" "$prefix.file.json"; then
        fail "$scenario: invariant: the project secret leaked into an API answer"
    fi
    docker logs "$server" >"$prefix.server.log" 2>&1 || true
    docker logs "$agent" >"$prefix.agent.log" 2>&1 || true
    if grep -q -e "$secret_marker" "$prefix.server.log" "$prefix.agent.log"; then
        fail "$scenario: invariant: the project secret leaked into a container log"
    fi
    rm -f "$prefix.server.log" "$prefix.agent.log" "$prefix.operation.json"
    record "invariants_$(printf '%s' "$scenario" | tr - _)" PASS
}

read_incarnation() {
    agent_state_sh 'cat /state/identity/agent-state.json 2>/dev/null || cat /state/agent-state.json 2>/dev/null || true' |
        jq -r '.current_incarnation // empty' 2>/dev/null || true
}

audit_page() {
    api GET "$base_url/api/v1/hosts/$agent_id/audit?limit=200" '' "$1"
}

# ------------------------------------------ case: restart without the Join Token
# A Join Token is a bootstrap secret: single-use, and reasonable to delete once
# it has been consumed. The baseline deleted this run's token above, which is
# what makes this case possible at all.
#
# A container's argument list does not change between restarts, so the Agent
# still carries --join-token-file pointing at a file that is deliberately gone.
# `docker start` re-runs exactly that argument list, which is the operational
# shape of a `restart: unless-stopped` policy acting after a reboot or a daemon
# restart. A registered Agent has to come back from it: it holds a runtime
# credential, and a consumed bootstrap secret is not a dependency of running.
#
# This case is not in the default selection, and has to be asked for by name:
#
#     HARDENING_CASES=join-token-restart ./scripts/run-hardening-matrix-e2e.sh ...
#
# It has to run before any case that replaces the Agent container, because the
# flag still being on the container's argument list is the whole point. But an
# extra Agent restart at the head of the full sequence changes what `db-restore`
# is measuring: the database snapshot that case restores is taken once, right
# after the baseline, and one more incarnation between the snapshot and the
# restore leaves the restored Server with a range it cannot explain. It then
# refuses the session with AUDIT_ACK_INELIGIBLE, repeatedly, and the Agent never
# returns.
#
# Whether that is a defect is an open question - see the campaign record - and
# it is not this case's question. Running this case in its own invocation keeps
# both gates measuring what they were written to measure, and keeps the
# interaction visible instead of averaged away.
if selected join-token-restart; then
    agent_state_sh '[ -e /state/join-token ] && echo present || echo absent' \
        >"$evidence_dir/join-token-restart.token-state"
    grep -q absent "$evidence_dir/join-token-restart.token-state" ||
        fail "join-token-restart: the consumed Join Token is still present, so this case would prove nothing"
    docker inspect --format '{{range .Args}}{{println .}}{{end}}' "$agent" \
        >"$evidence_dir/join-token-restart.args.txt"
    grep -q -- '--join-token-file' "$evidence_dir/join-token-restart.args.txt" ||
        fail "join-token-restart: the Agent container does not carry --join-token-file, so a restart would not exercise the defect"

    docker stop "$agent" >/dev/null
    docker start "$agent" >"$evidence_dir/join-token-restart.restart"
    if ! wait_active_host "$agent_id" "$evidence_dir/join-token-restart.dashboard.json" 240; then
        capture_log "$agent" "$evidence_dir/join-token-restart.agent.log"
        fail "join-token-restart: a registered Agent did not return ACTIVE after restarting with a consumed Join Token file"
    fi
    # Same host, one host: an Agent that re-enrolled would appear as a second.
    jq -e --arg id "$agent_id" '(.hosts | length) == 1 and .hosts[0].id == $id' \
        "$evidence_dir/join-token-restart.dashboard.json" >/dev/null ||
        fail "join-token-restart: the restarted Agent did not keep its identity"
    jq -e --arg uid "$project_uid" '[.projects[]? | select(.uid == $uid)] | length == 1' \
        "$evidence_dir/join-token-restart.dashboard.json" >/dev/null ||
        fail "join-token-restart: the fixture project did not survive the restart under its original uid"
    agent_state_sh '[ -e /state/join-token ] && echo present || echo absent' \
        >"$evidence_dir/join-token-restart.token-state.after"
    grep -q absent "$evidence_dir/join-token-restart.token-state.after" ||
        fail "join-token-restart: the Agent recreated a Join Token file"
    record join_token_restart_without_bootstrap_secret PASS
    record join_token_restart_identity_preserved PASS
    check_invariants join-token-restart
fi

# ------------------------------------------------- case: agent SIGKILL
# 11.5 requires every unclean shutdown to advance the incarnation and to leave
# AUDIT_CONTINUITY_UNCERTAIN, because a window always exists between observing
# an event and writing it to the WAL.
if selected agent-sigkill; then
    incarnation_before=$(read_incarnation)
    [ -n "$incarnation_before" ] || fail "agent-sigkill: could not read the Agent incarnation before the kill"
    docker kill --signal KILL "$agent" >/dev/null
    deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        state=$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$agent")
        case "$state" in "exited "*) break ;; esac
        sleep 1
    done
    printf '%s\n' "$state" >"$evidence_dir/agent-sigkill.state"
    case "$state" in
        "exited 137") ;;
        *) fail "agent-sigkill: SIGKILL did not leave the Agent exited with 137: $state" ;;
    esac
    agent_state_sh 'cat /state/identity/agent-state.json' >"$evidence_dir/agent-sigkill.state.json"
    jq -e '.clean_close == null or (.clean_close.incarnation != .current_incarnation)' \
        "$evidence_dir/agent-sigkill.state.json" >/dev/null ||
        fail "agent-sigkill: a killed Agent recorded a clean close for its live incarnation"
    docker rm -f "$agent" >/dev/null
    start_agent false >"$evidence_dir/agent-sigkill.restart.container-id"
    wait_active_host "$agent_id" "$evidence_dir/agent-sigkill.dashboard.json" 240 ||
        fail "agent-sigkill: the Agent did not return ACTIVE with its original identity"
    incarnation_after=$(read_incarnation)
    [ -n "$incarnation_after" ] || fail "agent-sigkill: could not read the Agent incarnation after restart"
    [ "$incarnation_after" -gt "$incarnation_before" ] ||
        fail "agent-sigkill: incarnation did not advance ($incarnation_before -> $incarnation_after)"
    found=0
    deadline=$(( $(date +%s) + 120 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        audit_page "$evidence_dir/agent-sigkill.audit.json"
        if jq -e --argjson previous "$incarnation_before" '
              [.events[] | select(.kind == "AUDIT_CONTINUITY_UNCERTAIN" and .previous_incarnation == $previous)] | length >= 1
            ' "$evidence_dir/agent-sigkill.audit.json" >/dev/null 2>&1; then
            found=1
            break
        fi
        sleep 3
    done
    [ "$found" -eq 1 ] ||
        fail "agent-sigkill: no AUDIT_CONTINUITY_UNCERTAIN was recorded for the killed incarnation"
    record agent_sigkill_incarnation_before "$incarnation_before"
    record agent_sigkill_incarnation_after "$incarnation_after"
    record agent_sigkill_continuity_uncertain PASS
    check_invariants agent-sigkill
fi

# ------------------------------------------- case: operation interrupted by a kill
# 11.5 and the operation journal together promise that an operation which was
# still running when the Agent died comes back terminal and marked as possibly
# having had partial effects, rather than staying nonterminal forever or being
# silently reported as clean.
if selected operation-interrupt; then
    cp "$runtime/projects/compose.yaml" "$runtime/bootstrap/compose.yaml.original"
    # A health-gated dependency is what makes compose.up slow enough to be
    # interrupted deterministically instead of racing a one-second start.
    cat >"$runtime/projects/compose.yaml" <<EOF
name: $compose_project
services:
  hardening-gate:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "sleep 40; touch /tmp/ready; trap 'exit 0' TERM INT; while :; do sleep 60; done"]
    healthcheck:
      test: ["CMD-SHELL", "[ -f /tmp/ready ]"]
      interval: 2s
      timeout: 2s
      retries: 60
  hardening-fixture:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 60; done"]
    depends_on:
      hardening-gate:
        condition: service_healthy
EOF
    slow_operation="hardening-slow-up-$$"
    track_operation "$slow_operation"
    api POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$slow_operation" --arg agent "$agent_id" --arg project "$project_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
        "$evidence_dir/operation-interrupt.accepted.json"
    running=0
    deadline=$(( $(date +%s) + 60 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/agents/$agent_id/operations/$slow_operation" \
            >"$evidence_dir/operation-interrupt.running.json" 2>/dev/null &&
            jq -e '.status == "running"' "$evidence_dir/operation-interrupt.running.json" >/dev/null 2>&1; then
            running=1
            break
        fi
        sleep 1
    done
    [ "$running" -eq 1 ] ||
        fail "operation-interrupt: the health-gated compose.up never reported running, so nothing was interrupted"
    docker kill --signal KILL "$agent" >/dev/null
    docker rm -f "$agent" >/dev/null
    cp "$runtime/bootstrap/compose.yaml.original" "$runtime/projects/compose.yaml"
    start_agent false >"$evidence_dir/operation-interrupt.restart.container-id"
    wait_active_host "$agent_id" "$evidence_dir/operation-interrupt.dashboard.json" 240 ||
        fail "operation-interrupt: the Agent did not return after being killed mid-operation"
    poll_operation "$slow_operation" "$evidence_dir/operation-interrupt.final.json" 180 ||
        fail "operation-interrupt: the interrupted operation never reported a terminal state"
    jq -e '.status == "interrupted"' "$evidence_dir/operation-interrupt.final.json" >/dev/null ||
        fail "operation-interrupt: an operation killed while running did not come back interrupted"
    jq -e '.partial_effects_possible == true' "$evidence_dir/operation-interrupt.final.json" >/dev/null ||
        fail "operation-interrupt: an interrupted operation did not admit possible partial effects"
    remove_compose_objects
    record operation_interrupt_terminal_after_kill PASS
    record operation_interrupt_partial_effects_admitted PASS
    check_invariants operation-interrupt
fi

# ------------------------------------------------- case: server SIGKILL
# A killed Server keeps its Identity State, so 6.4 classifies the reconnect as a
# normal one: same identity, same generation, same archive_id.
if selected server-sigkill; then
    audit_page "$evidence_dir/server-sigkill.audit.before.json"
    canonical_before=$(jq -r '[.events[].cursor.seq] | max // 0' "$evidence_dir/server-sigkill.audit.before.json")
    docker kill --signal KILL "$server" >/dev/null
    deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        state=$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$server")
        case "$state" in "exited "*) break ;; esac
        sleep 1
    done
    printf '%s\n' "$state" >"$evidence_dir/server-sigkill.state"
    case "$state" in
        "exited 137") ;;
        *) fail "server-sigkill: SIGKILL did not leave the Server exited with 137: $state" ;;
    esac
    docker start "$server" >"$evidence_dir/server-sigkill.restart"
    resolve_base_url
    wait_server_ready
    [ "$(read_identity_field server_identity_id)" = "$identity_baseline" ] ||
        fail "server-sigkill: server_identity_id changed after an unclean Server exit"
    [ "$(read_identity_field archive_generation)" = "$generation_baseline" ] ||
        fail "server-sigkill: archive_generation advanced although the Identity State survived"
    wait_active_host "$agent_id" "$evidence_dir/server-sigkill.dashboard.json" 300 ||
        fail "server-sigkill: the Agent did not reconnect after the Server was killed"
    audit_page "$evidence_dir/server-sigkill.audit.after.json"
    canonical_after=$(jq -r '[.events[].cursor.seq] | max // 0' "$evidence_dir/server-sigkill.audit.after.json")
    [ "$canonical_after" -ge "$canonical_before" ] ||
        fail "server-sigkill: the canonical cursor regressed ($canonical_before -> $canonical_after)"
    record server_sigkill_identity_preserved PASS
    record server_sigkill_cursor_monotonic PASS
    check_invariants server-sigkill
fi

# ------------------------------------------------- case: network partition
# The Agent dials the Server, so a severed path leaves its side of the session
# readable with nothing to read. The session must still end and reconnect.
if selected network-partition; then
    docker network disconnect "$network" "$agent" >/dev/null
    printf 'disconnected_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$evidence_dir/network-partition.env"
    offline=0
    deadline=$(( $(date +%s) + 240 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >"$evidence_dir/network-partition.offline.json" 2>/dev/null &&
            jq -e '[.hosts[] | select(.state == "ACTIVE")] | length == 0' \
                "$evidence_dir/network-partition.offline.json" >/dev/null 2>&1; then
            offline=1
            break
        fi
        sleep 3
    done
    [ "$offline" -eq 1 ] ||
        fail "network-partition: the Server never stopped reporting the partitioned Agent as ACTIVE"
    printf 'observed_offline_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$evidence_dir/network-partition.env"
    docker network connect "$network" "$agent" >/dev/null
    wait_active_host "$agent_id" "$evidence_dir/network-partition.recovered.json" 300 ||
        fail "network-partition: the Agent did not return ACTIVE after the path was restored"
    agent_state_sh '[ -e /state/join-token ] && echo present || echo absent' >"$evidence_dir/network-partition.token"
    grep -q absent "$evidence_dir/network-partition.token" ||
        fail "network-partition: recovery consumed a Join Token"
    record network_partition_session_ended PASS
    record network_partition_reconnect_without_token PASS
    check_invariants network-partition
fi

# ------------------------------------------------- case: compose interrupt
# 9.6: cancel signals the Compose process group, escalating to SIGKILL after the
# grace period. Compose has no single commit point to protect, so termination is
# allowed - but it must not leave an orphaned child behind.
if selected compose-interrupt; then
    compose_up="hardening-compose-up-$$"
    track_operation "$compose_up"
    api POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$compose_up" --arg agent "$agent_id" --arg project "$project_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
        "$evidence_dir/compose-interrupt.up.accepted.json"
    poll_operation "$compose_up" "$evidence_dir/compose-interrupt.up.final.json" 180 ||
        fail "compose-interrupt: the preparatory compose.up never reached a terminal state"
    jq -e '.status == "success"' "$evidence_dir/compose-interrupt.up.final.json" >/dev/null ||
        fail "compose-interrupt: the preparatory compose.up did not succeed"

    compose_down="hardening-compose-down-$$"
    track_operation "$compose_down"
    api POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$compose_down" --arg agent "$agent_id" --arg project "$project_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.down"}')" \
        "$evidence_dir/compose-interrupt.down.accepted.json"
    cancel_status=$(api_status POST "$base_url/api/v1/agents/$agent_id/operations/$compose_down/cancel" '{}' \
        "$evidence_dir/compose-interrupt.cancel.json")
    printf 'cancel_http_status=%s\n' "$cancel_status" >"$evidence_dir/compose-interrupt.env"
    case "$cancel_status" in
        200|404|409) ;;
        *) fail "compose-interrupt: cancel answered an unexpected HTTP status $cancel_status" ;;
    esac
    poll_operation "$compose_down" "$evidence_dir/compose-interrupt.down.final.json" 180 ||
        fail "compose-interrupt: the cancelled compose.down never reached a terminal state"
    jq -e '.status == "success" or .status == "canceled" or .status == "failed"' \
        "$evidence_dir/compose-interrupt.down.final.json" >/dev/null ||
        fail "compose-interrupt: the cancelled operation reached an unexpected terminal state"
    # A cancelled Compose run must leave no docker process of its own behind
    # inside the Agent, and repeating the cancel must stay idempotent.
    for attempt in 1 2 3; do
        repeat_status=$(api_status POST "$base_url/api/v1/agents/$agent_id/operations/$compose_down/cancel" '{}' \
            "$evidence_dir/compose-interrupt.cancel.repeat.$attempt.json")
        printf 'repeat_%s_status=%s\n' "$attempt" "$repeat_status" >>"$evidence_dir/compose-interrupt.env"
        case "$repeat_status" in
            200|404|409) ;;
            *) fail "compose-interrupt: repeated cancel answered HTTP $repeat_status" ;;
        esac
    done
    docker exec "$agent" /bin/sh -c 'ps -o pid,args 2>/dev/null || true' >"$evidence_dir/compose-interrupt.agent-ps.txt" 2>&1 || true
    if grep -E 'docker(-compose)? .*(up|down)' "$evidence_dir/compose-interrupt.agent-ps.txt" >/dev/null 2>&1; then
        fail "compose-interrupt: a Compose child process survived the cancelled operation"
    fi
    record compose_interrupt_terminal PASS
    record compose_interrupt_no_orphan_child PASS
    record compose_interrupt_repeated_cancel_stable PASS
    check_invariants compose-interrupt
fi

# ------------------------------------------------- case: concurrent edit
# 10.6 is CORE: a write carrying a stale expected_sha256 is refused with 409 and
# the current content, so a long-open editor cannot silently overwrite a change
# made outside DockLattice.
if selected concurrent-edit; then
    api GET "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" '' \
        "$evidence_dir/concurrent-edit.read.json"
    stale_sha=$(jq -r '.sha256' "$evidence_dir/concurrent-edit.read.json")
    [ -n "$stale_sha" ] && [ "$stale_sha" != null ] || fail "concurrent-edit: file read did not return a sha256"
    # An out-of-band edit, exactly the SSH case the design names.
    printf '\n# external edit %s\n' "$(date -u +%s)" >>"$runtime/projects/compose.yaml"
    external_sha=$(sha256sum "$runtime/projects/compose.yaml" | awk '{ print $1 }')
    [ "$external_sha" != "$stale_sha" ] || fail "concurrent-edit: the external edit did not change the file digest"
    # The file lives on the Agent, so only the Agent can compare digests: the
    # write is dispatched as an operation and the conflict is the operation's
    # terminal outcome rather than a synchronous status on the dispatch.
    conflict_operation="hardening-concurrent-edit-$$"
    track_operation "$conflict_operation"
    conflict_status=$(api_status PUT "$base_url/api/v1/projects/$project_uid/files" \
        "$(jq -cn --arg id "$conflict_operation" --arg path compose.yaml --arg sha "$stale_sha" \
            --arg content 'name: overwritten' \
            '{operation_id:$id,relative_path:$path,expected_sha256:$sha,content:$content}')" \
        "$evidence_dir/concurrent-edit.accepted.json")
    printf 'dispatch_http_status=%s\n' "$conflict_status" >"$evidence_dir/concurrent-edit.env"
    [ "$conflict_status" = 202 ] ||
        fail "concurrent-edit: the write dispatch answered HTTP $conflict_status instead of 202"
    poll_operation "$conflict_operation" "$evidence_dir/concurrent-edit.conflict.json" 120 ||
        fail "concurrent-edit: the conflicting write never reached a terminal state"
    jq -e '.status != "success"' "$evidence_dir/concurrent-edit.conflict.json" >/dev/null ||
        fail "concurrent-edit: a write with a stale expected_sha256 succeeded"
    jq -e '.partial_effects_possible == false' "$evidence_dir/concurrent-edit.conflict.json" >/dev/null ||
        fail "concurrent-edit: a refused write reported possible partial effects"
    # 10.6 exists so the UI can show a diff, which requires the outcome to name
    # the conflict and to carry the digest the file actually has now.
    jq -e --arg current "$external_sha" '(.error // "") | test("conflict"; "i") and test($current)' \
        "$evidence_dir/concurrent-edit.conflict.json" >/dev/null ||
        fail "concurrent-edit: the refusal does not identify a conflict and the current digest"
    on_disk=$(sha256sum "$runtime/projects/compose.yaml" | awk '{ print $1 }')
    [ "$on_disk" = "$external_sha" ] ||
        fail "concurrent-edit: the refused write still modified the file"
    api GET "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" '' \
        "$evidence_dir/concurrent-edit.reread.json"
    current_sha=$(jq -r '.sha256' "$evidence_dir/concurrent-edit.reread.json")
    [ "$current_sha" = "$external_sha" ] ||
        fail "concurrent-edit: the Agent did not report the externally edited digest"
    record concurrent_edit_dispatch_status "$conflict_status"
    record concurrent_edit_conflict_identified PASS
    record concurrent_edit_file_unmodified PASS
    check_invariants concurrent-edit
fi

# ------------------------------------------------- case: Server DB restore
# 6.4: within one identity, generation, and archive_id, a database restored to
# an older state is exactly the SERVER_CURSOR_REGRESSION condition, and 6.5
# names the single protocol block - an ACK cursor below the previous watermark.
if selected db-restore; then
    audit_page "$evidence_dir/db-restore.audit.before.json"
    canonical_before=$(jq -r '[.events[].cursor.seq] | max // 0' "$evidence_dir/db-restore.audit.before.json")
    jq -r '.coverage' "$evidence_dir/db-restore.audit.before.json" >"$evidence_dir/db-restore.coverage.before.json"
    ack_before=$(jq -r '.coverage.ack.seq // 0' "$evidence_dir/db-restore.audit.before.json")
    docker stop "$server" >/dev/null
    server_state_sh 'cp /state/server.db.snapshot /state/server.db && chown 65532:65532 /state/server.db && chmod 0600 /state/server.db && rm -f /state/server.db-wal /state/server.db-shm' >/dev/null
    docker start "$server" >"$evidence_dir/db-restore.restart"
    resolve_base_url
    wait_server_ready
    [ "$(read_identity_field server_identity_id)" = "$identity_baseline" ] ||
        fail "db-restore: server_identity_id changed although only the database was restored"
    [ "$(read_identity_field archive_generation)" = "$generation_baseline" ] ||
        fail "db-restore: archive_generation advanced although the Identity State was untouched"
    if ! wait_active_host "$agent_id" "$evidence_dir/db-restore.dashboard.json" 300; then
        # A reconnect that never happens is decided by cursor state, and the
        # dashboard shows only that the host is OFFLINE. Capture what the two
        # sides believe about coverage before the run is torn down, or the
        # failure is unactionable.
        audit_page "$evidence_dir/db-restore.audit.stuck.json" 2>/dev/null || true
        jq -r '[.coverage.gaps[]? | select(.source == "SERVER_CURSOR_REGRESSION")]' \
            "$evidence_dir/db-restore.audit.stuck.json" >"$evidence_dir/db-restore.regression.json" 2>/dev/null || true
        agent_state_sh 'cat /state/identity/agent-state.json 2>/dev/null || true' \
            >"$evidence_dir/db-restore.agent-state.stuck.json" 2>/dev/null || true
        agent_state_sh 'ls -l /state/audit-wal 2>/dev/null || true' \
            >"$evidence_dir/db-restore.agent-wal.stuck.txt" 2>/dev/null || true
        capture_log "$agent" "$evidence_dir/db-restore.agent.stuck.log"
        capture_log "$server" "$evidence_dir/db-restore.server.stuck.log"
        fail "db-restore: the Agent did not reconnect to the restored Server"
    fi
    settled=0
    deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        audit_page "$evidence_dir/db-restore.audit.after.json"
        ack_after=$(jq -r '.coverage.ack.seq // 0' "$evidence_dir/db-restore.audit.after.json")
        [ "$ack_after" -ge "$ack_before" ] && { settled=1; break; }
        sleep 5
    done
    canonical_after=$(jq -r '[.events[].cursor.seq] | max // 0' "$evidence_dir/db-restore.audit.after.json")
    jq -r '.coverage' "$evidence_dir/db-restore.audit.after.json" >"$evidence_dir/db-restore.coverage.after.json"
    {
        printf 'canonical_before=%s\n' "$canonical_before"
        printf 'canonical_after=%s\n' "$canonical_after"
        printf 'ack_before=%s\n' "$ack_before"
        printf 'ack_after=%s\n' "${ack_after:-0}"
        printf 'ack_recovered=%s\n' "$settled"
    } >"$evidence_dir/db-restore.env"
    # The restored Server must not pull the acknowledged watermark backwards and
    # leave it there: either it refuses the regression, or it catches up from the
    # Agent's retained WAL. A permanently lower ACK is unacknowledged data loss.
    [ "$settled" -eq 1 ] ||
        fail "db-restore: the acknowledged cursor stayed below its pre-restore watermark ($ack_before -> ${ack_after:-0})"
    jq -e '.coverage.established == true' "$evidence_dir/db-restore.audit.after.json" >/dev/null ||
        fail "db-restore: coverage was not re-established after the restore"
    # Whether the restore stranded a range depends on how far the Agent's own
    # acknowledgement had moved past the snapshot, which this harness does not
    # control. Record which path was taken so the evidence says what was
    # exercised rather than leaving it to be inferred from a pass.
    regression_ranges=$(jq -r '
        [.coverage.gaps[]? | select(.source == "SERVER_CURSOR_REGRESSION")] | length
      ' "$evidence_dir/db-restore.audit.after.json" 2>/dev/null || echo 0)
    case "$regression_ranges" in ''|*[!0-9]*) regression_ranges=0 ;; esac
    record db_restore_server_regression_ranges "$regression_ranges"
    restored_start=$(jq -r '.coverage.start.cursor | "\(.incarnation),\(.seq)"' \
        "$evidence_dir/db-restore.audit.after.json" 2>/dev/null || echo unknown)
    record db_restore_coverage_start_after "$restored_start"
    # The snapshot carried an established coverage row, so the restore had to
    # strand a range and the Server had to account for it. Recovering by
    # establishing coverage afresh would mean the precondition above did not
    # hold and this case is measuring the easy path again.
    [ "$regression_ranges" -ge 1 ] ||
        fail "db-restore: the restore stranded no range; the snapshot precondition did not hold and the recovery path was not exercised"
    record db_restore_identity_preserved PASS
    record db_restore_ack_watermark_not_regressed PASS
    # The Server's own record of everything requested after the snapshot is
    # gone. The Agent still holds those results, so the Server must refuse to
    # describe them by name instead of inventing a status - and the tracked set
    # restarts here, because a discarded history cannot be re-proved.
    prior_operation=$(head -n 1 "$observed_operations")
    if [ -n "$prior_operation" ]; then
        prior_status=$(api_status GET "$base_url/api/v1/agents/$agent_id/operations/$prior_operation" '' \
            "$evidence_dir/db-restore.prior-operation.json")
        case "$prior_status" in
            200)
                jq -e '.status == "success" or .status == "failed" or .status == "canceled" or
                       .status == "interrupted" or .status == "rejected"' \
                    "$evidence_dir/db-restore.prior-operation.json" >/dev/null ||
                    fail "db-restore: a surviving Operation came back nonterminal"
                ;;
            404|409)
                jq -e '.code == "CONFLICT" or .code == "NOT_FOUND"' \
                    "$evidence_dir/db-restore.prior-operation.json" >/dev/null ||
                    fail "db-restore: a discarded Operation was refused without a typed reason"
                ;;
            *)
                fail "db-restore: an Operation from before the snapshot answered HTTP $prior_status"
                ;;
        esac
        printf 'prior_operation_status=%s\n' "$prior_status" >>"$evidence_dir/db-restore.env"
    fi
    : >"$observed_operations"
    record db_restore_prior_operation_explained PASS
    check_invariants db-restore
fi

# ------------------------------------------- case: concurrent project mutation
# Two writes to the same project must not both apply. The project lock decides
# one winner; the loser must be refused or serialized, never interleaved into a
# half-applied file.
if selected concurrent-operations; then
    api GET "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" '' \
        "$evidence_dir/concurrent-operations.read.json"
    shared_sha=$(jq -r '.sha256' "$evidence_dir/concurrent-operations.read.json")
    [ -n "$shared_sha" ] && [ "$shared_sha" != null ] || fail "concurrent-operations: file read did not return a sha256"
    original_content=$(jq -r '.content' "$evidence_dir/concurrent-operations.read.json")
    first="hardening-race-a-$$"
    second="hardening-race-b-$$"
    # Both requests carry the same expected digest, so exactly one can be
    # correct once either has landed.
    for pair in "$first:A" "$second:B"; do
        operation_id=${pair%%:*}
        track_operation "$operation_id"
        marker_value=${pair##*:}
        api_status PUT "$base_url/api/v1/projects/$project_uid/files" \
            "$(jq -cn --arg id "$operation_id" --arg path compose.yaml --arg sha "$shared_sha" \
                --arg content "$original_content
# race marker $marker_value" \
                '{operation_id:$id,relative_path:$path,expected_sha256:$sha,content:$content}')" \
            "$evidence_dir/concurrent-operations.$marker_value.accepted.json" \
            >"$evidence_dir/concurrent-operations.$marker_value.status" &
    done
    wait
    winners=0
    for marker_value in A B; do
        operation_id=$first
        [ "$marker_value" = B ] && operation_id=$second
        poll_operation "$operation_id" "$evidence_dir/concurrent-operations.$marker_value.final.json" 180 ||
            fail "concurrent-operations: request $marker_value never reached a terminal state"
        if jq -e '.status == "success"' "$evidence_dir/concurrent-operations.$marker_value.final.json" >/dev/null; then
            winners=$((winners + 1))
        fi
    done
    printf 'successful_writes=%s\n' "$winners" >"$evidence_dir/concurrent-operations.env"
    [ "$winners" -eq 1 ] ||
        fail "concurrent-operations: $winners of 2 racing writes succeeded; exactly one must win"
    # The surviving file must be exactly one of the two intended results, never a
    # blend of both.
    final_content=$(cat "$runtime/projects/compose.yaml")
    markers=$(printf '%s\n' "$final_content" | grep -c '^# race marker' || true)
    printf 'race_markers_in_file=%s\n' "$markers" >>"$evidence_dir/concurrent-operations.env"
    [ "$markers" -eq 1 ] ||
        fail "concurrent-operations: the file carries $markers race markers; a serialized write leaves exactly one"
    record concurrent_operations_single_winner PASS
    record concurrent_operations_file_not_blended PASS
    check_invariants concurrent-operations
fi

# ------------------------------------------- case: Docker daemon restart
# Restarting the host Docker Engine stops every container on the machine, so
# this case runs only on explicit opt-in and records why it was skipped
# otherwise. It is never silently dropped.
if selected docker-daemon-restart; then
    if [ "${HARDENING_ALLOW_DOCKER_DAEMON_RESTART:-0}" != 1 ]; then
        record docker_daemon_restart SKIPPED_NOT_AUTHORIZED
        printf 'reason=set HARDENING_ALLOW_DOCKER_DAEMON_RESTART=1 to allow restarting the host Engine\n' \
            >"$evidence_dir/docker-daemon-restart.env"
    elif ! sudo -n systemctl is-active docker >/dev/null 2>&1; then
        record docker_daemon_restart SKIPPED_NO_NONINTERACTIVE_RESTART
        printf 'reason=non-interactive systemctl restart of the docker unit is unavailable\n' \
            >"$evidence_dir/docker-daemon-restart.env"
    else
        sudo -n systemctl restart docker
        deadline=$(( $(date +%s) + 120 ))
        while [ "$(date +%s)" -lt "$deadline" ]; do
            docker info >/dev/null 2>&1 && break
            sleep 2
        done
        docker info >/dev/null 2>&1 || fail "docker-daemon-restart: the Engine did not come back"
        docker start "$server" >/dev/null
        resolve_base_url
        wait_server_ready
        # The baseline deleted the consumed Join Token to prove it is
        # single-use, so the Agent cannot be restarted with the original
        # argument list: it still carries --join-token-file. A registered
        # Agent does not need one, which is how every other restart case in
        # this matrix brings it back.
        docker rm -f "$agent" >/dev/null
        start_agent false >"$evidence_dir/docker-daemon-restart.agent.container-id"
        wait_active_host "$agent_id" "$evidence_dir/docker-daemon-restart.dashboard.json" 300 ||
            fail "docker-daemon-restart: the Agent did not return ACTIVE after the Engine restarted"
        jq -e '.hosts[0].capabilities.docker.enabled == true' \
            "$evidence_dir/docker-daemon-restart.dashboard.json" >/dev/null ||
            fail "docker-daemon-restart: the Docker capability did not recover"
        record docker_daemon_restart PASS
        check_invariants docker-daemon-restart
    fi
fi

# ---------------------------- cases: disk pressure and the AUDIT_GAP it causes
# 14.3 enters DEGRADED_STORAGE on an OR of its conditions and reports it through
# the heartbeat capability reason without disabling allowed capabilities. 11.6
# makes an actual WAL loss out-of-band coverage state with reason DISK_PRESSURE,
# never a silent drop. Both are driven by the same constrained filesystem, so
# they share one setup and run last: the constrained Agent keeps its state on a
# tmpfs that does not survive the case.
if selected disk-pressure || selected audit-gap; then
    docker stop "$agent" >/dev/null
    mkdir "$runtime/bootstrap/agent-seed"
    docker cp "$agent:/var/lib/docklattice/." "$runtime/bootstrap/agent-seed" >/dev/null 2>&1 ||
        fail "disk-pressure: could not copy the Agent state out for seeding"
    # docker cp writes as the invoking user, so the seed has to be handed to the
    # Agent's UID before the constrained container can read it.
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/bootstrap/agent-seed:/seed" "$server_image" -c \
        'chown -R 65532:65532 /seed && chmod -R u+rwX,go-rwx /seed' >/dev/null
    docker rm -f "$agent" >/dev/null
    # 8 MiB leaves the WAL far below the 1 GiB free-space entry floor and forces
    # real eviction rather than a synthetic flag.
    docker run --pull never -d --name "$agent" --network "$network" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.docklattice.role=agent \
        --mount type=tmpfs,destination=/constrained,tmpfs-size=8m,tmpfs-mode=0777 \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/bootstrap/agent-seed:/seed:ro" \
        -v "$runtime/projects:$runtime/projects:rw" \
        --entrypoint /bin/sh "$agent_image" -c \
        "mkdir -m 0700 /constrained/state && cp -a /seed/. /constrained/state/ && \
         exec /usr/local/bin/docklattice agent --state-dir /constrained/state \
           --server server:8443 --registration-url https://server:8080 \
           --server-ca /constrained/state/server-ca.crt \
           --display-name hardening-agent --self-container-name '$agent' \
           --project-root '$runtime/projects'" \
        >"$evidence_dir/disk-pressure.container-id"
    wait_active_host "$agent_id" "$evidence_dir/disk-pressure.dashboard.json" 300 ||
        fail "disk-pressure: the Agent on a constrained filesystem did not reach ACTIVE"
    docker exec "$agent" /bin/sh -c 'df -Pk /constrained/state' >"$evidence_dir/disk-pressure.df.txt" 2>&1 || true
    degraded=0
    deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        api GET "$base_url/api/v1/dashboard" '' "$evidence_dir/disk-pressure.capability.json"
        if jq -e '[.hosts[0].capabilities | to_entries[] | select(.value.reason != null and (.value.reason | test("DEGRADED_STORAGE|FILESYSTEM_FREE_LOW|AGENT_STATE_BUDGET_EXCEEDED")))] | length >= 1' \
            "$evidence_dir/disk-pressure.capability.json" >/dev/null 2>&1; then
            degraded=1
            break
        fi
        sleep 5
    done
    [ "$degraded" -eq 1 ] ||
        fail "disk-pressure: no capability reported a degraded-storage reason on a filesystem far below the entry floor"
    # 14.3 keeps allowed capabilities enabled; the warning must not disable them.
    jq -e '.hosts[0].capabilities.connection.enabled == true and .hosts[0].capabilities.docker.enabled == true' \
        "$evidence_dir/disk-pressure.capability.json" >/dev/null ||
        fail "disk-pressure: degraded storage disabled a capability instead of annotating it"
    # An allowed read must keep working while degraded.
    read_status=$(api_status GET "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" '' \
        "$evidence_dir/disk-pressure.read.json")
    [ "$read_status" = 200 ] ||
        fail "disk-pressure: an allowed read answered HTTP $read_status while storage was degraded"
    record disk_pressure_reason_reported PASS
    record disk_pressure_capabilities_preserved PASS
    record disk_pressure_allowed_read_works PASS

    if selected audit-gap; then
        api GET "$base_url/api/v1/hosts/$agent_id/audit?limit=200" '' "$evidence_dir/audit-gap.audit.json"
        jq -r '.coverage' "$evidence_dir/audit-gap.audit.json" >"$evidence_dir/audit-gap.coverage.json"
        jq -e '.coverage.established == true' "$evidence_dir/audit-gap.audit.json" >/dev/null ||
            fail "audit-gap: coverage was not established for the constrained Agent"
        # Whether or not the WAL had to evict in this run, every gap the Server
        # reports must carry its precision and a design-named reason, and must
        # never appear as a silent hole in the record.
        jq -e '
          (.coverage.gaps // []) | all(
            (.precision == "exact" or .precision == "coalesced") and
            (.source == "AGENT_GAP" or .source == "SERVER_RETENTION" or
             .source == "AGENT_CONTINUITY_UNCERTAIN" or .source == "SERVER_CURSOR_REGRESSION") and
            (.from.seq <= .until.seq)
          )' "$evidence_dir/audit-gap.audit.json" >/dev/null ||
            fail "audit-gap: a reported coverage gap is missing its precision, source, or ordering"
        gap_count=$(jq -r '(.coverage.gaps // []) | length' "$evidence_dir/audit-gap.audit.json")
        effective=$(jq -r '.coverage.effective_gap_records // 0' "$evidence_dir/audit-gap.audit.json")
        printf 'gap_count=%s\neffective_gap_records=%s\n' "$gap_count" "$effective" >"$evidence_dir/audit-gap.env"
        record audit_gap_reported_gaps "$gap_count"
        record audit_gap_records_accounted "$effective"
        record audit_gap_every_gap_is_described PASS
    fi
    check_invariants degraded-storage
fi

{
    printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >>"$evidence_dir/assertions.env"

used_kib=$(du -sk "$evidence_dir" | awk '{ print $1 }')
[ $((used_kib * 1024)) -le "$evidence_max_bytes" ] || fail "evidence size cap exceeded"
( cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -exec sha256sum {} + >SHA256SUMS )
completed=1
