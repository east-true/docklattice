#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
driver="$repo_dir/scripts/run-product-resource-workload.sh"
runner="$repo_dir/scripts/run-resource-matrix.sh"
documentation="$repo_dir/docs/release/resource-gate.md"

[ -x "$driver" ] || {
    printf 'product resource workload verification failed: driver is not executable\n' >&2
    exit 1
}
sh -n "$driver"
sh -n "$runner"

require_literal() {
    literal=$1
    file=$2
    grep -F -- "$literal" "$file" >/dev/null || {
        printf 'product resource workload verification failed: %s lacks %s\n' "$file" "$literal" >&2
        exit 1
    }
}

for literal in \
    'RESOURCE_CASE_SECONDS must be at least 120' \
    'capabilities.compose.enabled == true' \
    'compose-child-processes.tsv' \
    'p0-p1-latency.jsonl' \
    'audit-cursor-progress.jsonl' \
    'bounded-buffers.jsonl' \
    'io-evidence.tsv' \
    'resource-trend.json' \
    'docker top "$DOCKLATTICE_AGENT_CONTAINER"' \
    'DOCKLATTICE_AUDIT_URL' \
    '/$agent_id/audit?limit=500' \
    'configured_max_buffer_bytes:1048576' \
    'slow_dropped_bytes' \
    'kind:"compose.up"' \
    'trigger == "pre_restore"' \
    'kind:"discovery.rescan"' \
    'DOCKLATTICE_AGENT_RECONNECT_HELPER' \
    'P0 p99 latency exceeds 500ms' \
    'Audit ACK lag exceeds 20 records' \
    'last-three average RSS <= 120% of first-three after the churn baseline' \
    'all four window quarters slope upward' \
    'anonymous memory increased persistently across the observation window' \
    'rm -f "$RESOURCE_VERDICT_FILE"' \
    'api/v1/live/matrix?agent_id=$agent_id' \
    'matrix-evidence.jsonl' \
    'capabilities.metrics.enabled' \
    'a Matrix-active trial is impossible' \
    'phase:"idle"' \
    'phase:"active"' \
    'grep -c ' \
    'this trial measured no Metrics activity' \
    'no Matrix frame carried a container row' \
    'Matrix capture exceeded its explicit bound' \
    'agent_dropped_frames' \
    'server_dropped_frames'; do
    require_literal "$literal" "$driver"
done

for key in PRODUCT_SERVER_AGENT REAL_COMPOSE_CHILD REAL_WAL_FSYNC BACKUP_SNAPSHOT_IO DISCOVERY_SCAN APPENDIX_A_MIX P0_P1_PASS AUDIT_CONTINUITY_PASS BOUNDED_BUFFERS_PASS RESOURCE_TREND_PASS MATRIX_IDLE_PASS MATRIX_ACTIVE_PASS; do
    require_literal "$key" "$driver"
done

require_literal 'RESOURCE_FIXTURE_IMAGE="$fixture_image"' "$runner"
require_literal 'Status: PASS' "$documentation"
require_literal 'run-product-resource-workload.sh' "$documentation"

for forbidden in 'docker build' 'docker push' 'docker pull' 'buildx build' 'curl http://' 'coverage-state.json' 'docker restart --time 10'; do
    if grep -F -- "$forbidden" "$driver" >/dev/null; then
        printf 'product resource workload verification failed: forbidden command/reference: %s\n' "$forbidden" >&2
        exit 1
    fi
done

if "$driver" >/dev/null 2>&1; then
    printf 'product resource workload verification failed: missing arguments returned success\n' >&2
    exit 1
else
    status=$?
    [ "$status" -eq 2 ] || {
        printf 'product resource workload verification failed: missing arguments returned %s, want 2\n' "$status" >&2
        exit 1
    }
fi

printf 'product resource workload syntax and static contracts are valid\n'
