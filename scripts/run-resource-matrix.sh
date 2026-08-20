#!/bin/sh
set -eu

usage() {
    printf 'usage: %s ABSOLUTE_ARTIFACT_DIR SERVER_IMAGE AGENT_IMAGE ABSOLUTE_WORKLOAD_DRIVER\n' "$0" >&2
    printf 'required environment: RESOURCE_FIXTURE_IMAGE (already present in the tested Docker Engine)\n' >&2
}

fail() {
    printf 'resource matrix failed: %s\n' "$*" >&2
    failure_reason=$*
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "preflight: required command not found: $1"
}

require_uint_range() {
    value=$1
    minimum=$2
    maximum=$3
    label=$4
    case "$value" in
        ''|*[!0-9]*) fail "preflight: $label must be an integer" ;;
    esac
    [ "$value" -ge "$minimum" ] && [ "$value" -le "$maximum" ] ||
        fail "preflight: $label must be between $minimum and $maximum"
}

[ "$#" -eq 4 ] || {
    usage
    exit 2
}

artifact_dir=$1
server_image=$2
agent_image=$3
workload_driver=$4
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
reconnect_helper="$script_dir/reconnect-product-resource-agent.sh"
fixture_image=${RESOURCE_FIXTURE_IMAGE:-}
case_seconds=${RESOURCE_CASE_SECONDS:-120}
sample_seconds=${RESOURCE_SAMPLE_SECONDS:-2}
artifact_max_bytes=${RESOURCE_ARTIFACT_MAX_BYTES:-536870912}

case "$artifact_dir" in
    /*) ;;
    *) fail "preflight: artifact directory must be absolute" ;;
esac
case "$artifact_dir" in
    *:*|*'
'*) fail "preflight: artifact directory cannot contain colon or newline" ;;
esac
case "$workload_driver" in
    /*) ;;
    *) fail "preflight: workload driver must be absolute" ;;
esac
[ ! -e "$artifact_dir" ] || fail "preflight: refusing to overwrite artifact directory: $artifact_dir"
[ -x "$workload_driver" ] && [ -f "$workload_driver" ] || fail "preflight: workload driver is not an executable regular file"
[ -x "$reconnect_helper" ] && [ -f "$reconnect_helper" ] || fail "preflight: Agent reconnect helper is unavailable"
[ -n "$fixture_image" ] || fail "preflight: RESOURCE_FIXTURE_IMAGE is required"
case "$fixture_image" in
    *[!A-Za-z0-9._/@:+-]*) fail "preflight: RESOURCE_FIXTURE_IMAGE contains unsafe characters" ;;
esac
require_uint_range "$case_seconds" 60 7200 RESOURCE_CASE_SECONDS
require_uint_range "$sample_seconds" 1 10 RESOURCE_SAMPLE_SECONDS
require_uint_range "$artifact_max_bytes" 67108864 2147483648 RESOURCE_ARTIFACT_MAX_BYTES

for command_name in docker openssl curl jq awk sed grep date du df sha256sum stat find sort wc setsid; do
    require_command "$command_name"
done
docker info >/dev/null 2>&1 || fail "preflight: Docker daemon is unavailable or permission is denied"
docker buildx version >/dev/null 2>&1 || fail "preflight: Docker buildx is unavailable"
[ "$(docker info --format '{{.OSType}}')" = linux ] || fail "preflight: a Linux Docker Engine is required"
[ "$(docker info --format '{{.CgroupVersion}}')" = 2 ] || fail "preflight: Docker must use cgroup v2"
[ -r /sys/fs/cgroup/cgroup.controllers ] || fail "preflight: host cgroup v2 filesystem is not readable"
[ -S /var/run/docker.sock ] || fail "preflight: v1 requires /var/run/docker.sock"
docker image inspect "$server_image" >/dev/null 2>&1 || fail "preflight: Server image is unavailable: $server_image"
docker image inspect "$agent_image" >/dev/null 2>&1 || fail "preflight: Agent image is unavailable: $agent_image"
docker image inspect "$fixture_image" >/dev/null 2>&1 || fail "preflight: fixture image is unavailable: $fixture_image"
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.licenses"}}' "$server_image")" = Apache-2.0 ] ||
    fail "preflight: Server image lacks the production license label"
[ "$(docker image inspect --format '{{index .Config.Labels "io.dockpilot.role"}}' "$agent_image")" = agent ] ||
    fail "preflight: Agent image lacks io.dockpilot.role=agent"
compose_label=$(docker image inspect --format '{{index .Config.Labels "io.dockpilot.compose.version"}}' "$agent_image")
[ -n "$compose_label" ] && [ "$compose_label" != '<no value>' ] ||
    fail "preflight: Agent image lacks its bundled Compose version label"

artifact_parent=$(dirname -- "$artifact_dir")
[ -d "$artifact_parent" ] || fail "preflight: artifact parent does not exist: $artifact_parent"
available_kib=$(df -Pk "$artifact_parent" | awk 'NR == 2 { print $4 }')
required_kib=$((artifact_max_bytes / 1024 + 1048576))
[ "$available_kib" -ge "$required_kib" ] ||
    fail "preflight: artifact filesystem needs the cap plus 1 GiB free"

# Lowercased because this prefix also derives the Compose project name, and
# Compose normalizes project names to lowercase before writing its
# com.docker.compose.project label. An uppercase prefix would make every label
# filter in this harness and in the workload driver miss.
prefix="dockpilot-resource-$(date -u +%Y%m%dT%H%M%SZ | tr '[:upper:]' '[:lower:]')-$$"
current_server=
current_agent=
current_network=
current_runtime=
current_evidence=
current_compose_project=
artifact_created=0
failure_reason="matrix did not complete"

stop_container() {
    name=$1
    [ -n "$name" ] || return 0
    docker rm -f "$name" >/dev/null 2>&1 || true
}

cleanup_compose_project() {
    project=$1
    [ -n "$project" ] || return 0
    container_ids=$(docker ps -aq --filter "label=com.docker.compose.project=$project" 2>/dev/null || true)
    if [ -n "$container_ids" ]; then
        # Docker object IDs contain no whitespace; this deliberate expansion
        # is bounded to the uniquely named harness project.
        # shellcheck disable=SC2086
        docker rm -f $container_ids >/dev/null 2>&1 || true
    fi
    network_ids=$(docker network ls -q --filter "label=com.docker.compose.project=$project" 2>/dev/null || true)
    if [ -n "$network_ids" ]; then
        # shellcheck disable=SC2086
        docker network rm $network_ids >/dev/null 2>&1 || true
    fi
}

scrub_runtime() {
    runtime=$1
    [ -n "$runtime" ] && [ -d "$runtime" ] || return 0
    case "$runtime" in
        "$artifact_dir"/trial-*/runtime) ;;
        *) printf 'refusing to scrub unexpected runtime path: %s\n' "$runtime" >&2; return 1 ;;
    esac
    docker run --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/dockpilot-resource-runtime" "$server_image" \
        -c 'rm -rf /dockpilot-resource-runtime/server /dockpilot-resource-runtime/agent /dockpilot-resource-runtime/bootstrap /dockpilot-resource-runtime/projects' \
        >/dev/null 2>&1
}

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    cleanup_compose_project "$current_compose_project"
    if [ "$artifact_created" -eq 1 ] && [ -n "$current_evidence" ] && [ -d "$current_evidence" ]; then
        if [ -n "$current_agent" ]; then
            docker logs --tail 2000 "$current_agent" >"$current_evidence/agent.failure.log" 2>&1 || true
        fi
        if [ -n "$current_server" ]; then
            docker logs --tail 2000 "$current_server" >"$current_evidence/server.failure.log" 2>&1 || true
        fi
    fi
    stop_container "$current_agent"
    stop_container "$current_server"
    if [ -n "$current_network" ]; then
        docker network rm "$current_network" >/dev/null 2>&1 || true
    fi
    scrub_runtime "$current_runtime" || true
    if [ "$artifact_created" -eq 1 ] && [ ! -e "$artifact_dir/STATUS" ]; then
        {
            printf 'status=FAIL\n'
            printf 'reason=%s\n' "$failure_reason" | tr '\r\n' '  '
            printf '\n'
        } >"$artifact_dir/STATUS"
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'failure_reason="matrix interrupted by signal"; exit 130' HUP INT TERM

# Prove that this daemon can create a constrained cgroup whose required files
# are visible from the host. Docker Desktop/remote-daemon setups fail here
# instead of producing host metrics for the wrong machine.
probe_name="$prefix-preflight"
docker run -d --name "$probe_name" --memory 32m --memory-swap 32m \
    --entrypoint /bin/sh "$server_image" -c 'sleep 30' >/dev/null ||
    fail "preflight: cannot create a memory-limited container"
probe_pid=$(docker inspect --format '{{.State.Pid}}' "$probe_name")
probe_cgroup=$(awk -F: '$1 == "0" { print $3; exit }' "/proc/$probe_pid/cgroup" 2>/dev/null || true)
[ -n "$probe_cgroup" ] || {
    stop_container "$probe_name"
    fail "preflight: cannot resolve container cgroup from host /proc"
}
probe_root="/sys/fs/cgroup$probe_cgroup"
for file in memory.current memory.peak memory.max memory.events.local memory.stat memory.pressure; do
    [ -r "$probe_root/$file" ] || {
        stop_container "$probe_name"
        fail "preflight: required cgroup file is unavailable: $file"
    }
done
[ "$(cat "$probe_root/memory.max")" = 33554432 ] || {
    stop_container "$probe_name"
    fail "preflight: Docker did not enforce the requested memory.max"
}
stop_container "$probe_name"

umask 077
mkdir "$artifact_dir"
artifact_created=1
{
    printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'source_revision=%s\n' "$(git -C "$(dirname -- "$0")/.." rev-parse HEAD 2>/dev/null || printf unknown)"
    printf 'kernel=%s\n' "$(uname -srvm)"
    printf 'docker_server_version=%s\n' "$(docker info --format '{{.ServerVersion}}')"
    printf 'docker_cgroup_driver=%s\n' "$(docker info --format '{{.CgroupDriver}}')"
    printf 'docker_cgroup_version=2\n'
    printf 'server_image=%s\n' "$server_image"
    printf 'server_image_id=%s\n' "$(docker image inspect --format '{{.Id}}' "$server_image")"
    printf 'agent_image=%s\n' "$agent_image"
    printf 'agent_image_id=%s\n' "$(docker image inspect --format '{{.Id}}' "$agent_image")"
    printf 'fixture_image=%s\n' "$fixture_image"
    printf 'fixture_image_id=%s\n' "$(docker image inspect --format '{{.Id}}' "$fixture_image")"
    printf 'agent_memory_max=536870912\n'
    printf 'server_memory_max=1073741824\n'
    printf 'repetitions=3\n'
    printf 'case_seconds=%s\n' "$case_seconds"
    printf 'sample_seconds=%s\n' "$sample_seconds"
    printf 'artifact_max_bytes=%s\n' "$artifact_max_bytes"
    printf 'prototype_acceptance_reused=false\n'
} >"$artifact_dir/environment.env"

check_artifact_cap() {
    used_kib=$(du -sk "$artifact_dir" | awk '{ print $1 }')
    [ $((used_kib * 1024)) -le "$artifact_max_bytes" ] || fail "artifact size cap exceeded"
}

cgroup_path() {
    container=$1
    pid=$(docker inspect --format '{{.State.Pid}}' "$container")
    [ "$pid" -gt 0 ] || return 1
    relative=$(awk -F: '$1 == "0" { print $3; exit }' "/proc/$pid/cgroup")
    [ -n "$relative" ] || return 1
    printf '/sys/fs/cgroup%s\n' "$relative"
}

metric_value() {
    key=$1
    file=$2
    awk -v key="$key" '$1 == key { print $2; found=1; exit } END { if (!found) exit 1 }' "$file"
}

pressure_value() {
    class=$1
    field=$2
    file=$3
    awk -v class="$class" -v field="$field" '$1 == class { for (i=2; i<=NF; i++) { split($i, pair, "="); if (pair[1] == field) { print pair[2]; found=1; exit } } } END { if (!found) exit 1 }' "$file"
}

write_raw_cgroup() {
    role=$1
    phase=$2
    root=$3
    out=$4
    for file in memory.current memory.peak memory.max memory.events.local memory.stat memory.pressure; do
        [ -r "$root/$file" ] || fail "$role cgroup lost required file $file"
        sed "s/^/$file /" "$root/$file" >>"$out/$role.$phase.cgroup.txt"
    done
}

sample_role() {
    role=$1
    container=$2
    output=$3
    root=$(cgroup_path "$container") || return 2
    pid=$(docker inspect --format '{{.State.Pid}}' "$container")
    [ "$pid" -gt 0 ] && [ -r "/proc/$pid/status" ] || return 2
    rss_kib=$(awk '$1 == "VmRSS:" { print $2; found=1; exit } END { if (!found) exit 1 }' "/proc/$pid/status")
    # /proc/<host-pid>/fd is commonly protected from an unprivileged harness
    # user. Read PID 1's directory as the container user instead of silently
    # recording zero or requiring root on the host.
    fd_count=$(docker exec "$container" /bin/sh -c 'ls -1 /proc/1/fd 2>/dev/null | wc -l')
    [ "$fd_count" -gt 0 ] || fail "$role file descriptors are not readable"
    current=$(cat "$root/memory.current")
    peak=$(cat "$root/memory.peak")
    maximum=$(cat "$root/memory.max")
    event_max=$(metric_value max "$root/memory.events.local")
    oom=$(metric_value oom "$root/memory.events.local")
    oom_kill=$(metric_value oom_kill "$root/memory.events.local")
    anon=$(metric_value anon "$root/memory.stat")
    file=$(metric_value file "$root/memory.stat")
    inactive_file=$(metric_value inactive_file "$root/memory.stat")
    sock=$(metric_value sock "$root/memory.stat")
    kernel=$(metric_value kernel "$root/memory.stat")
    slab_unreclaimable=$(metric_value slab_unreclaimable "$root/memory.stat")
    dirty=$(metric_value file_dirty "$root/memory.stat")
    writeback=$(metric_value file_writeback "$root/memory.stat")
    some_avg10=$(pressure_value some avg10 "$root/memory.pressure")
    some_total=$(pressure_value some total "$root/memory.pressure")
    full_avg10=$(pressure_value full avg10 "$root/memory.pressure")
    full_total=$(pressure_value full total "$root/memory.pressure")
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "$role" "$pid" "$rss_kib" "$fd_count" \
        "$current" "$peak" "$maximum" "$event_max" "$oom" "$oom_kill" "$anon" "$file" "$inactive_file" \
        "$sock" "$kernel" "$slab_unreclaimable" "$dirty" "$writeback" "$some_avg10/$some_total" "$full_avg10/$full_total" >>"$output"
}

wait_https() {
    url=$1
    certificate=$2
    deadline=$(( $(date +%s) + 60 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl --fail --silent --show-error --max-time 3 --cacert "$certificate" "$url" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

require_verdict() {
    file=$1
    key=$2
    count=$(awk -F= -v key="$key" '$1 == key && $2 == "1" { count++ } END { print count+0 }' "$file")
    [ "$count" -eq 1 ] || fail "workload verdict does not prove $key=1 exactly once"
}

require_workload_evidence() {
    evidence_root=$1
    for file in compose-child-processes.tsv p0-p1-latency.jsonl audit-cursor-progress.jsonl bounded-buffers.jsonl io-evidence.tsv resource-trend.json; do
        [ -s "$evidence_root/$file" ] || fail "workload evidence is missing or empty: $file"
    done
    for file in p0-p1-latency.jsonl audit-cursor-progress.jsonl bounded-buffers.jsonl; do
        jq -e -s 'length > 0' "$evidence_root/$file" >/dev/null || fail "workload evidence is not valid JSONL: $file"
    done
    jq -e '.pass == true' "$evidence_root/resource-trend.json" >/dev/null || fail "resource-trend.json does not pass"
    [ "$(awk 'END { print NR+0 }' "$evidence_root/compose-child-processes.tsv")" -ge 2 ] ||
        fail "compose child evidence has no observation rows"
    [ "$(awk 'END { print NR+0 }' "$evidence_root/io-evidence.tsv")" -ge 2 ] ||
        fail "WAL/backup/discovery I/O evidence has no observation rows"
}

summarize_resources() {
    samples_file=$1
    output=$2
    : >"$output"
    for role in server agent; do
        case "$role" in
            server) limit=1073741824 ;;
            agent) limit=536870912 ;;
        esac
        awk -F '\t' -v role="$role" -v limit="$limit" '
            NR > 1 && $2 == role {
                count++
                if ($4+0 > max_rss_kib) max_rss_kib=$4+0
                if ($5+0 > max_fd) max_fd=$5+0
                if ($6+0 > max_current) max_current=$6+0
                if ($7+0 > max_peak) max_peak=$7+0
                if ($9+0 > max_events) max_events=$9+0
                if ($12+0 > max_anon) max_anon=$12+0
            }
            END {
                if (count == 0) exit 1
                warning=(max_peak * 100 >= limit * 80 ? "true" : "false")
                printf "role=%s samples=%d max_rss_kib=%d max_fd=%d max_memory_current=%d max_memory_peak=%d memory_max=%d peak_over_80_percent=%s max_events_local_max=%d max_anon=%d\n", role, count, max_rss_kib, max_fd, max_current, max_peak, limit, warning, max_events, max_anon
            }
        ' "$samples_file" >>"$output" || fail "cannot summarize $role resource samples"
    done
}

capture_go_evidence() {
    role=$1
    container=$2
    output=$3
    # Capture GC lines before SIGQUIT so a large stack dump cannot rotate them
    # out of Docker's bounded local log.
    docker logs --tail 20000 "$container" 2>&1 |
        awk '/^gc [0-9]+ @/ { print }' >"$output/$role.gctrace.log"
    [ -s "$output/$role.gctrace.log" ] || fail "$role produced no Go GC heap evidence"
    docker kill --signal=QUIT "$container" >/dev/null || fail "could not request $role Go goroutine dump"
    deadline=$(( $(date +%s) + 15 ))
    while [ "$(docker inspect --format '{{.State.Running}}' "$container")" = true ] && [ "$(date +%s)" -lt "$deadline" ]; do
        sleep 1
    done
    [ "$(docker inspect --format '{{.State.Running}}' "$container")" = false ] || fail "$role did not stop after diagnostic SIGQUIT"
    docker logs --tail 20000 "$container" 2>&1 |
        awk -v summary="$output/$role.goroutines.txt" '
            /^goroutine [0-9]+ / { count++; if (count <= 2000) print }
            END { print "goroutine_stack_headers=" count+0 > summary }
        ' >"$output/$role.goroutine-headers.txt"
    goroutines=$(awk -F= '$1 == "goroutine_stack_headers" { print $2 }' "$output/$role.goroutines.txt")
    [ "$goroutines" -gt 0 ] || fail "$role produced no Go goroutine evidence"
}

sustained_increase_trend() {
    role=$1
    field=$2
    samples_file=$3
    # architecture.md:1774 states the failure condition as "anon Memory 지속 증가"
    # -- a *sustained* increase -- and architecture.md:1755 requires that it be
    # judged from the trend across the whole observation window rather than from
    # individual samples. Appendix A operationalises exactly this shape of
    # criterion for audit_sync_lag_records as "기울기가 지속 양수가 아닐 것"
    # (architecture.md:2198), so the same construct is applied here: split the
    # window into quarters and treat growth as sustained only when every quarter
    # slopes upward. The 120% materiality bound is Appendix A item 8's own
    # tolerance, applied to segment levels so that a GC sawtooth cannot decide
    # the verdict by which side of a collection a three-sample window lands on.
    awk -F '\t' -v role="$role" -v field="$field" '
        NR > 1 && $2 == role { n++; value[n]=$field+0 }
        END {
            if (n < 12) exit 1
            segs=4
            positive=0
            for (s=0; s<segs; s++) {
                lo=int(n*s/segs)+1
                hi=int(n*(s+1)/segs)
                m=0; sx=0; sy=0; num=0; den=0
                for (i=lo; i<=hi; i++) { m++; px[m]=m; py[m]=value[i]; sx+=m; sy+=value[i] }
                mx=sx/m; my=sy/m
                for (i=1; i<=m; i++) { num+=(px[i]-mx)*(py[i]-my); den+=(px[i]-mx)^2 }
                slope=(den == 0 ? 0 : num/den)
                mean[s]=my
                if (slope > 0) positive++
            }
            floor=mean[0]
            for (s=1; s<segs-1; s++) if (mean[s] < floor) floor=mean[s]
            last=mean[segs-1]
            material=(last*100 > floor*120)
            pass=((positive == segs && material) ? 0 : 1)
            printf "%d %d %d %d %d\n", n, int(floor), int(last), positive, pass
        }
    ' "$samples_file"
}

post_gc_heap_trend() {
    role=$1
    evidence_root=$2
    if [ "$role" = agent ] && [ -s "$evidence_root/agent.pre-reconnect.gctrace.log" ]; then
        files="$evidence_root/agent.pre-reconnect.gctrace.log $evidence_root/agent.gctrace.log"
    else
        files="$evidence_root/$role.gctrace.log"
    fi
    # gctrace's third value in X->Y->Z MB is live heap after that GC.
    # shellcheck disable=SC2086
    sed -n 's/.* [0-9][0-9]*->[0-9][0-9]*->\([0-9][0-9]*\) MB.*/\1/p' $files |
        awk '
            { n++; heap[n]=$1+0 }
            END {
                if (n < 6) exit 1
                for (i=1; i<=3; i++) first += heap[i]
                for (i=n-2; i<=n; i++) last += heap[i]
                first=int(first/3); last=int(last/3)
                pass=(last*100 <= (first == 0 ? 1 : first)*120)
                printf "%d %d %d %d\n", n, first, last, pass
            }
        '
}

evaluate_post_workload_trends() {
    evidence_root=$1
    samples_file=$2
    agent_anon=$(sustained_increase_trend agent 12 "$samples_file") || fail "insufficient Agent anon samples"
    set -- $agent_anon
    agent_anon_count=$1 agent_anon_floor=$2 agent_anon_last=$3 agent_anon_rising=$4 agent_anon_pass=$5
    server_anon=$(sustained_increase_trend server 12 "$samples_file") || fail "insufficient Server anon samples"
    set -- $server_anon
    server_anon_count=$1 server_anon_floor=$2 server_anon_last=$3 server_anon_rising=$4 server_anon_pass=$5
    agent_heap=$(post_gc_heap_trend agent "$evidence_root") || fail "insufficient Agent post-GC heap samples"
    set -- $agent_heap
    agent_heap_count=$1 agent_heap_first=$2 agent_heap_last=$3 agent_heap_pass=$4
    server_heap=$(post_gc_heap_trend server "$evidence_root") || fail "insufficient Server post-GC heap samples"
    set -- $server_heap
    server_heap_count=$1 server_heap_first=$2 server_heap_last=$3 server_heap_pass=$4
    [ "$agent_anon_pass" -eq 1 ] && [ "$server_anon_pass" -eq 1 ] || fail "anonymous memory increased persistently across the observation window"
    [ "$agent_heap_pass" -eq 1 ] && [ "$server_heap_pass" -eq 1 ] || fail "post-GC heap did not recover within 120 percent"
    jq -e -s 'length > 0 and all(.[];
        .configured_max_buffer_bytes > 0 and .configured_max_buffer_chunks > 0 and
        .dropped_bytes > 0 and .post_stop_recovered == true)' \
        "$evidence_root/bounded-buffers.jsonl" >/dev/null || fail "bounded stream buffer recovery evidence is missing"
    awk -F '\t' 'NR > 1 && ($10+0 > 0 || $11+0 > 0) { bad=1 } END { exit bad ? 0 : 1 }' "$samples_file" >/dev/null 2>&1 &&
        fail "sampled cgroup OOM counter increased"
    jq -n \
        --argjson aac "$agent_anon_count" --argjson aaf "$agent_anon_floor" --argjson aal "$agent_anon_last" --argjson aar "$agent_anon_rising" \
        --argjson sac "$server_anon_count" --argjson saf "$server_anon_floor" --argjson sal "$server_anon_last" --argjson sar "$server_anon_rising" \
        --argjson ahc "$agent_heap_count" --argjson ahf "$agent_heap_first" --argjson ahl "$agent_heap_last" \
        --argjson shc "$server_heap_count" --argjson shf "$server_heap_first" --argjson shl "$server_heap_last" \
        '{pass:true,
          anon_criterion:"architecture 1755/1774: fails only when all four window quarters slope upward and the final quarter mean exceeds 120% of the lowest earlier quarter mean",
          heap_criterion:"last-three average <= 120% of first-three",
          anon_bytes:{agent:{samples:$aac,quarter_floor:$aaf,final_quarter:$aal,rising_quarters:$aar,quarters:4},server:{samples:$sac,quarter_floor:$saf,final_quarter:$sal,rising_quarters:$sar,quarters:4}},
          post_gc_heap_mib:{agent:{samples:$ahc,first:$ahf,last:$ahl},server:{samples:$shc,first:$shf,last:$shl}},
          bounded_buffers:{drop_accounting:true,post_stop_recovered:true}}' >"$evidence_root/post-workload-trend.json"
}

for trial in 1 2 3; do
    trial_dir="$artifact_dir/trial-$trial"
    runtime="$trial_dir/runtime"
    evidence="$trial_dir/evidence"
    mkdir -p "$runtime/server/tls" "$runtime/agent" "$runtime/bootstrap" "$runtime/projects" "$evidence"
    current_runtime=$runtime
    current_evidence=$evidence
    network="$prefix-trial-$trial"
    server="$prefix-server-$trial"
    agent="$prefix-agent-$trial"
    fixture_project="$prefix-fixture-$trial"
    current_network=$network
    current_server=$server
    current_agent=$agent
    current_compose_project=$fixture_project

    openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
        -subj '/CN=server' -addext 'subjectAltName=DNS:server,IP:127.0.0.1' \
        -keyout "$runtime/server/tls/server.key" -out "$runtime/server/tls/server.crt" \
        >"$evidence/openssl.stdout" 2>"$evidence/openssl.stderr"
    cp "$runtime/server/tls/server.crt" "$runtime/bootstrap/server-ca.crt"
    printf 'name: %s\nservices:\n  resource-fixture:\n    image: %s\n    command: ["/bin/sh", "-c", "while :; do printf x; sleep 1; done"]\n' \
        "$fixture_project" "$fixture_image" >"$runtime/projects/compose.yaml"
    docker run --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime:/resource" "$server_image" -c \
        'chown -R 65532:65532 /resource/server /resource/projects; chmod 0700 /resource/server; chmod 0600 /resource/server/tls/server.crt /resource/server/tls/server.key; chmod 0755 /resource/projects; chmod 0644 /resource/projects/compose.yaml' \
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

    docker network create --subnet "$(harness_subnet)" "$network" >"$evidence/network.id"
    docker run -d --name "$server" --network "$network" --network-alias server \
        --memory 1g --memory-swap 1g --cpus 1 --pids-limit 512 --ulimit nofile=4096:4096 \
        --log-driver local --log-opt max-size=5m --log-opt max-file=1 --log-opt compress=false \
        -e GODEBUG=gctrace=1 -p 127.0.0.1::8080 \
        -v "$runtime/server:/var/lib/dockpilot" "$server_image" \
        server --listen 0.0.0.0:8080 --agent-listen 0.0.0.0:8443 --allow-public-bind \
        >"$evidence/server.container-id"
    server_port=$(docker port "$server" 8080/tcp | awk -F: 'NR == 1 { print $NF }')
    case "$server_port" in ''|*[!0-9]*) fail "could not resolve Server host port" ;; esac
    wait_https "https://127.0.0.1:$server_port/api/v1/dashboard" "$runtime/bootstrap/server-ca.crt" ||
        fail "Server did not become ready"

    docker run --rm --user 65532:65532 \
        -v "$runtime/server:/var/lib/dockpilot" "$server_image" \
        server issue-token --state-dir /var/lib/dockpilot --ttl 15m \
        >"$runtime/bootstrap/join-token" 2>"$evidence/issue-token.stderr"
    cp "$runtime/bootstrap/server-ca.crt" "$runtime/agent/server-ca.crt"
    cp "$runtime/bootstrap/join-token" "$runtime/agent/join-token"
    docker run --rm --user 0:0 --entrypoint /bin/sh \
        -v "$runtime/agent:/agent" "$server_image" -c \
        'chown -R 65532:65532 /agent; chmod 0700 /agent; chmod 0600 /agent/server-ca.crt /agent/join-token' >/dev/null

    socket_gid=$(stat -c '%g' /var/run/docker.sock)
    docker run -d --name "$agent" --network "$network" \
        --memory 512m --memory-swap 512m --cpus 1 --pids-limit 512 --ulimit nofile=4096:4096 \
        --log-driver local --log-opt max-size=5m --log-opt max-file=1 --log-opt compress=false \
        --group-add "$socket_gid" --label io.dockpilot.role=agent -e GODEBUG=gctrace=1 \
        -v /var/run/docker.sock:/var/run/docker.sock:rw \
        -v "$runtime/agent:/var/lib/dockpilot" \
        -v "$runtime/projects:$runtime/projects:rw" "$agent_image" agent \
        --server server:8443 --registration-url https://server:8080 \
        --server-ca /var/lib/dockpilot/server-ca.crt --join-token-file /var/lib/dockpilot/join-token \
        --display-name "resource-agent-$trial" --self-container-name "$agent" \
        --project-root "$runtime/projects" >"$evidence/agent.container-id"

    dashboard="$evidence/dashboard-ready.json"
    ready_deadline=$(( $(date +%s) + 90 ))
    ready=0
    while [ "$(date +%s)" -lt "$ready_deadline" ]; do
        if curl --fail --silent --show-error --max-time 3 --cacert "$runtime/bootstrap/server-ca.crt" \
            "https://127.0.0.1:$server_port/api/v1/dashboard" >"$dashboard.tmp" 2>/dev/null &&
            jq -e 'any(.hosts[]; .state == "ACTIVE" and .capabilities.connection.enabled == true and .capabilities.docker.enabled == true and .capabilities.compose.enabled == true)' "$dashboard.tmp" >/dev/null 2>&1; then
            mv "$dashboard.tmp" "$dashboard"
            ready=1
            break
        fi
        sleep 1
    done
    [ "$ready" -eq 1 ] || fail "Agent/project did not become ready"
    docker run --rm --user 0:0 --entrypoint /bin/sh -v "$runtime/agent:/agent" "$server_image" \
        -c 'rm -f /agent/join-token' >/dev/null
    rm -f "$runtime/bootstrap/join-token"

    server_cgroup=$(cgroup_path "$server") || fail "cannot resolve Server cgroup"
    agent_cgroup=$(cgroup_path "$agent") || fail "cannot resolve Agent cgroup"
    [ "$(cat "$server_cgroup/memory.max")" = 1073741824 ] || fail "Server memory.max is not 1 GiB"
    [ "$(cat "$agent_cgroup/memory.max")" = 536870912 ] || fail "Agent memory.max is not 512 MiB"
    write_raw_cgroup server start "$server_cgroup" "$evidence"
    write_raw_cgroup agent start "$agent_cgroup" "$evidence"

    samples="$evidence/resource-samples.tsv"
    printf 'at\trole\tpid\trss_kib\tfd_count\tmemory_current\tmemory_peak\tmemory_max\tevents_max\toom\toom_kill\tanon\tfile\tinactive_file\tsock\tkernel\tslab_unreclaimable\tfile_dirty\tfile_writeback\tpressure_some_avg10_total\tpressure_full_avg10_total\n' >"$samples"
    verdict="$evidence/workload-verdict.env"
    DOCKPILOT_BASE_URL="https://127.0.0.1:$server_port" \
    DOCKPILOT_CA_FILE="$runtime/bootstrap/server-ca.crt" \
    DOCKPILOT_DASHBOARD_FILE="$dashboard" \
    DOCKPILOT_PROJECT_ROOT="$runtime/projects" \
    DOCKPILOT_COMPOSE_PROJECT="$fixture_project" \
    DOCKPILOT_SERVER_CONTAINER="$server" \
    DOCKPILOT_AGENT_CONTAINER="$agent" \
    DOCKPILOT_AUDIT_URL="https://127.0.0.1:$server_port/api/v1/hosts" \
    DOCKPILOT_AGENT_RECONNECT_HELPER="$reconnect_helper" \
    DOCKPILOT_AGENT_IMAGE="$agent_image" \
    DOCKPILOT_AGENT_NETWORK="$network" \
    DOCKPILOT_AGENT_STATE_DIR="$runtime/agent" \
    DOCKPILOT_AGENT_SOCKET_GID="$socket_gid" \
    DOCKPILOT_AGENT_DISPLAY_NAME="resource-agent-$trial" \
    DOCKPILOT_RECONNECT_MARKER="$evidence/agent-reconnect.active" \
    DOCKPILOT_RECONNECT_CGROUP_EVIDENCE="$evidence/agent.pre-reconnect.memory-events.txt" \
    DOCKPILOT_RECONNECT_GCTRACE_EVIDENCE="$evidence/agent.pre-reconnect.gctrace.log" \
    RESOURCE_FIXTURE_IMAGE="$fixture_image" \
    RESOURCE_VERDICT_FILE="$verdict" \
    RESOURCE_CASE_SECONDS="$case_seconds" \
        setsid "$workload_driver" "$trial" "$evidence" >"$evidence/workload.stdout" 2>"$evidence/workload.stderr" &
    workload_pid=$!
    deadline=$(( $(date +%s) + case_seconds ))
    timed_out=0
    while kill -0 "$workload_pid" 2>/dev/null; do
        sample_role server "$server" "$samples"
        if [ ! -e "$evidence/agent-reconnect.active" ]; then
            if ! sample_role agent "$agent" "$samples"; then
                [ -e "$evidence/agent-reconnect.active" ] || fail "Agent process exited during measurement"
            fi
        fi
        check_artifact_cap
        if [ "$(date +%s)" -ge "$deadline" ]; then
            timed_out=1
            kill -TERM "-$workload_pid" 2>/dev/null || true
            terminate_deadline=$(( $(date +%s) + 10 ))
            while kill -0 "$workload_pid" 2>/dev/null && [ "$(date +%s)" -lt "$terminate_deadline" ]; do
                sleep 1
            done
            kill -KILL "-$workload_pid" 2>/dev/null || true
            break
        fi
        sleep "$sample_seconds"
    done
    if wait "$workload_pid"; then
        workload_status=0
    else
        workload_status=$?
    fi
    # A successful driver must not leave detached helpers running on the host.
    # setsid gives every trial its own process group, so this is narrowly scoped.
    kill -TERM "-$workload_pid" 2>/dev/null || true
    [ "$timed_out" -eq 0 ] || fail "workload exceeded RESOURCE_CASE_SECONDS"
    [ "$workload_status" -eq 0 ] || fail "workload driver exited $workload_status"
    [ -f "$verdict" ] && [ -s "$verdict" ] || fail "workload driver produced no verdict"
    sample_count=$(awk 'NR > 1 && $2 == "agent" { count++ } END { print count+0 }' "$samples")
    [ "$sample_count" -ge 10 ] || fail "fewer than 10 resource samples were collected"
    for key in PRODUCT_SERVER_AGENT REAL_COMPOSE_CHILD REAL_WAL_FSYNC BACKUP_SNAPSHOT_IO DISCOVERY_SCAN APPENDIX_A_MIX P0_P1_PASS AUDIT_CONTINUITY_PASS BOUNDED_BUFFERS_PASS RESOURCE_TREND_PASS; do
        require_verdict "$verdict" "$key"
    done
    require_workload_evidence "$evidence"

    sample_role server "$server" "$samples"
    sample_role agent "$agent" "$samples"
    server_cgroup=$(cgroup_path "$server") || fail "cannot resolve final Server cgroup"
    agent_cgroup=$(cgroup_path "$agent") || fail "cannot resolve final Agent cgroup"
    write_raw_cgroup server end "$server_cgroup" "$evidence"
    write_raw_cgroup agent end "$agent_cgroup" "$evidence"
    for role in server agent; do
        start_file="$evidence/$role.start.cgroup.txt"
        end_file="$evidence/$role.end.cgroup.txt"
        start_oom=$(awk '$1 == "memory.events.local" && $2 == "oom" { print $3 }' "$start_file")
        end_oom=$(awk '$1 == "memory.events.local" && $2 == "oom" { print $3 }' "$end_file")
        start_kill=$(awk '$1 == "memory.events.local" && $2 == "oom_kill" { print $3 }' "$start_file")
        end_kill=$(awk '$1 == "memory.events.local" && $2 == "oom_kill" { print $3 }' "$end_file")
        [ "$end_oom" -eq "$start_oom" ] || fail "$role memory.events.local.oom increased"
        [ "$end_kill" -eq "$start_kill" ] || fail "$role memory.events.local.oom_kill increased"
    done
    summarize_resources "$samples" "$evidence/resource-summary.txt"

    capture_go_evidence agent "$agent" "$evidence"
    capture_go_evidence server "$server" "$evidence"
    evaluate_post_workload_trends "$evidence" "$samples"
    [ "$(docker inspect --format '{{.State.OOMKilled}}' "$agent")" = false ] || fail "Agent was OOM-killed"
    [ "$(docker inspect --format '{{.State.OOMKilled}}' "$server")" = false ] || fail "Server was OOM-killed"
    docker inspect "$agent" >"$evidence/agent.inspect.json"
    docker inspect "$server" >"$evidence/server.inspect.json"
    du -ak "$runtime" | sort -n >"$evidence/runtime-sizes-kib.txt"
    check_artifact_cap

    stop_container "$agent"
    stop_container "$server"
    cleanup_compose_project "$fixture_project"
    current_compose_project=
    current_agent=
    current_server=
    docker network rm "$network" >/dev/null
    current_network=
    scrub_runtime "$runtime"
    current_runtime=
    current_evidence=
    printf 'status=PASS\ntrial=%s\n' "$trial" >"$trial_dir/STATUS"
    check_artifact_cap
done

{
    printf 'completed_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'repetitions=3\n'
    printf 'prototype_acceptance_reused=false\n'
} >"$artifact_dir/completion.env"
(
    cd "$artifact_dir"
    find . -type f ! -name SHA256SUMS -exec sha256sum '{}' \; | sort >SHA256SUMS
)
printf 'status=PASS\n' >"$artifact_dir/STATUS"
failure_reason=
trap - EXIT HUP INT TERM
printf 'production resource matrix completed: %s\n' "$artifact_dir"
