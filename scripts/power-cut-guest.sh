#!/bin/sh
set -eu

# Guest half of the power-cut gate. It runs inside a disposable lab VM and is
# driven by scripts/run-power-cut-e2e.sh on the host, which cuts the guest's
# power between the two phases.
#
#   setup    bring up a Server, an Agent, and a fixture project; do work that
#            the API reported as durable; record what must still be true
#   verify   after the guest has been powered off mid-flight and booted again,
#            check every recorded fact still holds
#
# Nothing here is a substitute for the process-kill cases in the hardening
# matrix. Those prove a process can die. This proves the *filesystem* can lose
# everything that was not on the platter: a SIGKILL still leaves the page cache
# intact and the kernel flushes it, and a power cut does not.
#
# Setup leaves a writer running. Cutting power to a guest that finished all its
# writes a minute ago mostly proves the kernel flushed them, which was never in
# doubt; the interesting moment is power disappearing *during* a write the API
# is in the middle of acknowledging. The writer rewrites one file in a loop and
# records every acknowledgement to a synced journal, so verification can say
# exactly which contents the product promised and check the file against them.

fail() {
    printf 'power-cut guest failed: %s\n' "$*" >&2
    exit 1
}

phase=${1:-}
runtime=${2:-}
[ -n "$phase" ] && [ -n "$runtime" ] || fail "usage: $0 setup|verify RUNTIME_DIR [SERVER_IMAGE AGENT_IMAGE FIXTURE_IMAGE]"
case "$runtime" in /*) ;; *) fail "runtime directory must be absolute" ;; esac

server=docklattice-powercut-server
agent=docklattice-powercut-agent
network=docklattice-powercut-net
state_file=$runtime/power-cut-state.env
secret_marker=POWERCUT-SECRET-DO-NOT-LEAK

for tool in docker jq curl openssl sha256sum awk; do
    command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool"
done

resolve_base_url() {
    server_port=$(docker port "$server" 8080/tcp | awk -F: 'NR == 1 { print $NF }')
    case "$server_port" in ''|*[!0-9]*) fail "could not resolve the Server HTTPS port" ;; esac
    base_url="https://127.0.0.1:$server_port"
}

wait_server_ready() {
    deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        curl --fail --silent --show-error --max-time 3 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/dashboard" >/dev/null 2>&1 && return 0
        sleep 2
    done
    fail "Server did not become HTTPS-ready"
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

api() {
    method=$1; url=$2; body=$3; output=$4
    if [ "$method" = GET ]; then
        curl --fail --silent --show-error --max-time 20 --cacert "$runtime/bootstrap/server-ca.crt" "$url" >"$output"
    else
        curl --fail --silent --show-error --max-time 20 --cacert "$runtime/bootstrap/server-ca.crt" \
            -H 'Content-Type: application/json' -X "$method" --data "$body" "$url" >"$output"
    fi
}

poll_operation() {
    operation_id=$1; output=$2; seconds=$3
    deadline=$(( $(date +%s) + seconds ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 5 --cacert "$runtime/bootstrap/server-ca.crt" \
            "$base_url/api/v1/agents/$agent_id/operations/$operation_id" >"$output.tmp" 2>/dev/null &&
            jq -e '.status == "success" or .status == "failed" or .status == "canceled" or
                   .status == "interrupted" or .status == "rejected"' "$output.tmp" >/dev/null 2>&1; then
            mv "$output.tmp" "$output"
            return 0
        fi
        sleep 2
    done
    return 1
}

agent_state_sh() {
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/agent:/state" "$server_image" -c "$1"
}

read_incarnation() {
    agent_state_sh 'cat /state/identity/agent-state.json 2>/dev/null || cat /state/agent-state.json 2>/dev/null || true' |
        jq -r '.current_incarnation // empty' 2>/dev/null || true
}

# The Agent derives a project UID as sha256(agent_id || NUL || canonical
# working directory). Computing it here means the fixture is chosen by the
# identity this script created, never by list position.
expected_fixture_uid() {
    printf '%s\000%s' "$1" "$2" | sha256sum | awk '{ print $1 }'
}


# Every attempt the writer makes has fully determined content, so verification
# can reconstruct any attempt from its number alone rather than trusting a
# record written by the process that was killed.
# No trailing newline: the content reaches the API through a command
# substitution, which strips them, and the digest has to be taken over exactly
# the bytes that were sent.
attempt_content() {
    printf '%s\n# durable marker %s' "$(cat "$runtime/projects/compose.baseline.yaml")" "$1"
}

attempt_sha() {
    attempt_content "$1" | sha256sum | awk '{ print $1 }'
}

case "$phase" in
setup)
    server_image=${3:-}; agent_image=${4:-}; fixture_image=${5:-}
    [ -n "$server_image" ] && [ -n "$agent_image" ] && [ -n "$fixture_image" ] ||
        fail "setup needs SERVER_IMAGE AGENT_IMAGE FIXTURE_IMAGE"
    [ ! -e "$runtime" ] || fail "refusing to reuse an existing runtime directory: $runtime"
    for image in "$server_image" "$agent_image" "$fixture_image"; do
        [ "$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null)" = "$image" ] ||
            fail "image reference did not resolve to its exact requested ID: $image"
    done
    [ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$server_image")" = "DockLattice Server" ] ||
        fail "the Server image is not a DockLattice Server image"
    [ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.title"}}' "$agent_image")" = "DockLattice Agent" ] ||
        fail "the Agent image is not a DockLattice Agent image"

    mkdir -p "$runtime"
    mkdir "$runtime/server" "$runtime/server/tls" "$runtime/agent" "$runtime/bootstrap" "$runtime/projects" "$runtime/evidence"
    compose_project=docklattice-powercut-fixture
    socket_gid=$(stat -c '%g' /var/run/docker.sock)
    openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
        -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
        -keyout "$runtime/server/tls/server.key" -out "$runtime/server/tls/server.crt" \
        >"$runtime/evidence/openssl.stdout" 2>"$runtime/evidence/openssl.stderr"
    cp "$runtime/server/tls/server.crt" "$runtime/bootstrap/server-ca.crt"
    cat >"$runtime/projects/compose.yaml" <<EOF
name: $compose_project
services:
  powercut-fixture:
    image: $fixture_image
    pull_policy: never
    command: ["/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 60; done"]
EOF
    printf 'POWERCUT_SECRET=%s\n' "$secret_marker" >"$runtime/projects/.env"
    # The writer's attempts are this file plus one marker line, so the
    # unmarked original is kept where verification can rebuild them from it.
    cp "$runtime/projects/compose.yaml" "$runtime/projects/compose.baseline.yaml"
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/powercut" "$server_image" -c \
        'chown -R 65532:65532 /powercut/server /powercut/agent; chmod 0700 /powercut/server /powercut/agent /powercut/server/tls; chmod 0600 /powercut/server/tls/server.crt /powercut/server/tls/server.key; chown -R 65532:65532 /powercut/projects; chmod 0777 /powercut/projects; chmod 0666 /powercut/projects/compose.yaml /powercut/projects/.env' \
        >/dev/null

    # 198.18.0.0/15 is RFC 2544 benchmarking space: never routed, so claiming
    # it cannot collide with anything the guest actually talks to.
    docker network create --subnet 198.18.240.0/24 "$network" >"$runtime/evidence/network.id"

    # Both containers carry a restart policy because that is how a real
    # deployment survives a power cut, and it is the path under test: after the
    # guest boots, nothing here is allowed to hand-start them.
    docker run --pull never -d --name "$server" --network "$network" --network-alias server \
        --restart unless-stopped \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        -p 127.0.0.1::8080 -v "$runtime/server:/var/lib/docklattice:rw" "$server_image" \
        server --listen 0.0.0.0:8080 --agent-listen 0.0.0.0:8443 --allow-public-bind >/dev/null
    resolve_base_url
    wait_server_ready

    docker run --pull never --rm --user 65532:65532 \
        -v "$runtime/server:/var/lib/docklattice:rw" "$server_image" \
        server issue-token --state-dir /var/lib/docklattice --ttl 15m \
        >"$runtime/bootstrap/join-token" 2>"$runtime/evidence/issue-token.stderr"
    [ "$(wc -c <"$runtime/bootstrap/join-token" | awk '{ print $1 }')" -gt 1 ] || fail "Join Token CLI produced no token"
    docker run --pull never --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/agent:/agent" -v "$runtime/bootstrap:/bootstrap:ro" "$server_image" -c \
        'cp /bootstrap/server-ca.crt /agent/server-ca.crt; cp /bootstrap/join-token /agent/join-token; chown -R 65532:65532 /agent; chmod 0700 /agent; chmod 0600 /agent/server-ca.crt /agent/join-token' >/dev/null
    rm -f "$runtime/bootstrap/join-token"

    docker run --pull never -d --name "$agent" --network "$network" \
        --restart unless-stopped \
        --log-driver local --log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/agent:/var/lib/docklattice:rw" \
        -v "$runtime/projects:$runtime/projects:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/docklattice/server-ca.crt \
        --join-token-file /var/lib/docklattice/join-token \
        --display-name powercut-agent --self-container-name "$agent" \
        --project-root "$runtime/projects" >/dev/null

    wait_active_host "" "$runtime/evidence/dashboard.baseline.json" 240 ||
        fail "the Agent did not register before the power cut"
    agent_id=$(jq -r '.hosts[0].id' "$runtime/evidence/dashboard.baseline.json")
    [ -n "$agent_id" ] && [ "$agent_id" != null ] || fail "the baseline dashboard omitted the Agent id"
    derived_uid=$(expected_fixture_uid "$agent_id" "$runtime/projects")
    jq -e --arg uid "$derived_uid" '[.projects[]? | select(.uid == $uid)] | length == 1' \
        "$runtime/evidence/dashboard.baseline.json" >/dev/null ||
        fail "the dashboard does not list the fixture project under the uid derived from its identity"
    project_uid=$derived_uid

    # Durable work, in the order the API says it becomes durable. Each of these
    # is a promise the product made *before* the power was cut, and each one is
    # re-checked after the guest comes back.
    up_operation="powercut-up-$$"
    api POST "$base_url/api/v1/operations" \
        "$(jq -cn --arg id "$up_operation" --arg agent "$agent_id" --arg project "$project_uid" \
            '{operation_id:$id,agent_id:$agent,project_uid:$project,kind:"compose.up"}')" \
        "$runtime/evidence/compose-up.accepted.json"
    poll_operation "$up_operation" "$runtime/evidence/compose-up.final.json" 240 ||
        fail "the preparatory compose.up never reached a terminal state"
    jq -e '.status == "success"' "$runtime/evidence/compose-up.final.json" >/dev/null ||
        fail "the preparatory compose.up did not succeed"

    api GET "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" '' \
        "$runtime/evidence/file.read.json"
    base_sha=$(jq -r '.sha256' "$runtime/evidence/file.read.json")
    [ -n "$base_sha" ] && [ "$base_sha" != null ] || fail "the file read did not return a sha256"
    write_operation="powercut-write-$$"
    api PUT "$base_url/api/v1/projects/$project_uid/files" \
        "$(jq -cn --arg id "$write_operation" --arg path compose.yaml --arg sha "$base_sha" \
            --arg content "$(attempt_content 1)" \
            '{operation_id:$id,relative_path:$path,expected_sha256:$sha,content:$content}')" \
        "$runtime/evidence/file.write.accepted.json"
    poll_operation "$write_operation" "$runtime/evidence/file.write.final.json" 180 ||
        fail "the durable write never reached a terminal state"
    jq -e '.status == "success"' "$runtime/evidence/file.write.final.json" >/dev/null ||
        fail "the durable write did not succeed"
    written_sha=$(sha256sum "$runtime/projects/compose.yaml" | awk '{ print $1 }')
    [ "$written_sha" = "$(attempt_sha 1)" ] ||
        fail "the first acknowledged write did not produce the content it was given"

    api GET "$base_url/api/v1/hosts/$agent_id/audit?limit=200" '' "$runtime/evidence/audit.before.json"
    audit_before=$(jq '[.events[]] | length' "$runtime/evidence/audit.before.json")
    incarnation_before=$(read_incarnation)
    [ -n "$incarnation_before" ] || fail "could not read the Agent incarnation before the power cut"
    fixture_container=$(docker ps -q --filter "label=com.docker.compose.project=$compose_project" | head -1)
    [ -n "$fixture_container" ] || fail "compose.up did not leave a running fixture container"

    cat >"$state_file" <<STATE
runtime=$runtime
server_image=$server_image
agent_image=$agent_image
fixture_image=$fixture_image
compose_project=$compose_project
agent_id=$agent_id
project_uid=$project_uid
up_operation=$up_operation
write_operation=$write_operation
written_sha=$written_sha
audit_before=$audit_before
incarnation_before=$incarnation_before
fixture_container=$fixture_container
secret_marker=$secret_marker
STATE
    # The state file has to survive the cut it is describing, so it is the one
    # thing this script flushes by hand. Everything else is the product's job.
    sync
    # The writer outlives this script on purpose: the host cuts power while it
    # is mid-loop, which is the moment the gate exists to observe.
    : >"$runtime/writer.journal"
    printf 'acked 1\n' >>"$runtime/writer.journal"
    sync "$runtime/writer.journal"
    setsid nohup "$0" writer "$runtime" >"$runtime/evidence/writer.log" 2>&1 </dev/null &
    # Give it enough of a head start that power lands inside the loop rather
    # than before it.
    sleep 5
    [ "$(wc -l <"$runtime/writer.journal" | awk '{ print $1 }')" -ge 2 ] ||
        fail "the writer did not acknowledge a second write; nothing would be in flight at the cut"
    printf 'setup=OK agent_id=%s project_uid=%s incarnation=%s audit_events=%s acked=%s\n' \
        "$agent_id" "$project_uid" "$incarnation_before" "$audit_before" \
        "$(wc -l <"$runtime/writer.journal" | awk '{ print $1 }')"
    ;;

writer)
    # One file, rewritten forever, with every acknowledgement recorded to a
    # journal this script flushes by hand. The journal is deliberately more
    # durable than what it is measuring: if an acknowledgement survives the cut
    # and the content it acknowledged does not, that is the finding.
    [ -f "$state_file" ] || fail "the writer has no state file"
    # shellcheck disable=SC1090
    . "$state_file"
    resolve_base_url
    attempt=2
    last_sha=$(attempt_sha 1)
    while [ "$attempt" -lt 100000 ]; do
        operation="powercut-writer-$attempt"
        code=$(curl --silent --show-error --max-time 20 --output "$runtime/evidence/writer.accepted.json" \
            --write-out '%{http_code}' --cacert "$runtime/bootstrap/server-ca.crt" \
            -H 'Content-Type: application/json' -X PUT \
            --data "$(jq -cn --arg id "$operation" --arg path compose.yaml --arg sha "$last_sha" \
                --arg content "$(attempt_content "$attempt")" \
                '{operation_id:$id,relative_path:$path,expected_sha256:$sha,content:$content}')" \
            "$base_url/api/v1/projects/$project_uid/files" 2>/dev/null) || code=000
        if [ "$code" = 202 ] &&
            poll_operation "$operation" "$runtime/evidence/writer.final.json" 60 &&
            jq -e '.status == "success"' "$runtime/evidence/writer.final.json" >/dev/null 2>&1; then
            printf 'acked %s\n' "$attempt" >>"$runtime/writer.journal"
            sync "$runtime/writer.journal"
            last_sha=$(attempt_sha "$attempt")
        else
            # A refusal is not a failure of this loop; the writer only has to
            # keep the journal honest about what was acknowledged.
            observed=$(curl --fail --silent --max-time 10 --cacert "$runtime/bootstrap/server-ca.crt" \
                "$base_url/api/v1/projects/$project_uid/files?path=compose.yaml" 2>/dev/null |
                jq -r '.sha256 // empty' 2>/dev/null || true)
            [ -z "$observed" ] || last_sha=$observed
        fi
        attempt=$((attempt + 1))
    done
    ;;

verify)
    [ -f "$state_file" ] || fail "no state file survived the power cut: $state_file"
    # shellcheck disable=SC1090
    . "$state_file"
    evidence=$runtime/evidence
    mkdir -p "$evidence"
    record() { printf '%s=%s\n' "$1" "$2" >>"$evidence/power-cut.assertions.env"; }
    : >"$evidence/power-cut.assertions.env"

    # Nothing is hand-started. Both containers carry a restart policy, and if
    # the product cannot come back on its own that is the finding.
    deadline=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        [ "$(docker inspect --format '{{.State.Running}}' "$server" 2>/dev/null)" = true ] &&
            [ "$(docker inspect --format '{{.State.Running}}' "$agent" 2>/dev/null)" = true ] && break
        sleep 3
    done
    [ "$(docker inspect --format '{{.State.Running}}' "$server" 2>/dev/null)" = true ] ||
        fail "the Server container did not restart itself after the power cut"
    [ "$(docker inspect --format '{{.State.Running}}' "$agent" 2>/dev/null)" = true ] ||
        fail "the Agent container did not restart itself after the power cut"
    record containers_restarted_themselves PASS

    resolve_base_url
    wait_server_ready
    record server_answers_after_power_cut PASS

    wait_active_host "$agent_id" "$evidence/dashboard.after.json" 300 ||
        fail "the Agent did not return ACTIVE with its original identity after the power cut"
    record agent_returned_active_same_identity PASS

    jq -e --arg uid "$project_uid" '[.projects[]? | select(.uid == $uid)] | length == 1' \
        "$evidence/dashboard.after.json" >/dev/null ||
        fail "the fixture project is not listed under its original uid after the power cut"
    record project_uid_stable PASS

    # A power cut is exactly the case where a write the API called durable can
    # turn out not to have been. This is the whole point of the gate.
    on_disk=$(sha256sum "$runtime/projects/compose.yaml" | awk '{ print $1 }')
    [ -f "$runtime/writer.journal" ] || fail "the writer journal did not survive the power cut"
    last_acked=$(awk '$1 == "acked" { value = $2 } END { print value + 0 }' "$runtime/writer.journal")
    [ "$last_acked" -ge 2 ] ||
        fail "the writer journal records only $last_acked acknowledged writes; the cut did not land in the loop"
    record writer_acknowledged_writes "$last_acked"

    # An acknowledged write is a durability promise. The file may also hold the
    # very next attempt - acknowledged to a process that was killed before it
    # could journal - but it may never hold anything older, and it may never
    # hold a blend of two attempts.
    if [ "$on_disk" = "$(attempt_sha "$last_acked")" ]; then
        record durable_write_survived "attempt=$last_acked"
    elif [ "$on_disk" = "$(attempt_sha $(( last_acked + 1 )) )" ]; then
        record durable_write_survived "attempt=$(( last_acked + 1 )) (acknowledged after the last journal flush)"
    else
        marker_lines=$(grep -c '^# durable marker ' "$runtime/projects/compose.yaml" || true)
        printf 'on_disk=%s\nlast_acked=%s\nexpected=%s\nmarker_lines=%s\n' \
            "$on_disk" "$last_acked" "$(attempt_sha "$last_acked")" "$marker_lines" \
            >"$evidence/durability.mismatch.env"
        cp "$runtime/projects/compose.yaml" "$evidence/compose.after-power-cut.yaml" 2>/dev/null || true
        fail "the file on disk is neither the last acknowledged write ($last_acked) nor the one after it; an acknowledged write did not survive the power cut"
    fi

    # Whatever it holds, it has to be one whole attempt: exactly one marker
    # line and nothing truncated behind it.
    [ "$(grep -c '^# durable marker ' "$runtime/projects/compose.yaml")" -eq 1 ] ||
        fail "the file carries more than one durable marker; a write was torn across the power cut"
    record file_not_torn PASS

    api GET "$base_url/api/v1/agents/$agent_id/operations/$write_operation" '' "$evidence/write.after.json"
    jq -e '.status == "success"' "$evidence/write.after.json" >/dev/null ||
        fail "the Server no longer reports the successful write as successful"
    api GET "$base_url/api/v1/agents/$agent_id/operations/$up_operation" '' "$evidence/up.after.json"
    jq -e '.status == "success"' "$evidence/up.after.json" >/dev/null ||
        fail "the Server no longer reports the successful compose.up as successful"
    record acknowledged_operations_survived PASS

    # An Agent that lost power did not close cleanly, and the audit stream has
    # to say so rather than quietly resuming.
    incarnation_after=$(read_incarnation)
    [ -n "$incarnation_after" ] || fail "could not read the Agent incarnation after the power cut"
    [ "$incarnation_after" -gt "$incarnation_before" ] ||
        fail "the incarnation did not advance across the power cut ($incarnation_before -> $incarnation_after)"
    record incarnation_advanced "$incarnation_before->$incarnation_after"

    found=0
    deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        api GET "$base_url/api/v1/hosts/$agent_id/audit?limit=200" '' "$evidence/audit.after.json"
        if jq -e --argjson previous "$incarnation_before" '
              [.events[] | select(.kind == "AUDIT_CONTINUITY_UNCERTAIN" and .previous_incarnation == $previous)] | length >= 1
            ' "$evidence/audit.after.json" >/dev/null 2>&1; then
            found=1
            break
        fi
        sleep 5
    done
    [ "$found" -eq 1 ] ||
        fail "no AUDIT_CONTINUITY_UNCERTAIN was recorded for the incarnation that lost power"
    record audit_continuity_uncertain_recorded PASS

    # Audit is append-only. A power cut may lose the tail that was never
    # acknowledged; it may never lose or rewrite what was already there.
    audit_after=$(jq '[.events[]] | length' "$evidence/audit.after.json")
    [ "$audit_after" -ge "$audit_before" ] ||
        fail "the audit page shrank across the power cut ($audit_before -> $audit_after)"
    record audit_did_not_shrink "$audit_before->$audit_after"

    # The project secret must not have been dragged into audit or an operation
    # result by any recovery path.
    if grep -RIl "$secret_marker" "$evidence" >/dev/null 2>&1; then
        fail "the project secret appeared in recovery evidence"
    fi
    record secret_not_leaked PASS

    printf 'verify=OK\n'
    ;;

teardown)
    pkill -f "power-cut-guest.sh writer" >/dev/null 2>&1 || true
    docker rm -f "$server" "$agent" >/dev/null 2>&1 || true
    if [ -f "$state_file" ]; then
        # shellcheck disable=SC1090
        . "$state_file"
        docker ps -aq --filter "label=com.docker.compose.project=$compose_project" |
            while read -r id; do [ -n "$id" ] && docker rm -f "$id" >/dev/null 2>&1 || true; done
    fi
    docker network rm "$network" >/dev/null 2>&1 || true
    printf 'teardown=OK\n'
    ;;

*)
    fail "unknown phase: $phase"
    ;;
esac
