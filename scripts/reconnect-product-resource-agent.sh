#!/bin/sh
set -eu

fail() {
    printf 'resource Agent reconnect failed: %s\n' "$*" >&2
    exit 1
}

for name in DOCKLATTICE_AGENT_CONTAINER DOCKLATTICE_AGENT_IMAGE DOCKLATTICE_AGENT_NETWORK \
    DOCKLATTICE_AGENT_STATE_DIR DOCKLATTICE_PROJECT_ROOT DOCKLATTICE_AGENT_SOCKET_GID \
    DOCKLATTICE_AGENT_DISPLAY_NAME DOCKLATTICE_RECONNECT_MARKER DOCKLATTICE_RECONNECT_CGROUP_EVIDENCE; do
    eval "value=\${$name:-}"
    [ -n "$value" ] || fail "required environment is missing: $name"
done
eval "gctrace_evidence=\${DOCKLATTICE_RECONNECT_GCTRACE_EVIDENCE:-}"
[ -n "$gctrace_evidence" ] || fail "required environment is missing: DOCKLATTICE_RECONNECT_GCTRACE_EVIDENCE"

case "$DOCKLATTICE_AGENT_STATE_DIR" in /*) ;; *) fail "Agent state directory must be absolute" ;; esac
case "$DOCKLATTICE_PROJECT_ROOT" in /*) ;; *) fail "project root must be absolute" ;; esac
case "$DOCKLATTICE_RECONNECT_MARKER" in /*) ;; *) fail "reconnect marker must be absolute" ;; esac
case "$DOCKLATTICE_RECONNECT_CGROUP_EVIDENCE" in /*) ;; *) fail "cgroup evidence path must be absolute" ;; esac
case "$gctrace_evidence" in /*) ;; *) fail "gctrace evidence path must be absolute" ;; esac
case "$DOCKLATTICE_AGENT_SOCKET_GID" in ''|*[!0-9]*) fail "Docker socket GID must be numeric" ;; esac
[ ! -e "$DOCKLATTICE_RECONNECT_CGROUP_EVIDENCE" ] || fail "refusing to overwrite reconnect evidence"
[ ! -e "$gctrace_evidence" ] || fail "refusing to overwrite reconnect GC evidence"

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    rm -f "$DOCKLATTICE_RECONNECT_MARKER"
    exit "$status"
}
trap cleanup EXIT HUP INT TERM
: >"$DOCKLATTICE_RECONNECT_MARKER"

pid=$(docker inspect --format '{{.State.Pid}}' "$DOCKLATTICE_AGENT_CONTAINER")
case "$pid" in ''|*[!0-9]*) fail "cannot resolve running Agent PID" ;; esac
[ "$pid" -gt 0 ] || fail "Agent is not running"
cgroup=$(awk -F: '$1 == "0" { print $3; exit }' "/proc/$pid/cgroup")
[ -n "$cgroup" ] || fail "cannot resolve Agent cgroup"
events="/sys/fs/cgroup$cgroup/memory.events.local"
[ -r "$events" ] || fail "Agent cgroup memory events are unreadable"
awk '$1 == "oom" || $1 == "oom_kill" { print }' "$events" >"$DOCKLATTICE_RECONNECT_CGROUP_EVIDENCE"
[ "$(awk '$1 == "oom" { print $2 }' "$DOCKLATTICE_RECONNECT_CGROUP_EVIDENCE")" -eq 0 ] || fail "pre-reconnect Agent cgroup recorded OOM"
[ "$(awk '$1 == "oom_kill" { print $2 }' "$DOCKLATTICE_RECONNECT_CGROUP_EVIDENCE")" -eq 0 ] || fail "pre-reconnect Agent cgroup recorded OOM kill"
docker logs --tail 20000 "$DOCKLATTICE_AGENT_CONTAINER" 2>&1 |
    awk '/^gc [0-9]+ @/ { print }' >"$gctrace_evidence"
[ -s "$gctrace_evidence" ] || fail "pre-reconnect Agent produced no Go GC evidence"

docker rm -f "$DOCKLATTICE_AGENT_CONTAINER" >/dev/null
docker run -d --name "$DOCKLATTICE_AGENT_CONTAINER" --network "$DOCKLATTICE_AGENT_NETWORK" \
    --memory 512m --memory-swap 512m --cpus 1 --pids-limit 512 --ulimit nofile=4096:4096 \
    --log-driver local --log-opt max-size=5m --log-opt max-file=1 --log-opt compress=false \
    --group-add "$DOCKLATTICE_AGENT_SOCKET_GID" --label io.docklattice.role=agent -e GODEBUG=gctrace=1 \
    -v /var/run/docker.sock:/var/run/docker.sock:rw \
    -v "$DOCKLATTICE_AGENT_STATE_DIR:/var/lib/docklattice" \
    -v "$DOCKLATTICE_PROJECT_ROOT:$DOCKLATTICE_PROJECT_ROOT:rw" \
    "$DOCKLATTICE_AGENT_IMAGE" agent \
    --server server:8443 --registration-url https://server:8080 \
    --server-ca /var/lib/docklattice/server-ca.crt \
    --display-name "$DOCKLATTICE_AGENT_DISPLAY_NAME" --self-container-name "$DOCKLATTICE_AGENT_CONTAINER" \
    --project-root "$DOCKLATTICE_PROJECT_ROOT"
