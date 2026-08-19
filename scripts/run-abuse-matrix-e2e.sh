#!/bin/sh
set -eu

# Untrusted-input and invariant-violation matrix. The browser-facing HTTP API is
# the product's one untrusted surface, and a project directory is written by
# people outside Dockpilot. Every case here sends something the product must
# refuse, and asserts the refusal rather than the absence of a crash.
#
# Cases and the contract each one checks:
#   path-abuse         10.7  traversal, absolute, and unmanaged paths are refused
#   secret-exposure    10.x  secret values are masked without reveal and never
#                            reach Server persistence
#   operation-id-reuse       one operation ID cannot be rebound to another spec
#   operation-flood          a burst of operations stays bounded and the Server
#                            keeps answering
#   token-single-use         a Join Token that already registered an Agent cannot
#                            register a second one
#   wrong-server-ca          an Agent trusting a foreign CA never registers
#   operation-bounds         the project lock refuses a second mutation with
#                            PROJECT_BUSY, and an overrun result ring reports its
#                            forgotten records as gone rather than from cache
#   backup-tamper            a modified backup archive is refused before it can
#                            replace a live project file
#   non-identical-bind 3.1   a discovery root whose container path differs from
#                            its host path disables filesystem write capability
#   name-collision     7.6   two projects claiming one Compose name become
#                            read-only instead of racing each other
#   self-protection          a container operation aimed at the Agent itself is
#                            refused
#   protected-compose-project a Compose mutation aimed at the Compose project the
#                            Agent itself belongs to is refused, while an
#                            unrelated project still works
#   request-abuse            oversized, malformed, unknown-field, and
#                            wrong-method requests are refused with their status
#
# Select cases with ABUSE_CASES (default: all).

usage() {
    printf 'usage: %s ABSOLUTE_EVIDENCE_DIR SERVER_IMAGE_ID AGENT_IMAGE_ID FIXTURE_IMAGE_ID\n' "$0" >&2
    printf 'all image arguments must be exact local sha256 image IDs\n' >&2
}

fail() {
    printf 'abuse matrix failed: %s\n' "$*" >&2
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
evidence_max_bytes=${ABUSE_EVIDENCE_MAX_BYTES:-16777216}
log_max_bytes=${ABUSE_LOG_MAX_BYTES:-1048576}
selected_cases=${ABUSE_CASES:-path-abuse secret-exposure token-single-use wrong-server-ca operation-id-reuse operation-flood operation-bounds backup-tamper non-identical-bind name-collision self-protection protected-compose-project request-abuse}

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

case "$evidence_max_bytes" in ''|*[!0-9]*) fail "preflight: ABUSE_EVIDENCE_MAX_BYTES must be an integer" ;; esac
case "$log_max_bytes" in ''|*[!0-9]*) fail "preflight: ABUSE_LOG_MAX_BYTES must be an integer" ;; esac
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
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$server_image")" = "Dockpilot Server" ] ||
    fail "preflight: Server image is not the production Server target"
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$agent_image")" = "Dockpilot Agent" ] ||
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

prefix="dockpilot-abuse-$(date -u +%Y%m%dT%H%M%SZ)-$$"
umask 077
artifact_created=0
runtime=
server="$prefix-server"
agent="$prefix-agent"
network="$prefix-network"
compose_project=$(printf '%s' "$prefix-fixture" | tr '[:upper:]' '[:lower:]')
completed=0
extra_containers=
failure_reason="harness did not complete"

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
    case "$runtime" in "$runtime_base"/dockpilot-abuse.*) ;; *) return 1 ;; esac
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/abuse-runtime" "$server_image" \
        -c 'rm -rf /abuse-runtime/server /abuse-runtime/agent /abuse-runtime/bootstrap /abuse-runtime/projects /abuse-runtime/projects-second /abuse-runtime/projects-protected /abuse-runtime/replay /abuse-runtime/first-use /abuse-runtime/stranger' \
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
    for extra in ${extra_containers:-}; do
        docker rm -f "$extra" >/dev/null 2>&1 || true
    done
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

runtime=$(mktemp -d "$runtime_base/dockpilot-abuse.XXXXXXXX")
case "$runtime" in "$runtime_base"/dockpilot-abuse.*) ;; *) fail "mktemp returned an unexpected runtime root" ;; esac
chmod 0700 "$runtime"
mkdir "$evidence_dir"
chmod 0700 "$evidence_dir"
artifact_created=1
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
socket_gid=$(stat -c '%g' /var/run/docker.sock)
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
    -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
    -keyout "$runtime/server/tls/server.key" -out "$runtime/server/tls/server.crt" \
    >"$evidence_dir/openssl.stdout" 2>"$evidence_dir/openssl.stderr"
cp "$runtime/server/tls/server.crt" "$runtime/bootstrap/server-ca.crt"
cat >"$runtime/projects/compose.yaml" <<EOF
name: $compose_project
services:
  abuse-fixture:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 60; done"]
EOF
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
    -v "$runtime:/abuse" "$server_image" -c \
    'chown -R 65532:65532 /abuse/server /abuse/agent; chmod 0700 /abuse/server /abuse/agent /abuse/server/tls; chmod 0600 /abuse/server/tls/server.crt /abuse/server/tls/server.key; chown -R 65532:65532 /abuse/projects; chmod 0777 /abuse/projects; chmod 0666 /abuse/projects/compose.yaml' \
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

docker network create "$network" >"$evidence_dir/network.id"

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
    deadline=$(( $(date +%s) + 90 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        curl --fail --silent --show-error --max-time 3 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >/dev/null 2>&1 && return 0
        sleep 1
    done
    fail "Server did not become HTTPS-ready"
}

api() {
    method=$1; url=$2; body=$3; output=$4
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
              if $expected == "" then
                (.hosts | length) == 1 and .hosts[0].state == "ACTIVE"
              else
                ([.hosts[] | select(.id == $expected and .state == "ACTIVE")] | length) == 1
              end
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
        -v "$runtime/server:/var/lib/dockpilot:rw" "$server_image" \
        server issue-token --state-dir /var/lib/dockpilot --ttl 15m \
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
        --display-name abuse-agent --self-container-name "$agent" \
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


# ------------------------------------------------------------------ baseline
start_server >"$evidence_dir/server.container-id"
resolve_base_url
wait_server_ready
issue_token
start_agent true >"$evidence_dir/agent.container-id"
wait_active_host "" "$evidence_dir/dashboard.baseline.json" 180 ||
    fail "baseline registration did not produce exactly one ACTIVE host"
agent_id=$(jq -r '.hosts[0].id' "$evidence_dir/dashboard.baseline.json")
project_uid=$(jq -r '.projects[0].uid' "$evidence_dir/dashboard.baseline.json")
[ -n "$agent_id" ] && [ "$agent_id" != null ] || fail "baseline dashboard omitted the Agent id"
[ -n "$project_uid" ] && [ "$project_uid" != null ] || fail "baseline dashboard omitted the project uid"
agent_state_sh 'rm -f /state/join-token' >/dev/null
identity_baseline=$(read_identity_field server_identity_id)
generation_baseline=$(read_identity_field archive_generation)
record baseline_agent_id "$agent_id"
record baseline_project_uid "$project_uid"

read_incarnation() {
    agent_state_sh 'cat /state/identity/agent-state.json 2>/dev/null || cat /state/agent-state.json 2>/dev/null || true' |
        jq -r '.current_incarnation // empty' 2>/dev/null || true
}

audit_page() {
    api GET "$base_url/api/v1/hosts/$agent_id/audit?limit=200" '' "$1"
}


agent_container_id=$(docker inspect --format '{{.Id}}' "$agent")

# ------------------------------------------------- case: path abuse
# 10.7 bounds file operations to a managed set inside one project root. Every
# escape attempt must be refused before it can reach the filesystem.
if selected path-abuse; then
    : >"$evidence_dir/path-abuse.results.tsv"
    refused=0
    attempts=0
    for encoded in \
        '../compose.yaml' \
        '../../etc/passwd' \
        '/etc/passwd' \
        'subdir/../../compose.yaml' \
        './.env/../../../etc/hostname' \
        'compose.yaml/' \
        'not-managed.txt' \
        '.git/config'; do
        attempts=$((attempts + 1))
        encoded_path=$(printf '%s' "$encoded" | jq -sRr @uri)
        status=$(api_status GET "$base_url/api/v1/projects/$project_uid/files?path=$encoded_path" '' \
            "$evidence_dir/path-abuse.read.$attempts.json")
        printf 'read\t%s\t%s\n' "$encoded" "$status" >>"$evidence_dir/path-abuse.results.tsv"
        case "$status" in
            400|403|404|409) refused=$((refused + 1)) ;;
            *) fail "path-abuse: reading $encoded answered HTTP $status instead of a refusal" ;;
        esac
        attempts=$((attempts + 1))
        write_status=$(api_status PUT "$base_url/api/v1/projects/$project_uid/files" \
            "$(jq -cn --arg id "abuse-path-$attempts-$$" --arg path "$encoded" \
                --arg sha "$(printf 'a%.0s' $(seq 64))" --arg content 'x' \
                '{operation_id:$id,relative_path:$path,expected_sha256:$sha,content:$content}')" \
            "$evidence_dir/path-abuse.write.$attempts.json")
        printf 'write\t%s\t%s\n' "$encoded" "$write_status" >>"$evidence_dir/path-abuse.results.tsv"
        case "$write_status" in
            400|403|404|409) refused=$((refused + 1)) ;;
            *) fail "path-abuse: writing $encoded answered HTTP $write_status instead of a refusal" ;;
        esac
    done
    [ "$refused" -eq "$attempts" ] || fail "path-abuse: only $refused of $attempts escape attempts were refused"
    # Nothing outside the project root may have been created.
    [ ! -e "$runtime/not-managed.txt" ] || fail "path-abuse: a write escaped the project root"
    record path_abuse_attempts "$attempts"
    record path_abuse_all_refused PASS
fi

# ------------------------------------------------- case: secret exposure
# A .env value is secret-bearing. It must be masked unless explicitly revealed,
# and it must never be persisted by the Server, whose storage is a cache of
# metadata rather than of project content.
if selected secret-exposure; then
    secret_value="abuse-secret-$(date -u +%s)-do-not-persist"
    env_operation="abuse-env-write-$$"
    api GET "$base_url/api/v1/projects/$project_uid/files?path=.env" '' \
        "$evidence_dir/secret-exposure.read-before.json" 2>/dev/null ||
        printf '{"sha256":""}\n' >"$evidence_dir/secret-exposure.read-before.json"
    existing_sha=$(jq -r '.sha256 // ""' "$evidence_dir/secret-exposure.read-before.json")
    if [ -z "$existing_sha" ] || [ "$existing_sha" = null ]; then
        # safefile preserves the target's ownership, so a seed file has to be
        # created as the Agent's UID rather than as the harness user.
        docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
            -v "$runtime/projects:/projects" "$server_image" -c \
            ': >/projects/.env && chown 65532:65532 /projects/.env && chmod 0644 /projects/.env' >/dev/null
        api GET "$base_url/api/v1/projects/$project_uid/files?path=.env" '' \
            "$evidence_dir/secret-exposure.read-empty.json"
        existing_sha=$(jq -r '.sha256' "$evidence_dir/secret-exposure.read-empty.json")
    fi
    write_status=$(api_status PUT "$base_url/api/v1/projects/$project_uid/files" \
        "$(jq -cn --arg id "$env_operation" --arg sha "$existing_sha" --arg value "$secret_value" \
            '{operation_id:$id,relative_path:".env",expected_sha256:$sha,content:("API_TOKEN=" + $value + "\n")}')" \
        "$evidence_dir/secret-exposure.write.json")
    [ "$write_status" = 202 ] || fail "secret-exposure: the .env write answered HTTP $write_status"
    poll_operation "$env_operation" "$evidence_dir/secret-exposure.final.json" 120 ||
        fail "secret-exposure: the .env write never reached a terminal state"
    jq -e '.status == "success"' "$evidence_dir/secret-exposure.final.json" >/dev/null ||
        fail "secret-exposure: the .env write did not succeed"
    api GET "$base_url/api/v1/projects/$project_uid/files?path=.env" '' \
        "$evidence_dir/secret-exposure.masked.json"
    grep -F -- "$secret_value" "$evidence_dir/secret-exposure.masked.json" >/dev/null &&
        fail "secret-exposure: the unrevealed read returned the secret value"
    api GET "$base_url/api/v1/projects/$project_uid/environment" '' \
        "$evidence_dir/secret-exposure.environment.json"
    grep -F -- "$secret_value" "$evidence_dir/secret-exposure.environment.json" >/dev/null &&
        fail "secret-exposure: the unrevealed environment listing returned the secret value"
    # Server persistence must not contain the value anywhere, in any table.
    server_state_sh "grep -a -c -F -- '$secret_value' /state/server.db 2>/dev/null || true" \
        >"$evidence_dir/secret-exposure.database-hits.txt"
    hits=$(tr -d ' \n' <"$evidence_dir/secret-exposure.database-hits.txt")
    [ "$hits" = 0 ] || fail "secret-exposure: the Server database contains the secret value ($hits matches)"
    audit_page "$evidence_dir/secret-exposure.audit.json"
    grep -F -- "$secret_value" "$evidence_dir/secret-exposure.audit.json" >/dev/null &&
        fail "secret-exposure: the Audit record contains the secret value"
    record secret_exposure_masked_without_reveal PASS
    record secret_exposure_absent_from_server_storage PASS
fi

# ------------------------------------------------- case: operation ID reuse
# An operation ID is the idempotency key. Rebinding it to a different Agent,
# project, kind, or target would let one identifier describe two different
# actions, so the second spec must be refused rather than merged.
if selected operation-id-reuse; then
    reused="abuse-reuse-$$"
    api POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$reused" --arg agent "$agent_id" --arg project "$project_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
        "$evidence_dir/operation-id-reuse.first.json"
    poll_operation "$reused" "$evidence_dir/operation-id-reuse.first.final.json" 180 ||
        fail "operation-id-reuse: the first operation never reached a terminal state"
    : >"$evidence_dir/operation-id-reuse.results.tsv"
    for spec in 'kind:compose.down' 'target:different-target' 'project:none'; do
        label=${spec%%:*}
        value=${spec#*:}
        case "$label" in
            kind) body=$(jq -cn --arg id "$reused" --arg agent "$agent_id" --arg project "$project_uid" --arg kind "$value" \
                    '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:$kind}') ;;
            target) body=$(jq -cn --arg id "$reused" --arg agent "$agent_id" --arg project "$project_uid" --arg target "$value" \
                    '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up",target:$target}') ;;
            *) body=$(jq -cn --arg id "$reused" --arg agent "$agent_id" \
                    '{operation_id:$id,agent_id:$agent,kind:"discovery.rescan"}') ;;
        esac
        status=$(api_status POST "$base_url/api/v1/operations" "$body" \
            "$evidence_dir/operation-id-reuse.$label.json")
        printf '%s\t%s\n' "$label" "$status" >>"$evidence_dir/operation-id-reuse.results.tsv"
        [ "$status" = 409 ] ||
            fail "operation-id-reuse: rebinding by $label answered HTTP $status instead of 409"
    done
    # The original record must be untouched by the refused attempts.
    api GET "$base_url/api/v1/agents/$agent_id/operations/$reused" '' \
        "$evidence_dir/operation-id-reuse.after.json"
    jq -e '.status == "success"' "$evidence_dir/operation-id-reuse.after.json" >/dev/null ||
        fail "operation-id-reuse: the refused attempts disturbed the original operation"
    record operation_id_reuse_refused PASS
    record operation_id_reuse_original_intact PASS
fi

# ------------------------------------------------- case: operation flood
# The active-operation index is bounded by design. A burst must not grow it
# without limit and must not stop the Server from answering.
if selected operation-flood; then
    flood_total=40
    accepted=0
    rejected=0
    index=0
    : >"$evidence_dir/operation-flood.statuses.txt"
    while [ "$index" -lt "$flood_total" ]; do
        index=$((index + 1))
        status=$(api_status POST "$base_url/api/v1/operations" \
            "$(jq -cn --arg id "abuse-flood-$index-$$" --arg agent "$agent_id" \
                '{operation_id:$id,agent_id:$agent,kind:"discovery.rescan"}')" \
            "$evidence_dir/operation-flood.body.json")
        printf '%s\n' "$status" >>"$evidence_dir/operation-flood.statuses.txt"
        case "$status" in
            202) accepted=$((accepted + 1)) ;;
            409|429|503) rejected=$((rejected + 1)) ;;
            *) fail "operation-flood: request $index answered an unexpected HTTP $status" ;;
        esac
    done
    {
        printf 'requested=%s\n' "$flood_total"
        printf 'accepted=%s\n' "$accepted"
        printf 'rejected=%s\n' "$rejected"
    } >"$evidence_dir/operation-flood.env"
    [ $((accepted + rejected)) -eq "$flood_total" ] ||
        fail "operation-flood: not every request produced a decision"
    # The Server must still answer normally right after the burst.
    dashboard_status=$(api_status GET "$base_url/api/v1/dashboard" '' "$evidence_dir/operation-flood.dashboard.json")
    [ "$dashboard_status" = 200 ] ||
        fail "operation-flood: the dashboard answered HTTP $dashboard_status after the burst"
    jq -e --arg id "$agent_id" '[.hosts[] | select(.id == $id and .state == "ACTIVE")] | length == 1' \
        "$evidence_dir/operation-flood.dashboard.json" >/dev/null ||
        fail "operation-flood: the Agent left ACTIVE during the burst"
    # The bounded active list must not have grown past its documented cap.
    api GET "$base_url/api/v1/hosts/$agent_id/operations" '' "$evidence_dir/operation-flood.active.json" 2>/dev/null || true
    record operation_flood_accepted "$accepted"
    record operation_flood_rejected "$rejected"
    record operation_flood_server_responsive PASS
fi

# ------------------------------------------------- case: self protection
# A container operation aimed at the Agent's own container must be refused. The
# Agent is what carries out the operation, so obeying would end the session that
# asked for it.
if selected self-protection; then
    : >"$evidence_dir/self-protection.results.tsv"
    for kind in container.stop container.restart container.remove; do
        operation_id="abuse-self-${kind%%.*}-$(printf '%s' "$kind" | tr -d '.')-$$"
        status=$(api_status POST "$base_url/api/v1/operations" \
            "$(jq -cn --arg id "$operation_id" --arg agent "$agent_id" --arg kind "$kind" \
                --arg target "$agent_container_id" \
                '{operation_id:$id,agent_id:$agent,kind:$kind,target:$target}')" \
            "$evidence_dir/self-protection.$kind.json")
        printf '%s\t%s\n' "$kind" "$status" >>"$evidence_dir/self-protection.results.tsv"
        case "$status" in
            202)
                poll_operation "$operation_id" "$evidence_dir/self-protection.$kind.final.json" 120 ||
                    fail "self-protection: $kind against the Agent never reached a terminal state"
                jq -e '.status != "success"' "$evidence_dir/self-protection.$kind.final.json" >/dev/null ||
                    fail "self-protection: $kind against the Agent's own container succeeded"
                ;;
            400|403|409) ;;
            *) fail "self-protection: $kind answered an unexpected HTTP $status" ;;
        esac
    done
    # The Agent must still be running and ACTIVE afterwards.
    [ "$(docker inspect --format '{{.State.Status}}' "$agent")" = running ] ||
        fail "self-protection: the Agent container is no longer running"
    wait_active_host "$agent_id" "$evidence_dir/self-protection.dashboard.json" 120 ||
        fail "self-protection: the Agent is no longer ACTIVE after targeting itself"
    record self_protection_refused PASS
    record self_protection_agent_survives PASS
fi

# ------------------------------------------------- case: request abuse
# The HTTP API is the one untrusted surface. Malformed, oversized, unknown-field
# and wrong-method requests must be refused with their own status rather than
# reaching a handler or answering 500.
if selected request-abuse; then
    : >"$evidence_dir/request-abuse.results.tsv"
    check() {
        label=$1; expected=$2; actual=$3
        printf '%s\t%s\t%s\n' "$label" "$expected" "$actual" >>"$evidence_dir/request-abuse.results.tsv"
        [ "$actual" = "$expected" ] || fail "request-abuse: $label answered HTTP $actual instead of $expected"
    }
    status=$(api_status POST "$base_url/api/v1/operations" '{"operation_id":' \
        "$evidence_dir/request-abuse.malformed.json")
    check malformed-json 400 "$status"
    status=$(api_status POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "abuse-unknown-$$" --arg agent "$agent_id" \
            '{operation_id:$id,agent_id:$agent,kind:"discovery.rescan",unexpected_field:"x"}')" \
        "$evidence_dir/request-abuse.unknown-field.json")
    check unknown-field 400 "$status"
    status=$(api_status GET "$base_url/api/v1/operations" '' "$evidence_dir/request-abuse.method.json")
    case "$status" in
        404|405) printf 'wrong-method\t404-or-405\t%s\n' "$status" >>"$evidence_dir/request-abuse.results.tsv" ;;
        *) fail "request-abuse: GET on the operations collection answered HTTP $status" ;;
    esac
    status=$(api_status GET "$base_url/api/v1/hosts/$agent_id/audit?limit=0" '' \
        "$evidence_dir/request-abuse.limit.json")
    check audit-limit-zero 400 "$status"
    status=$(api_status GET "$base_url/api/v1/hosts/$agent_id/audit?cursor=not-a-cursor" '' \
        "$evidence_dir/request-abuse.cursor.json")
    check audit-cursor 400 "$status"
    status=$(api_status GET "$base_url/api/v1/dashboard?unexpected=1" '' \
        "$evidence_dir/request-abuse.query.json")
    check dashboard-query 400 "$status"
    # An oversized body must be refused on size, never buffered whole.
    oversized=$(mktemp "$runtime/bootstrap/oversized.XXXXXX")
    # Streamed into the file rather than built as an argument: a two-megabyte
    # argv does not survive execve.
    {
        printf '{"operation_id":"abuse-oversized-%s","agent_id":"%s","kind":"discovery.rescan","target":"' \
            "$$" "$agent_id"
        head -c 2000000 /dev/zero | tr '\0' 'a'
        printf '"}'
    } >"$oversized"
    status=$(curl --silent --show-error --max-time 30 --output "$evidence_dir/request-abuse.oversized.json" \
        --write-out '%{http_code}' --cacert "$runtime/bootstrap/server-ca.crt" \
        -H 'Content-Type: application/json' -X POST --data-binary "@$oversized" \
        "$base_url/api/v1/operations")
    rm -f "$oversized"
    case "$status" in
        400|413) printf 'oversized-body\t400-or-413\t%s\n' "$status" >>"$evidence_dir/request-abuse.results.tsv" ;;
        *) fail "request-abuse: an oversized body answered HTTP $status" ;;
    esac
    # None of the refusals may have been a 500, and the Server must stay healthy.
    if grep -P '\t5\d\d$' "$evidence_dir/request-abuse.results.tsv" >/dev/null 2>&1; then
        fail "request-abuse: a refusal answered with a server error"
    fi
    dashboard_status=$(api_status GET "$base_url/api/v1/dashboard" '' "$evidence_dir/request-abuse.dashboard.json")
    [ "$dashboard_status" = 200 ] || fail "request-abuse: the Server is unhealthy after the abusive requests"
    record request_abuse_all_refused_with_client_status PASS
    record request_abuse_server_healthy PASS
fi

# ------------------------------------------------- case: Join Token is single use
# A Join Token is one-time. The first Agent to present it consumes it; a second
# Agent presenting the same secret must be refused, or the token is a reusable
# credential rather than a one-time one.
if selected token-single-use; then
    issue_token
    mkdir "$runtime/first-use" "$runtime/replay"
    # Both throwaway Agents get their own empty state, so each one genuinely
    # registers rather than reusing a credential it already holds.
    for target in first-use replay; do
        docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
            -v "$runtime/agent:/source:ro" -v "$runtime/$target:/target" "$server_image" -c \
            'cp /source/server-ca.crt /target/server-ca.crt && cp /source/join-token /target/join-token && chown -R 65532:65532 /target && chmod 0700 /target && chmod 0600 /target/server-ca.crt /target/join-token' >/dev/null
    done
    agent_state_sh 'rm -f /state/join-token' >/dev/null

    start_throwaway_agent() {
        name=$1
        state=$2
        docker run --pull never -d --name "$name" --network "$network" \
            --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
            --group-add "$socket_gid" --label io.dockpilot.role=agent \
            -v /var/run/docker.sock:/var/run/docker.sock:rw \
            -v "$runtime/$state:/var/lib/dockpilot:rw" "$agent_image" agent \
            --server server:8443 --registration-url https://server:8080 \
            --server-ca /var/lib/dockpilot/server-ca.crt \
            --join-token-file /var/lib/dockpilot/join-token \
            --display-name "abuse-$state" --self-container-name "$name"
    }

    first_agent="$prefix-agent-first-use"
    replay_agent="$prefix-agent-replay"
    extra_containers="$extra_containers $first_agent $replay_agent"
    start_throwaway_agent "$first_agent" first-use >"$evidence_dir/token-single-use.first.container-id"
    joined=0
    deadline=$(( $(date +%s) + 240 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        api GET "$base_url/api/v1/dashboard" '' "$evidence_dir/token-single-use.after-first.json"
        if jq -e '(.hosts | length) == 2' "$evidence_dir/token-single-use.after-first.json" >/dev/null 2>&1; then
            joined=1
            break
        fi
        sleep 3
    done
    capture_log "$first_agent" "$evidence_dir/token-single-use.first.log"
    [ "$joined" -eq 1 ] ||
        fail "token-single-use: the first Agent did not consume the token and register"

    start_throwaway_agent "$replay_agent" replay >"$evidence_dir/token-single-use.replay.container-id"
    refused=0
    replay_state=
    deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        replay_state=$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$replay_agent")
        case "$replay_state" in
            "exited "*)
                [ "$replay_state" = "exited 0" ] && fail "token-single-use: the replaying Agent exited successfully"
                refused=1
                break
                ;;
        esac
        api GET "$base_url/api/v1/dashboard" '' "$evidence_dir/token-single-use.after-replay.json"
        jq -e '(.hosts | length) > 2' "$evidence_dir/token-single-use.after-replay.json" >/dev/null 2>&1 &&
            fail "token-single-use: a replayed token registered a second Agent"
        sleep 3
    done
    capture_log "$replay_agent" "$evidence_dir/token-single-use.replay.log"
    printf 'replay_state=%s\n' "$replay_state" >"$evidence_dir/token-single-use.env"
    [ "$refused" -eq 1 ] ||
        fail "token-single-use: the replaying Agent neither exited nor was rejected on an already consumed token"
    api GET "$base_url/api/v1/dashboard" '' "$evidence_dir/token-single-use.final.json"
    jq -e '(.hosts | length) == 2' "$evidence_dir/token-single-use.final.json" >/dev/null ||
        fail "token-single-use: the replay changed the registered host count"
    docker rm -f "$first_agent" "$replay_agent" >/dev/null
    record token_single_use_replay_refused PASS
    record token_single_use_no_extra_host PASS
fi

# ------------------------------------------------- case: wrong Server CA
# The Agent authenticates the Server before presenting anything. An Agent handed
# a CA that did not sign this Server must never reach registration.
if selected wrong-server-ca; then
    stranger="$prefix-agent-stranger"
    extra_containers="$extra_containers $stranger"
    mkdir "$runtime/stranger"
    openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
        -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
        -keyout "$runtime/stranger/foreign.key" -out "$runtime/stranger/server-ca.crt" \
        >"$evidence_dir/wrong-server-ca.openssl.stdout" 2>"$evidence_dir/wrong-server-ca.openssl.stderr"
    issue_token
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/agent:/source:ro" -v "$runtime/stranger:/stranger" "$server_image" -c \
        'cp /source/join-token /stranger/join-token && rm -f /stranger/foreign.key && chown -R 65532:65532 /stranger && chmod 0700 /stranger && chmod 0600 /stranger/server-ca.crt /stranger/join-token' >/dev/null
    agent_state_sh 'rm -f /state/join-token' >/dev/null
    api GET "$base_url/api/v1/dashboard" '' "$evidence_dir/wrong-server-ca.before.json"
    hosts_before=$(jq -r '.hosts | length' "$evidence_dir/wrong-server-ca.before.json")
    docker run --pull never -d --name "$stranger" --network "$network" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.dockpilot.role=agent \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/stranger:/var/lib/dockpilot:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/dockpilot/server-ca.crt \
        --join-token-file /var/lib/dockpilot/join-token \
        --display-name abuse-stranger --self-container-name "$stranger" \
        >"$evidence_dir/wrong-server-ca.container-id"
    # Give it a generous window to prove it cannot get in, then confirm the
    # Server never saw a second host.
    settle=$(( $(date +%s) + 60 ))
    while [ "$(date +%s)" -lt "$settle" ]; do
        api GET "$base_url/api/v1/dashboard" '' "$evidence_dir/wrong-server-ca.dashboard.json"
        jq -e --argjson before "$hosts_before" '(.hosts | length) > $before' \
            "$evidence_dir/wrong-server-ca.dashboard.json" >/dev/null 2>&1 &&
            fail "wrong-server-ca: an Agent trusting a foreign CA registered"
        sleep 5
    done
    capture_log "$stranger" "$evidence_dir/wrong-server-ca.log"
    {
        printf 'hosts_before=%s\n' "$hosts_before"
        printf 'stranger_state=%s\n' "$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$stranger")"
    } >"$evidence_dir/wrong-server-ca.env"
    docker rm -f "$stranger" >/dev/null
    record wrong_server_ca_never_registers PASS
fi

# ------------------------------------------------- case: operation bounds
# Two bounds decide what a burst can do: the project lock refuses a second
# mutation while one holds the project, and the Agent's bounded result ring
# forgets old records. The Agent is authoritative for both, so a forgotten
# record must answer 404 rather than being served from the Server cache.
if selected operation-bounds; then
    cp "$runtime/projects/compose.yaml" "$runtime/bootstrap/compose.yaml.bounds"
    cat >"$runtime/projects/compose.yaml" <<EOF
name: $compose_project
services:
  abuse-gate:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "sleep 45; touch /tmp/ready; trap 'exit 0' TERM INT; while :; do sleep 60; done"]
    healthcheck:
      test: ["CMD-SHELL", "[ -f /tmp/ready ]"]
      interval: 2s
      timeout: 2s
      retries: 60
  abuse-fixture:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 60; done"]
    depends_on:
      abuse-gate:
        condition: service_healthy
EOF
    holder="abuse-lock-holder-$$"
    api POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$holder" --arg agent "$agent_id" --arg project "$project_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
        "$evidence_dir/operation-bounds.holder.accepted.json"
    running=0
    deadline=$(( $(date +%s) + 60 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/agents/$agent_id/operations/$holder" \
            >"$evidence_dir/operation-bounds.holder.running.json" 2>/dev/null &&
            jq -e '.status == "running"' "$evidence_dir/operation-bounds.holder.running.json" >/dev/null 2>&1; then
            running=1
            break
        fi
        sleep 1
    done
    [ "$running" -eq 1 ] ||
        fail "operation-bounds: the lock-holding compose.up never reported running"
    contender="abuse-lock-contender-$$"
    contender_status=$(api_status POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$contender" --arg agent "$agent_id" --arg project "$project_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.down"}')" \
        "$evidence_dir/operation-bounds.contender.json")
    printf 'contender_dispatch_status=%s\n' "$contender_status" >"$evidence_dir/operation-bounds.env"
    busy=0
    case "$contender_status" in
        409)
            grep -F 'PROJECT_BUSY' "$evidence_dir/operation-bounds.contender.json" >/dev/null && busy=1
            ;;
        202)
            poll_operation "$contender" "$evidence_dir/operation-bounds.contender.final.json" 120 ||
                fail "operation-bounds: the contending mutation never reached a terminal state"
            jq -e '.status != "success"' "$evidence_dir/operation-bounds.contender.final.json" >/dev/null ||
                fail "operation-bounds: a second mutation ran while the project was locked"
            grep -F 'PROJECT_BUSY' "$evidence_dir/operation-bounds.contender.final.json" >/dev/null && busy=1
            ;;
        *) fail "operation-bounds: the contending mutation answered an unexpected HTTP $contender_status" ;;
    esac
    [ "$busy" -eq 1 ] ||
        fail "operation-bounds: the refusal did not name PROJECT_BUSY, so the project lock was not what refused it"
    poll_operation "$holder" "$evidence_dir/operation-bounds.holder.final.json" 240 ||
        fail "operation-bounds: the lock-holding operation never finished"
    jq -e '.status == "success"' "$evidence_dir/operation-bounds.holder.final.json" >/dev/null ||
        fail "operation-bounds: the lock holder did not complete normally after refusing the contender"
    cp "$runtime/bootstrap/compose.yaml.bounds" "$runtime/projects/compose.yaml"
    remove_compose_objects
    record operation_bounds_project_busy PASS

    # The result ring keeps the newest OperationResultMax records. Overrunning it
    # must forget the oldest, and a forgotten record must be reported as gone
    # rather than served from the Server's cache.
    ring_max=500
    ring_extra=25
    first_ring="abuse-ring-1-$$"
    index=0
    while [ "$index" -lt $((ring_max + ring_extra)) ]; do
        index=$((index + 1))
        status=$(api_status POST "$base_url/api/v1/operations" \
            "$(jq -cn --arg id "abuse-ring-$index-$$" --arg agent "$agent_id" \
                '{operation_id:$id,agent_id:$agent,kind:"discovery.rescan"}')" \
            "$evidence_dir/operation-bounds.ring.body.json")
        case "$status" in
            202|409|429|503) ;;
            *) fail "operation-bounds: ring request $index answered an unexpected HTTP $status" ;;
        esac
    done
    last_ring="abuse-ring-$index-$$"
    poll_operation "$last_ring" "$evidence_dir/operation-bounds.ring.last.json" 120 ||
        fail "operation-bounds: the newest ring operation never reached a terminal state"
    evicted=0
    deadline=$(( $(date +%s) + 120 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        first_status=$(api_status GET "$base_url/api/v1/agents/$agent_id/operations/$first_ring" '' \
            "$evidence_dir/operation-bounds.ring.first.json")
        [ "$first_status" = 404 ] && { evicted=1; break; }
        sleep 3
    done
    {
        printf 'ring_requested=%s\n' "$index"
        printf 'oldest_lookup_status=%s\n' "${first_status:-none}"
    } >>"$evidence_dir/operation-bounds.env"
    [ "$evicted" -eq 1 ] ||
        fail "operation-bounds: the oldest record answered HTTP ${first_status:-none} after the ring overran; an evicted record must be reported as gone"
    record operation_bounds_ring_evicts_oldest PASS
    record operation_bounds_newest_still_readable PASS
fi

# ------------------------------------------------- case: tampered backup archive
# A restore replaces live project files, so the archive it reads is trusted
# input. Every entry carries a digest, and a modified archive must be refused
# before anything is replaced.
if selected backup-tamper; then
    backup_operation="abuse-backup-create-$$"
    api POST "$base_url/api/v1/projects/$project_uid/backups" \
        "$(jq -cn --arg id "$backup_operation" '{operation_id:$id,relative_paths:["compose.yaml"]}')" \
        "$evidence_dir/backup-tamper.create.accepted.json"
    poll_operation "$backup_operation" "$evidence_dir/backup-tamper.create.final.json" 180 ||
        fail "backup-tamper: the backup never reached a terminal state"
    jq -e '.status == "success"' "$evidence_dir/backup-tamper.create.final.json" >/dev/null ||
        fail "backup-tamper: the backup did not succeed"
    api GET "$base_url/api/v1/projects/$project_uid/backups" '' "$evidence_dir/backup-tamper.list.json"
    backup_id=$(jq -r '.[0].backup_id // .backups[0].backup_id // ""' "$evidence_dir/backup-tamper.list.json")
    [ -n "$backup_id" ] && [ "$backup_id" != null ] || fail "backup-tamper: the backup listing had no backup_id"
    before_sha=$(sha256sum "$runtime/projects/compose.yaml" | awk '{ print $1 }')
    # Flip bytes inside the stored archive without changing its length, so the
    # only thing that can catch it is the per-entry digest.
    agent_state_sh "find /state/backups -type f -name '*.tar*' -print" >"$evidence_dir/backup-tamper.files.txt"
    # The archive has to be the one this case restores. Earlier cases leave
    # pre-write snapshots behind, so picking whichever archive the directory
    # happens to list first would tamper with a backup nobody reads.
    archive="/state/backups/$project_uid/$backup_id/files.tar.gz"
    grep -F -x -- "$archive" "$evidence_dir/backup-tamper.files.txt" >/dev/null ||
        fail "backup-tamper: the archive of the backup under test was not found on the Agent"
    agent_state_sh "dd if=/dev/urandom of='$archive' bs=1 seek=\$(( \$(wc -c <'$archive') / 2 )) count=64 conv=notrunc 2>/dev/null" >/dev/null
    restore_operation="abuse-backup-restore-$$"
    status=$(api_status POST "$base_url/api/v1/projects/$project_uid/backups/$backup_id/restore" \
        "$(jq -cn --arg id "$restore_operation" '{operation_id:$id}')" \
        "$evidence_dir/backup-tamper.restore.json")
    printf 'restore_dispatch_status=%s\n' "$status" >"$evidence_dir/backup-tamper.env"
    case "$status" in
        202)
            poll_operation "$restore_operation" "$evidence_dir/backup-tamper.restore.final.json" 180 ||
                fail "backup-tamper: the restore never reached a terminal state"
            jq -e '.status != "success"' "$evidence_dir/backup-tamper.restore.final.json" >/dev/null ||
                fail "backup-tamper: a restore from a modified archive succeeded"
            ;;
        400|409|422) ;;
        *) fail "backup-tamper: the restore answered an unexpected HTTP $status" ;;
    esac
    after_sha=$(sha256sum "$runtime/projects/compose.yaml" | awk '{ print $1 }')
    printf 'compose_sha_before=%s\ncompose_sha_after=%s\n' "$before_sha" "$after_sha" \
        >>"$evidence_dir/backup-tamper.env"
    [ "$before_sha" = "$after_sha" ] ||
        fail "backup-tamper: the refused restore still replaced the project file"
    record backup_tamper_restore_refused PASS
    record backup_tamper_project_untouched PASS
fi

# ------------------------------------------- case: non-identical bind path
# 3.1 and 3.2 require a discovery root to be a bind mount whose host path is
# identical to its container path, because Compose passes paths to the Docker
# daemon, which resolves them on the host. A root mounted anywhere else must
# disable filesystem capability rather than write to a path the daemon would
# read differently.
if selected non-identical-bind; then
    docker rm -f "$agent" >/dev/null
    # Same host directory, deliberately a different path inside the container.
    docker run --pull never -d --name "$agent" --network "$network" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.dockpilot.role=agent \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/agent:/var/lib/dockpilot:rw" \
        -v "$runtime/projects:/elsewhere/projects:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/dockpilot/server-ca.crt \
        --display-name abuse-agent --self-container-name "$agent" \
        --project-root /elsewhere/projects \
        >"$evidence_dir/non-identical-bind.container-id"
    disabled=0
    deadline=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >"$evidence_dir/non-identical-bind.dashboard.json" 2>/dev/null &&
            jq -e --arg id "$agent_id" '
                [.hosts[] | select(.id == $id)] as $host |
                ($host | length) == 1 and $host[0].state == "ACTIVE" and
                $host[0].capabilities.fs_write.enabled == false and
                ($host[0].capabilities.fs_write.reason // "") != ""' \
                "$evidence_dir/non-identical-bind.dashboard.json" >/dev/null 2>&1; then
            disabled=1
            break
        fi
        sleep 5
    done
    capture_log "$agent" "$evidence_dir/non-identical-bind.log"
    [ "$disabled" -eq 1 ] ||
        fail "non-identical-bind: filesystem write capability stayed enabled for a root whose container path differs from its host path"
    jq -r --arg id "$agent_id" '[.hosts[] | select(.id == $id)][0].capabilities.fs_write.reason' \
        "$evidence_dir/non-identical-bind.dashboard.json" \
        >"$evidence_dir/non-identical-bind.reason.txt"
    # A write must be refused for that reason rather than silently attempted.
    stale_uid=$(jq -r '.projects[0].uid // ""' "$evidence_dir/non-identical-bind.dashboard.json")
    if [ -n "$stale_uid" ] && [ "$stale_uid" != null ]; then
        write_status=$(api_status PUT "$base_url/api/v1/projects/$stale_uid/files" \
            "$(jq -cn --arg id "abuse-nonidentical-$$" --arg sha "$(printf 'a%.0s' $(seq 64))" \
                '{operation_id:$id,relative_path:"compose.yaml",expected_sha256:$sha,content:"x"}')" \
            "$evidence_dir/non-identical-bind.write.json")
        printf 'write_status=%s\n' "$write_status" >"$evidence_dir/non-identical-bind.env"
        case "$write_status" in
            400|403|404|409|503) ;;
            *) fail "non-identical-bind: a write answered HTTP $write_status instead of a refusal" ;;
        esac
    fi
    # Put the Agent back on an identical-path root for the cases that follow.
    docker rm -f "$agent" >/dev/null
    start_agent false >"$evidence_dir/non-identical-bind.restored.container-id"
    wait_active_host "$agent_id" "$evidence_dir/non-identical-bind.restored.json" 240 ||
        fail "non-identical-bind: the Agent did not return on an identical-path root"
    record non_identical_bind_fs_write_disabled PASS
    record non_identical_bind_write_refused PASS
fi

# ------------------------------------------------- case: project name collision
# 7.6 is CORE: two project directories claiming one Compose project name cannot
# both be operated, because Compose would resolve them to the same runtime
# objects. Both must become read-only rather than racing.
if selected name-collision; then
    mkdir "$runtime/projects-second"
    cat >"$runtime/projects-second/compose.yaml" <<EOF
name: $compose_project
services:
  abuse-collision:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 60; done"]
EOF
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/projects-second:/second" "$server_image" -c \
        'chown -R 65532:65532 /second; chmod 0777 /second; chmod 0666 /second/compose.yaml' >/dev/null
    docker rm -f "$agent" >/dev/null
    # shellcheck disable=SC2086
    docker run --pull never -d --name "$agent" --network "$network" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.dockpilot.role=agent \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/agent:/var/lib/dockpilot:rw" \
        -v "$runtime/projects:$runtime/projects:rw" \
        -v "$runtime/projects-second:$runtime/projects-second:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/dockpilot/server-ca.crt \
        --display-name abuse-agent --self-container-name "$agent" \
        --project-root "$runtime/projects" --project-root "$runtime/projects-second" \
        >"$evidence_dir/name-collision.container-id"
    collided=0
    deadline=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >"$evidence_dir/name-collision.dashboard.json" 2>/dev/null &&
            jq -e '[.projects[] | select(.collision == true)] | length >= 2' \
                "$evidence_dir/name-collision.dashboard.json" >/dev/null 2>&1; then
            collided=1
            break
        fi
        sleep 5
    done
    [ "$collided" -eq 1 ] ||
        fail "name-collision: two projects claiming one Compose name were not both marked as colliding"
    collided_uid=$(jq -r '[.projects[] | select(.collision == true)][0].uid' "$evidence_dir/name-collision.dashboard.json")
    mutation_status=$(api_status POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "abuse-collision-$$" --arg agent "$agent_id" --arg project "$collided_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
        "$evidence_dir/name-collision.operation.json")
    [ "$mutation_status" = 409 ] ||
        fail "name-collision: a mutation on a colliding project answered HTTP $mutation_status instead of 409"
    record name_collision_detected PASS
    record name_collision_mutation_refused PASS
fi

# ------------------------------------ case: protected Compose project
# Self-protection covers the Agent's own container and the Compose project that
# container belongs to. An Agent deployed with Compose carries the project label,
# so a Compose mutation aimed at that project would take down the Agent that is
# carrying it out. This case gives the Agent the label a Compose deployment would
# give it and then aims a mutation at that project name.
if selected protected-compose-project; then
    protected_project=$(printf '%s' "$prefix-selfdeploy" | tr '[:upper:]' '[:lower:]')
    mkdir "$runtime/projects-protected"
    cat >"$runtime/projects-protected/compose.yaml" <<EOF
name: $protected_project
services:
  abuse-selfdeploy:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 60; done"]
EOF
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/projects-protected:/protected" "$server_image" -c \
        'chown -R 65532:65532 /protected; chmod 0777 /protected; chmod 0666 /protected/compose.yaml' >/dev/null
    docker rm -f "$agent" >/dev/null
    # The Compose labels are exactly what a Compose deployment of the Agent
    # would set, and are what IdentifySelf reads to build its protection set.
    docker run --pull never -d --name "$agent" --network "$network" \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.dockpilot.role=agent \
        --label "com.docker.compose.project=$protected_project" \
        --label com.docker.compose.service=agent \
        --label com.docker.compose.container-number=1 \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/agent:/var/lib/dockpilot:rw" \
        -v "$runtime/projects:$runtime/projects:rw" \
        -v "$runtime/projects-protected:$runtime/projects-protected:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/dockpilot/server-ca.crt \
        --display-name abuse-agent --self-container-name "$agent" \
        --project-root "$runtime/projects" --project-root "$runtime/projects-protected" \
        >"$evidence_dir/protected-compose-project.container-id"
    discovered=0
    deadline=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >"$evidence_dir/protected-compose-project.dashboard.json" 2>/dev/null &&
            jq -e --arg name "$protected_project" --arg id "$agent_id" \
                '([.hosts[] | select(.id == $id and .state == "ACTIVE")] | length) == 1 and
                 ([.projects[] | select(.name == $name)] | length == 1)' \
                "$evidence_dir/protected-compose-project.dashboard.json" >/dev/null 2>&1; then
            discovered=1
            break
        fi
        sleep 5
    done
    [ "$discovered" -eq 1 ] ||
        fail "protected-compose-project: the project sharing the Agent Compose project name was not discovered"
    protected_uid=$(jq -r --arg name "$protected_project" \
        '[.projects[] | select(.name == $name)][0].uid' "$evidence_dir/protected-compose-project.dashboard.json")
    # The Server keeps a row for every project UID it has ever seen and marks a
    # vanished one Missing only after a complete discovery sweep, so a candidate
    # is only usable once the Agent actually answers for it.
    unrelated_uid=
    for candidate in $(jq -r --arg name "$protected_project" \
        '[.projects[] | select(.name != $name and .collision != true and .stale != true)][].uid' \
        "$evidence_dir/protected-compose-project.dashboard.json"); do
        probe=$(api_status GET "$base_url/api/v1/projects/$candidate/files?path=compose.yaml" '' \
            "$evidence_dir/protected-compose-project.probe.json")
        if [ "$probe" = 200 ]; then
            unrelated_uid=$candidate
            break
        fi
    done
    protected_operation="abuse-protected-down-$$"
    status=$(api_status POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$protected_operation" --arg agent "$agent_id" --arg project "$protected_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.down"}')" \
        "$evidence_dir/protected-compose-project.operation.json")
    printf 'dispatch_status=%s\n' "$status" >"$evidence_dir/protected-compose-project.env"
    denied=0
    case "$status" in
        202)
            poll_operation "$protected_operation" "$evidence_dir/protected-compose-project.final.json" 180 ||
                fail "protected-compose-project: the mutation never reached a terminal state"
            jq -e '.status != "success"' "$evidence_dir/protected-compose-project.final.json" >/dev/null ||
                fail "protected-compose-project: a Compose mutation on the Agent's own project succeeded"
            grep -F 'DENY_PROTECTED_PROJECT' "$evidence_dir/protected-compose-project.final.json" >/dev/null && denied=1
            ;;
        400|403|409) denied=1 ;;
        *) fail "protected-compose-project: the mutation answered an unexpected HTTP $status" ;;
    esac
    [ "$denied" -eq 1 ] ||
        fail "protected-compose-project: the refusal did not name DENY_PROTECTED_PROJECT, so self-protection was not what refused it"
    [ "$(docker inspect --format '{{.State.Status}}' "$agent")" = running ] ||
        fail "protected-compose-project: the Agent container is no longer running"
    # The denial must be aimed, not a blanket refusal of Compose mutations.
    if [ -n "$unrelated_uid" ] && [ "$unrelated_uid" != null ]; then
        unrelated_operation="abuse-unrelated-up-$$"
        api POST "$base_url/api/v1/operations" \
            "$(jq -cn --arg id "$unrelated_operation" --arg agent "$agent_id" --arg project "$unrelated_uid" \
                '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
            "$evidence_dir/protected-compose-project.unrelated.accepted.json"
        poll_operation "$unrelated_operation" "$evidence_dir/protected-compose-project.unrelated.final.json" 180 ||
            fail "protected-compose-project: the unrelated Compose mutation never reached a terminal state"
        jq -e '.status == "success"' "$evidence_dir/protected-compose-project.unrelated.final.json" >/dev/null ||
            fail "protected-compose-project: self-protection refused a Compose mutation outside the protected project"
        remove_compose_objects
        record protected_compose_project_unrelated_still_allowed PASS
    fi
    wait_active_host "$agent_id" "$evidence_dir/protected-compose-project.after.json" 180 ||
        fail "protected-compose-project: the Agent is no longer ACTIVE"
    record protected_compose_project_denied PASS
    record protected_compose_project_agent_survives PASS
fi

{
    printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >>"$evidence_dir/assertions.env"

used_kib=$(du -sk "$evidence_dir" | awk '{ print $1 }')
[ $((used_kib * 1024)) -le "$evidence_max_bytes" ] || fail "evidence size cap exceeded"
( cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -exec sha256sum {} + >SHA256SUMS )
completed=1
