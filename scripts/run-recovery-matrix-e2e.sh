#!/bin/sh
set -eu

# Real-container recovery matrix for the two Server-side loss outcomes that
# section 6.1 of docs/architecture.md distinguishes:
#
#   Audit DB lost, Identity State kept -> the existing Agent authenticates
#     automatically, archive_generation advances, and the archive rebinds.
#   Identity State lost                -> a different server_identity_id, so the
#     existing Agent must not be accepted and manual re-registration is needed.
#
# The package-level matrix in internal/serverbootstrap covers the bootstrap
# decision itself. This gate proves the same two outcomes across a real Server
# container, a real Agent container, and a real reconnect.

usage() {
    printf 'usage: %s ABSOLUTE_EVIDENCE_DIR SERVER_IMAGE_ID AGENT_IMAGE_ID\n' "$0" >&2
    printf 'both image arguments must be exact local sha256 image IDs\n' >&2
}

fail() {
    printf 'recovery matrix E2E failed: %s\n' "$*" >&2
    failure_reason=$*
    exit 1
}

[ "$#" -eq 3 ] || {
    usage
    exit 2
}

evidence_dir=$1
server_image=$2
agent_image=$3
evidence_max_bytes=${RECOVERY_EVIDENCE_MAX_BYTES:-16777216}
log_max_bytes=${RECOVERY_LOG_MAX_BYTES:-1048576}

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
[ "$server_image" != "$agent_image" ] || fail "preflight: Server and Agent image IDs must be distinct"

case "$evidence_max_bytes" in
    ''|*[!0-9]*) fail "preflight: RECOVERY_EVIDENCE_MAX_BYTES must be an integer" ;;
esac
case "$log_max_bytes" in
    ''|*[!0-9]*) fail "preflight: RECOVERY_LOG_MAX_BYTES must be an integer" ;;
esac
[ "$evidence_max_bytes" -ge 4194304 ] && [ "$evidence_max_bytes" -le 67108864 ] ||
    fail "preflight: evidence cap must be between 4 MiB and 64 MiB"
[ "$log_max_bytes" -ge 65536 ] && [ "$log_max_bytes" -le 4194304 ] ||
    fail "preflight: log cap must be between 64 KiB and 4 MiB"

command -v docker >/dev/null 2>&1 || fail "preflight: docker is required"
for command_name in openssl curl jq awk date du df mktemp head wc find chmod stat cat mv rm rmdir mkdir sleep tr; do
    command -v "$command_name" >/dev/null 2>&1 || fail "preflight: required command not found: $command_name"
done
[ -z "${DOCKER_HOST:-}" ] || fail "preflight: DOCKER_HOST is not supported; use the local default Engine socket"
docker info >/dev/null 2>&1 || fail "preflight: Docker daemon is unavailable or permission is denied"
[ "$(docker info --format '{{.OSType}}')" = linux ] || fail "preflight: a Linux Docker Engine is required"
[ -S /var/run/docker.sock ] && [ -r /var/run/docker.sock ] && [ -w /var/run/docker.sock ] ||
    fail "preflight: readable and writable /var/run/docker.sock is required"

for image in "$server_image" "$agent_image"; do
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
case "$server_revision" in ''|*[!0-9a-f]*) fail "preflight: production revision must be lowercase hexadecimal" ;; esac
[ "${#server_revision}" -eq 40 ] || fail "preflight: production revision must be a full 40-character Git object ID"

available_kib=$(df -Pk "$evidence_parent" | awk 'NR == 2 { print $4 }')
[ "$available_kib" -ge $((evidence_max_bytes / 1024 + 65536)) ] ||
    fail "preflight: evidence filesystem needs the cap plus 64 MiB free"

runtime_base=${TMPDIR:-/tmp}
case "$runtime_base" in /*) ;; *) fail "preflight: TMPDIR must be absolute" ;; esac
case "$runtime_base" in *:*|*'
'*) fail "preflight: TMPDIR cannot contain colon or newline" ;; esac
[ "$runtime_base" != / ] || fail "preflight: TMPDIR cannot be the filesystem root"
[ -d "$runtime_base" ] || fail "preflight: TMPDIR does not exist"

prefix="dockpilot-recovery-$(date -u +%Y%m%dT%H%M%SZ)-$$"
umask 077
artifact_created=0
runtime=
server="$prefix-server"
agent="$prefix-agent"
network="$prefix-network"
completed=0
failure_reason="harness did not complete"

capture_container_state() {
    docker inspect --format '{{.Name}} status={{.State.Status}} exit={{.State.ExitCode}} restarts={{.RestartCount}} started={{.State.StartedAt}} finished={{.State.FinishedAt}}' \
        "$1" >"$2" 2>&1 || true
}

capture_log() {
    container=$1
    output=$2
    docker inspect "$container" >/dev/null 2>&1 || return 0
    docker logs --tail 2000 "$container" 2>&1 | head -c "$log_max_bytes" >"$output" || true
}

scrub_runtime() {
    [ -n "${runtime:-}" ] && [ -d "$runtime" ] || return 0
    case "$runtime" in "$runtime_base"/dockpilot-recovery.*) ;; *) return 1 ;; esac
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/dockpilot-recovery-runtime" "$server_image" \
        -c 'rm -rf /dockpilot-recovery-runtime/server /dockpilot-recovery-runtime/agent /dockpilot-recovery-runtime/bootstrap /dockpilot-recovery-runtime/projects' \
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

runtime=$(mktemp -d "$runtime_base/dockpilot-recovery.XXXXXXXX")
case "$runtime" in "$runtime_base"/dockpilot-recovery.*) ;; *) fail "mktemp returned an unexpected runtime root" ;; esac
chmod 0700 "$runtime"
mkdir "$evidence_dir"
chmod 0700 "$evidence_dir"
artifact_created=1
{
    printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'docker_server_version=%s\n' "$(docker info --format '{{.ServerVersion}}')"
    printf 'server_image_id=%s\n' "$server_image"
    printf 'agent_image_id=%s\n' "$agent_image"
    printf 'release_version=%s\n' "$server_version"
    printf 'release_revision=%s\n' "$server_revision"
} >"$evidence_dir/environment.env"

mkdir "$runtime/server" "$runtime/server/tls" "$runtime/agent" "$runtime/bootstrap" "$runtime/projects"
socket_gid=$(stat -c '%g' /var/run/docker.sock)
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
    -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
    -keyout "$runtime/server/tls/server.key" -out "$runtime/server/tls/server.crt" \
    >"$evidence_dir/openssl.stdout" 2>"$evidence_dir/openssl.stderr"
cp "$runtime/server/tls/server.crt" "$runtime/bootstrap/server-ca.crt"
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
    -v "$runtime:/recovery" "$server_image" -c \
    'chown -R 65532:65532 /recovery/server /recovery/agent; chmod 0700 /recovery/server /recovery/agent /recovery/server/tls; chmod 0600 /recovery/server/tls/server.crt /recovery/server/tls/server.key; chmod 0755 /recovery/projects' \
    >/dev/null

# The Server state root is 0700 and owned by 65532, so every inspection and
# every deliberate loss is performed through a root helper container.
server_state_sh() {
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/server:/state" "$server_image" -c "$1"
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
        -p 127.0.0.1::8080 -v "$runtime/server:/var/lib/dockpilot:rw" "$server_image" \
        server --listen 0.0.0.0:8080 --agent-listen 0.0.0.0:8443 --allow-public-bind
}

resolve_base_url() {
    server_port=$(docker port "$server" 8080/tcp | awk -F: 'NR == 1 { print $NF }')
    case "$server_port" in ''|*[!0-9]*) fail "could not resolve the Server HTTPS port" ;; esac
    base_url="https://127.0.0.1:$server_port"
}

wait_server_ready() {
    deadline=$(( $(date +%s) + 60 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 3 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    fail "Server did not become HTTPS-ready"
}

# Returns 0 as soon as exactly one ACTIVE host with the given id is visible.
wait_active_host() {
    expected=$1
    output=$2
    seconds=$3
    deadline=$(( $(date +%s) + seconds ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >"$output.tmp" 2>/dev/null &&
            jq -e --arg expected "$expected" '
              (.hosts | length) == 1 and
              .hosts[0].state == "ACTIVE" and
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
        --display-name recovery-agent --self-container-name "$agent" \
        --project-root "$runtime/projects"
}

# The Server container is stopped and started again rather than recreated, which
# is how an operator restarts the service around a state repair.
restart_server() {
    docker start "$server" >"$evidence_dir/server.restart.$1"
    resolve_base_url
    wait_server_ready
}

# ---------------------------------------------------------------- baseline
start_server >"$evidence_dir/server.container-id.baseline"
resolve_base_url
wait_server_ready
issue_token
start_agent true >"$evidence_dir/agent.container-id"
wait_active_host "" "$evidence_dir/dashboard.baseline.json" 120 ||
    fail "baseline registration did not produce exactly one ACTIVE host"
agent_id=$(jq -r '.hosts[0].id' "$evidence_dir/dashboard.baseline.json")
[ -n "$agent_id" ] && [ "$agent_id" != null ] || fail "baseline dashboard omitted the Agent id"
server_state_sh 'rm -f /state/join-token' >/dev/null 2>&1 || true
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh -v "$runtime/agent:/agent" "$server_image" \
    -c 'rm -f /agent/join-token' >/dev/null
identity_baseline=$(read_identity_field server_identity_id)
generation_baseline=$(read_identity_field archive_generation)
case "$generation_baseline" in ''|*[!0-9]*) fail "baseline archive_generation is not an integer" ;; esac

# ------------------------------- control: Server restart with nothing lost
# Same identity, same generation, same archive_id is "normal reconnect" in the
# table in section 6.4. It runs first so that a reconnect failure in either loss
# case cannot be blamed on the restart mechanics themselves.
docker stop "$server" >/dev/null
restart_server control
if ! wait_active_host "$agent_id" "$evidence_dir/dashboard.control.json" 300; then
    capture_container_state "$agent" "$evidence_dir/control.agent-state"
    capture_log "$agent" "$evidence_dir/control.agent.log"
    fail "control: the Agent did not reconnect after a Server restart that lost nothing"
fi

# ------------------------------------------- case 1: Audit DB lost, Identity kept
docker stop "$server" >/dev/null
server_state_sh 'rm -f /state/server.db /state/server.db-wal /state/server.db-shm' >/dev/null
server_state_sh '[ -e /state/server.db ] && echo present || echo absent' >"$evidence_dir/case1.database-removed"
grep -q absent "$evidence_dir/case1.database-removed" || fail "case 1: the Audit database was not removed"
server_state_sh '[ -e /state/identity/server-identity.json ] && echo present || echo absent' >"$evidence_dir/case1.identity-kept"
grep -q present "$evidence_dir/case1.identity-kept" || fail "case 1: the Identity State was removed by mistake"
restart_server case1
identity_case1=$(read_identity_field server_identity_id)
generation_case1=$(read_identity_field archive_generation)
[ "$identity_case1" = "$identity_baseline" ] ||
    fail "case 1: server_identity_id changed although the Identity State was preserved"
[ "$generation_case1" -gt "$generation_baseline" ] ||
    fail "case 1: archive_generation did not advance after Audit database loss"
# The Agent container is never touched and holds no Join Token, so reaching
# ACTIVE again can only come from automatic credential authentication.
wait_active_host "$agent_id" "$evidence_dir/dashboard.case1.json" 300 ||
    fail "case 1: the existing Agent did not reconnect automatically with its original identity"
capture_log "$agent" "$evidence_dir/agent.case1.log"
capture_log "$server" "$evidence_dir/server.case1.log"

# --------------- case 2: Identity State lost, Audit database preserved
# Section 6.1 gives this outcome a different name from a database loss: the
# Server would present a different server_identity_id over an Audit Archive that
# belongs to the old one, so it must fail closed rather than adopt the archive.
docker stop "$server" >/dev/null
server_state_sh 'rm -rf /state/identity' >/dev/null
server_state_sh '[ -e /state/identity ] && echo present || echo absent' >"$evidence_dir/case2.identity-removed"
grep -q absent "$evidence_dir/case2.identity-removed" || fail "case 2: the Identity State was not removed"
server_state_sh '[ -e /state/server.db ] && echo present || echo absent' >"$evidence_dir/case2.database-kept"
grep -q present "$evidence_dir/case2.database-kept" || fail "case 2: the Audit database was removed by mistake"
docker start "$server" >"$evidence_dir/server.restart.case2"
refused=0
deadline=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
    state=$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$server")
    case "$state" in
        "exited "*)
            [ "$state" != "exited 0" ] || fail "case 2: the Server exited successfully instead of failing closed"
            refused=1
            break
            ;;
    esac
    sleep 1
done
capture_log "$server" "$evidence_dir/case2.server.log"
[ "$refused" -eq 1 ] || fail "case 2: the Server did not fail closed after losing only its Identity State"
grep -q 'another Server Identity' "$evidence_dir/case2.server.log" ||
    fail "case 2: the Server failed without naming the Archive identity refusal"

# ---------- case 3: both Server stores lost, so a new trust domain is created
# With no Identity State and no database there is nothing to contradict, and
# section 6.1 requires manual re-registration because existing Agents now face a
# different server_identity_id.
server_state_sh 'rm -f /state/server.db /state/server.db-wal /state/server.db-shm' >/dev/null
restart_server case3
identity_case3=$(read_identity_field server_identity_id)
[ -n "$identity_case3" ] && [ "$identity_case3" != null ] || fail "case 3: the Server did not create a new Identity State"
[ "$identity_case3" != "$identity_baseline" ] ||
    fail "case 3: server_identity_id was reused after both stores were lost"
# The old credential is bound to the lost identity, so the untouched Agent must
# stay out. A short window is deliberate: this asserts an absence.
if wait_active_host "$agent_id" "$evidence_dir/dashboard.case3.json" 120; then
    fail "case 3: the Agent was accepted although the Server identity changed"
fi
curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
    "$base_url/api/v1/dashboard" >"$evidence_dir/dashboard.case3.json" 2>/dev/null ||
    fail "case 3: dashboard is unavailable after both stores were lost"
jq -e '[.hosts[] | select(.state == "ACTIVE")] | length == 0' "$evidence_dir/dashboard.case3.json" >/dev/null ||
    fail "case 3: an ACTIVE host is reported although no Agent can authenticate"
capture_log "$agent" "$evidence_dir/agent.case3.log"

# Manual re-registration: a fresh Agent state plus a new Join Token.
docker rm -f "$agent" >/dev/null
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh -v "$runtime/agent:/agent" "$server_image" \
    -c 'rm -rf /agent/..?* /agent/.[!.]* /agent/*' >/dev/null 2>&1 || true
issue_token
start_agent true >"$evidence_dir/agent.container-id.reregistered"
wait_active_host "" "$evidence_dir/dashboard.case3-reregistered.json" 120 ||
    fail "case 3: manual re-registration did not produce an ACTIVE host"
reregistered_agent_id=$(jq -r '.hosts[0].id' "$evidence_dir/dashboard.case3-reregistered.json")
[ -n "$reregistered_agent_id" ] && [ "$reregistered_agent_id" != null ] ||
    fail "case 3: re-registered dashboard omitted the Agent id"
docker run --pull never --rm --user 0:0 --entrypoint /bin/sh -v "$runtime/agent:/agent" "$server_image" \
    -c 'rm -f /agent/join-token' >/dev/null

{
    printf 'server_image_id=%s\n' "$server_image"
    printf 'agent_image_id=%s\n' "$agent_image"
    printf 'release_version=%s\n' "$server_version"
    printf 'release_revision=%s\n' "$server_revision"
    printf 'baseline_agent_id=%s\n' "$agent_id"
    printf 'baseline_server_identity_id=%s\n' "$identity_baseline"
    printf 'baseline_archive_generation=%s\n' "$generation_baseline"
    printf 'case1_server_identity_id=%s\n' "$identity_case1"
    printf 'case1_archive_generation=%s\n' "$generation_case1"
    printf 'case3_server_identity_id=%s\n' "$identity_case3"
    printf 'case3_reregistered_agent_id=%s\n' "$reregistered_agent_id"
    printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'plain_restart_reconnect=PASS\n'
    printf 'database_loss_identity_preserved=PASS\n'
    printf 'database_loss_generation_advanced=PASS\n'
    printf 'database_loss_automatic_reconnect=PASS\n'
    printf 'identity_loss_with_database_fails_closed=PASS\n'
    printf 'both_stores_lost_new_trust_domain=PASS\n'
    printf 'both_stores_lost_old_agent_rejected=PASS\n'
    printf 'both_stores_lost_manual_reregistration=PASS\n'
} >"$evidence_dir/assertions.env"

used_kib=$(du -sk "$evidence_dir" | awk '{ print $1 }')
[ $((used_kib * 1024)) -le "$evidence_max_bytes" ] || fail "evidence size cap exceeded"
( cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -exec sha256sum {} + >SHA256SUMS )
completed=1
